package tlsfingerprint

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"sort"

	utls "github.com/refraction-networking/utls"
)

// RFC 8446 §4.1.3：HRR 用此固定 random 区分于普通 ServerHello。
var rustlsHRRRandom = [32]byte{
	0xcf, 0x21, 0xad, 0x74, 0xe5, 0x9a, 0x61, 0x11,
	0xbe, 0x1d, 0x8c, 0x02, 0x1e, 0x65, 0xb8, 0x91,
	0xc2, 0xa2, 0x11, 0x16, 0x7a, 0xbb, 0x8c, 0x5e,
	0x07, 0x9e, 0x09, 0xe2, 0xc8, 0xa8, 0x33, 0x9c,
}

// newTLSFingerprintClient 仅配置 TLS，不接触 HTTP 身份或业务会话。
func newTLSFingerprintClient(conn net.Conn, profile *Profile, config *utls.Config) (*utls.UConn, error) {
	if err := profile.ValidateRustlsNativeOrder(); err != nil {
		return nil, err
	}
	var seed uint16
	var observed *rustlsHRRConn
	if profile != nil && profile.RustlsNativeOrder {
		seed = newRustlsOrderSeed()
		observed = &rustlsHRRConn{Conn: conn, seed: seed}
		conn = observed
	}
	spec := buildClientHelloSpecWithOrderSeed(profile, seed)
	client := utls.UClient(conn, config, utls.HelloCustom)
	if err := client.ApplyPreset(spec); err != nil {
		return nil, fmt.Errorf("apply TLS preset: %w", err)
	}
	if observed != nil {
		ids := defaultExtensionOrder
		if len(profile.Extensions) > 0 {
			ids = profile.Extensions
		}
		ids = rustlsExtensionOrder(ids, seed)
		if len(ids) != len(spec.Extensions) {
			return nil, fmt.Errorf("原生排序扩展映射与 TLS 模板不一致")
		}
		byExtension := make(map[utls.TLSExtension]uint16, len(ids)+1)
		for i, id := range ids {
			byExtension[spec.Extensions[i]] = id
		}
		observed.observer.onCookie = func() {
			// uTLS 1.8.2 在处理 HRR 时先找已有 *CookieExtension，未找到才随机插入。
			// 在原始 ServerHello 字节交给 uTLS 前预置正确位置，随后由 uTLS 校验并填充 Cookie。
			// 不改写网络字节、握手 transcript、第三方库或 Cookie 内容。
			for _, ext := range client.Extensions {
				if _, ok := byExtension[ext]; !ok {
					observed.orderErr = fmt.Errorf("uTLS 增加了未识别扩展，原生 HRR 排序已中止")
					return
				}
			}
			cookie := &utls.CookieExtension{}
			client.Extensions = append(client.Extensions, cookie)
			byExtension[cookie] = 44
			order := rustlsExtensionOrder(append(ids, 44), seed)
			rank := make(map[uint16]int, len(order))
			for i, id := range order {
				rank[id] = i
			}
			sort.SliceStable(client.Extensions, func(i, j int) bool {
				return rank[byExtension[client.Extensions[i]]] < rank[byExtension[client.Extensions[j]]]
			})
		}
	}
	return client, nil
}

// rustlsHRRConn 只在同步握手读取过程中观察首个 ServerHello，完成后不再解析数据。
// 它不改变任何成功读取的字节，证书、协议合法性及 transcript 验证仍由 uTLS 完成。
type rustlsHRRConn struct {
	net.Conn
	seed     uint16
	observer rustlsHRRObserver
	orderErr error
}

func (c *rustlsHRRConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.observer.feed(p[:n])
	if c.orderErr != nil {
		return 0, c.orderErr
	}
	return n, err
}

type rustlsHRRObserver struct {
	records   []byte
	handshake []byte
	done      bool
	onCookie  func()
}

func (o *rustlsHRRObserver) stop() {
	o.done = true
	o.records = nil
	o.handshake = nil
	o.onCookie = nil
}

// feed 支持 TCP 和 TLS 记录分片，最多保留 128 KiB 的首包元数据。
// 畸形输入立即停止观察，原字节继续交给 uTLS 的权威协议解析器处理。
func (o *rustlsHRRObserver) feed(data []byte) {
	if o.done {
		return
	}
	if len(o.records)+len(o.handshake)+len(data) > 128<<10 {
		o.stop()
		return
	}
	o.records = append(o.records, data...)
	for len(o.records) >= 5 {
		length := int(binary.BigEndian.Uint16(o.records[3:5]))
		if length > 18432 {
			o.stop()
			return
		}
		if len(o.records) < 5+length {
			return
		}
		recordType := o.records[0]
		body := o.records[5 : 5+length]
		if recordType == 22 {
			o.handshake = append(o.handshake, body...)
		} else if recordType != 20 {
			o.stop()
			return
		}
		o.records = o.records[5+length:]
		if len(o.handshake) < 4 {
			continue
		}
		size := int(o.handshake[1])<<16 | int(o.handshake[2])<<8 | int(o.handshake[3])
		if o.handshake[0] != 2 || size > 128<<10 {
			o.stop()
			return
		}
		if len(o.handshake) < 4+size {
			continue
		}
		hasCookie := rustlsHRRHasCookie(o.handshake[4 : 4+size])
		callback := o.onCookie
		o.stop()
		if hasCookie && callback != nil {
			callback()
		}
		return
	}
}

// rustlsHRRHasCookie 只识别完整 HRR 中的非空 Cookie，不生成或认证 Cookie。
func rustlsHRRHasCookie(body []byte) bool {
	if len(body) < 40 || !bytes.Equal(body[2:34], rustlsHRRRandom[:]) {
		return false
	}
	offset := 35 + int(body[34]) + 3 // session_id、cipher_suite、compression_method
	if offset+2 > len(body) {
		return false
	}
	size := int(binary.BigEndian.Uint16(body[offset : offset+2]))
	offset += 2
	if offset+size != len(body) {
		return false
	}
	found := false
	for offset < len(body) {
		if offset+4 > len(body) {
			return false
		}
		id := binary.BigEndian.Uint16(body[offset : offset+2])
		length := int(binary.BigEndian.Uint16(body[offset+2 : offset+4]))
		offset += 4
		if offset+length > len(body) {
			return false
		}
		if id == 44 {
			if found || length < 3 || int(binary.BigEndian.Uint16(body[offset:offset+2])) != length-2 {
				return false
			}
			found = true
		}
		offset += length
	}
	return found
}
