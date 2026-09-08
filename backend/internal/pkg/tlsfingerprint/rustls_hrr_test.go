package tlsfingerprint

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"slices"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/stretchr/testify/require"
)

// 构造仅用于回环协议测试的 HRR；不涉及真实服务或授权数据。
func nativeTestHRR(session []byte, cookie bool) []byte {
	body := []byte{3, 3}
	body = append(body, rustlsHRRRandom[:]...)
	body = append(body, byte(len(session)))
	body = append(body, session...)
	body = append(body, 0x13, 0x01, 0)
	extensions := []byte{0, 43, 0, 2, 3, 4, 0, 51, 0, 2, 0, 23}
	if cookie {
		extensions = append(extensions, 0, 44, 0, 5, 0, 3, 't', 'l', 's')
	}
	body = binary.BigEndian.AppendUint16(body, uint16(len(extensions)))
	body = append(body, extensions...)
	handshake := []byte{2, 0, byte(len(body) >> 8), byte(len(body))}
	handshake = append(handshake, body...)
	record := []byte{22, 3, 3}
	record = binary.BigEndian.AppendUint16(record, uint16(len(handshake)))
	return append(record, handshake...)
}

// 逐字节、分 TLS 记录输入和普通 ServerHello 均由同一有界观察器处理。
func TestRustlsNativeOrderHRRObserver(t *testing.T) {
	for _, chunkSize := range []int{1, 3, 5, 13, 4096} {
		called := 0
		observer := rustlsHRRObserver{onCookie: func() { called++ }}
		record := nativeTestHRR(nil, true)
		for pos := 0; pos < len(record); pos += chunkSize {
			observer.feed(record[pos:min(pos+chunkSize, len(record))])
		}
		require.Equal(t, 1, called)
		observer.feed(record)
		require.Equal(t, 1, called, "观察只执行一次，不检查后续应用数据")
		require.Empty(t, observer.records)
		require.Empty(t, observer.handshake)
	}
	called := 0
	observer := rustlsHRRObserver{onCookie: func() { called++ }}
	record := nativeTestHRR(nil, true)
	// 将一个 ServerHello 拆成两个合法 TLS 记录。
	first, second := slices.Clone(record[5:25]), slices.Clone(record[25:])
	for _, part := range [][]byte{first, second} {
		frame := binary.BigEndian.AppendUint16([]byte{22, 3, 3}, uint16(len(part)))
		observer.feed(append(frame, part...))
	}
	require.Equal(t, 1, called)
	for _, input := range [][]byte{
		nativeTestHRR(nil, false),
		append([]byte{23, 3, 3, 0, 1}, 0),
		{22, 3, 3, 255, 255},
	} {
		observer := rustlsHRRObserver{onCookie: func() { t.Error("不应为此输入插入 Cookie") }}
		observer.feed(input)
	}
}

// 实际 uTLS 重试产生的第二个 ClientHello 必须正确回显 Cookie 并复用排序种子。
func TestRustlsNativeOrderCookieHRROnWire(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	type capture struct {
		record []byte
		err    error
	}
	result := make(chan capture, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- capture{err: err}
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(conn)
		readRecord := func() ([]byte, error) {
			header := make([]byte, 5)
			if _, err := io.ReadFull(reader, header); err != nil {
				return nil, err
			}
			data := make([]byte, int(binary.BigEndian.Uint16(header[3:])))
			_, err := io.ReadFull(reader, data)
			return append(header, data...), err
		}
		first, err := readRecord()
		if err != nil || len(first) < 44 || len(first) < 44+int(first[43]) {
			result <- capture{err: io.ErrUnexpectedEOF}
			return
		}
		_, err = conn.Write(nativeTestHRR(first[44:44+int(first[43])], true))
		if err != nil {
			result <- capture{err: err}
			return
		}
		for range 3 {
			record, err := readRecord()
			if err != nil || record[0] == 22 {
				result <- capture{record: record, err: err}
				return
			}
		}
		result <- capture{err: io.ErrUnexpectedEOF}
	}()
	raw, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	require.NoError(t, raw.SetDeadline(time.Now().Add(5*time.Second)))
	client, err := newTLSFingerprintClient(raw, nativeOrderTestProfile(), &utls.Config{ServerName: "example.com"})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	state, ok := client.NetConn().(*rustlsHRRConn)
	require.True(t, ok, "原生排序必须挂接 HRR 状态连接")
	require.Error(t, client.HandshakeContext(context.Background()), "采集端在第二个 ClientHello 后关闭，不伪装为完整握手成功")
	got := <-result
	require.NoError(t, got.err)
	hello, err := ParseCapturedClientHello(got.record)
	require.NoError(t, err)
	expected := rustlsExtensionOrder(append(slices.Clone(nativeOrderTestProfile().Extensions), 44), state.seed)
	require.Equal(t, expected, hello.Extensions)
	require.True(t, bytes.Contains(got.record, []byte{0, 44, 0, 5, 0, 3, 't', 'l', 's'}))
}
