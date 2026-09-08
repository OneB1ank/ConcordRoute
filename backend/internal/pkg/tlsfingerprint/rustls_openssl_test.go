package tlsfingerprint

import (
	"bytes"
	"context"
	"crypto/x509"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/stretchr/testify/require"
)

// 仅用于 OpenSSL CLI 测试夹具：3.5.5 的 stateless 分支在第二次读取前
// 重置状态，会拒绝兼容模式的 dummy CCS。过滤不参与 transcript 的该记录，
// 以独立验证 Cookie/ClientHello 和 Finished；生产路径不使用这个适配器。
type opensslStatelessFixtureConn struct{ net.Conn }

func (c *opensslStatelessFixtureConn) Write(p []byte) (int, error) {
	if bytes.Equal(p, []byte{20, 3, 3, 0, 1, 1}) {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

// OpenSSL 的 stateless 服务端真实生成并验证 HRR Cookie；
// 本地显式设置 TLS_TEST_OPENSSL 才运行，不下载程序或连接外网。
func TestRustlsNativeOrderOpenSSLCookieHandshake(t *testing.T) {
	openssl := os.Getenv("TLS_TEST_OPENSSL")
	if openssl == "" {
		t.Skip("本地完整 Cookie HRR 验证需指定 TLS_TEST_OPENSSL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	directory := t.TempDir()
	key, cert := filepath.Join(directory, "key.pem"), filepath.Join(directory, "cert.pem")
	generate := exec.CommandContext(ctx, openssl, "req", "-x509", "-newkey", "rsa:2048",
		"-nodes", "-keyout", key, "-out", cert, "-subj", "/CN=example.com",
		"-addext", "subjectAltName=DNS:example.com", "-days", "1")
	output, err := generate.CombinedOutput()
	require.NoError(t, err, string(output))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	server := exec.CommandContext(ctx, openssl, "s_server", "-accept", address,
		"-cert", cert, "-key", key, "-tls1_3", "-stateless", "-groups", "P-256", "-quiet")
	if os.Getenv("TLS_TEST_OPENSSL_TRACE") == "1" {
		server.Args = append(server.Args, "-state", "-msg")
	}
	// s_server 的 -www 分支不执行 stateless Cookie 流程，必须使用普通连接分支。
	stdin, err := server.StdinPipe()
	require.NoError(t, err)
	defer func() { _ = stdin.Close() }()
	// 只保留本地测试输出，退出后再读取，避免与管道复制协程竞争。
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	require.NoError(t, server.Start())
	defer func() {
		_ = server.Process.Kill()
		_ = server.Wait()
		if t.Failed() {
			t.Log(serverOutput.String())
		}
	}()
	var raw net.Conn
	for range 100 {
		raw, err = net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoError(t, err)
	require.NoError(t, raw.SetDeadline(time.Now().Add(8*time.Second)))
	roots := x509.NewCertPool()
	pem, err := os.ReadFile(cert)
	require.NoError(t, err)
	require.True(t, roots.AppendCertsFromPEM(pem))
	recording := &nativeOrderRecordingConn{Conn: &opensslStatelessFixtureConn{Conn: raw}}
	client, err := newTLSFingerprintClient(recording, nativeOrderTestProfile(),
		&utls.Config{RootCAs: roots, ServerName: "example.com"})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	hrr, ok := client.NetConn().(*rustlsHRRConn)
	require.True(t, ok, "Cookie 测试必须使用原生排序 HRR 连接")
	seed := hrr.seed
	require.NoError(t, client.HandshakeContext(ctx))
	require.Equal(t, uint16(0x0304), client.ConnectionState().Version)
	require.NotEmpty(t, client.ConnectionState().VerifiedChains, "必须真实完成证书链验证")
	const message = "cookie-hrr-ok\n"
	_, err = io.WriteString(stdin, message)
	require.NoError(t, err)
	reply := make([]byte, len(message))
	_, err = io.ReadFull(client, reply)
	require.NoError(t, err)
	require.Equal(t, message, string(reply))
	var hellos []*CapturedClientHello
	for data := recording.writes; len(data) >= 5; {
		size := 5 + (int(data[3]) << 8) + int(data[4])
		require.LessOrEqual(t, size, len(data))
		record := data[:size]
		data = data[size:]
		if record[0] == 22 && len(record) > 5 && record[5] == 1 {
			hello, err := ParseCapturedClientHello(record)
			require.NoError(t, err)
			hellos = append(hellos, hello)
		}
	}
	require.Len(t, hellos, 2, "stateless 服务端必须实际触发 Cookie HRR")
	baseIDs := nativeOrderTestProfile().Extensions
	require.Equal(t, rustlsExtensionOrder(baseIDs, seed), hellos[0].Extensions)
	require.Equal(t, rustlsExtensionOrder(append(slices.Clone(baseIDs), 44), seed), hellos[1].Extensions)
	t.Log("TLS1.3 + Cookie HRR + certificate verification + application data: PASS")
}
