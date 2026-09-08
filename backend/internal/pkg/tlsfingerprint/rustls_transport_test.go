package tlsfingerprint

import (
	"bufio"
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/stretchr/testify/require"
)

// 测试只访问回环地址，不使用真实账号、授权令牌或会话内容。
func nativeOrderTestProfile() *Profile {
	return &Profile{
		Name: "rustls native test", RustlsNativeOrder: true,
		CipherSuites: []uint16{0x1301, 0x1302, 0x1303, 0xc02f},
		Curves:       []uint16{29, 23}, PointFormats: []uint16{0},
		SignatureAlgorithms: []uint16{0x0804, 0x0805, 0x0806, 0x0403},
		ALPNProtocols:       []string{"http/1.1"}, SupportedVersions: []uint16{0x0304, 0x0303},
		KeyShareGroups: []uint16{29}, PSKModes: []uint16{1},
		Extensions: []uint16{0, 5, 10, 11, 13, 16, 23, 35, 43, 45, 51},
	}
}

// HTTP/HTTPS CONNECT、SOCKS5 和直连共用同一排序入口，线上不依赖代理类型换算法。
func TestRustlsNativeOrderOnWire(t *testing.T) {
	profile := nativeOrderTestProfile()
	allowed := make(map[string]bool)
	for seed := 0; seed < 65536; seed++ {
		allowed[fmt.Sprint(rustlsExtensionOrder(profile.Extensions, uint16(seed)))] = true
	}
	for _, kind := range []string{"direct", "http", "https", "socks5"} {
		t.Run(kind, func(t *testing.T) {
			var dial func(context.Context) (net.Conn, error)
			var resultCh <-chan localClientHelloResult
			switch kind {
			case "direct":
				address, results := startLocalDirectHelloCapture(t)
				resultCh = results
				d := NewDialer(profile, func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, address)
				})
				dial = func(ctx context.Context) (net.Conn, error) {
					return d.DialTLSContext(ctx, "tcp", "upstream.example:443")
				}
			case "http", "https":
				results := make(chan localClientHelloResult, 1)
				resultCh = results
				server := httptest.NewUnstartedServer(localCONNECTCaptureHandler(results))
				if kind == "https" {
					server.StartTLS()
				} else {
					server.Start()
				}
				t.Cleanup(server.Close)
				proxyURL, err := url.Parse(server.URL)
				require.NoError(t, err)
				d := NewHTTPProxyDialer(profile, proxyURL)
				if kind == "https" {
					roots := x509.NewCertPool()
					roots.AddCert(server.Certificate())
					d.proxyTLSConfig = &stdtls.Config{RootCAs: roots, MinVersion: stdtls.VersionTLS12}
				}
				dial = func(ctx context.Context) (net.Conn, error) {
					return d.DialTLSContext(ctx, "tcp", "upstream.example:443")
				}
			case "socks5":
				proxyURL, results := startLocalSOCKSHelloCapture(t)
				resultCh = results
				d := NewSOCKS5ProxyDialer(profile, proxyURL)
				dial = func(ctx context.Context) (net.Conn, error) {
					return d.DialTLSContext(ctx, "tcp", "upstream.example:443")
				}
			}
			hello := awaitLocalClientHello(t, dial, resultCh)
			require.True(t, allowed[fmt.Sprint(hello.Extensions)], "实际首包必须属于原生算法的输出")
			onWireProfile := *profile
			onWireProfile.Extensions = hello.Extensions
			requireLocalProfileOnWire(t, &onWireProfile, hello)
		})
	}
}

// 记录客户端写入，用于区分首包和普通 key_share HRR 的第二个 ClientHello。
type nativeOrderRecordingConn struct {
	net.Conn
	writes []byte
}

func (c *nativeOrderRecordingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.writes = append(c.writes, p[:n]...)
	return n, err
}

// 完整握手验证使用本地受信任证书；两次请求复用连接且请求字段保持原样。
func TestRustlsNativeOrderHandshakeAndHTTPIdentity(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		version uint16
		hrr     bool
		ech     bool
	}{
		{"tls12", stdtls.VersionTLS12, false, false},
		{"tls13", stdtls.VersionTLS13, false, false},
		{"tls13_keyshare_hrr", stdtls.VersionTLS13, true, false},
		{"tls13_ech_suffix_hrr", stdtls.VersionTLS13, true, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			const body = `{"session_id":"SESSION-TEST","prompt_cache_key":"CACHE-TEST","root_turn_id":"TURN-TEST"}`
			received := make(chan string, 2)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					received <- err.Error()
				} else {
					received <- r.UserAgent() + "|" + r.Header.Get("Chatgpt-Account-Id") + "|" + string(data)
				}
				_, _ = io.WriteString(w, "ok")
			}))
			server.TLS = &stdtls.Config{MinVersion: stdtls.VersionTLS12, MaxVersion: scenario.version}
			if scenario.version == stdtls.VersionTLS13 {
				server.TLS.MinVersion = stdtls.VersionTLS13
			}
			if scenario.hrr {
				server.TLS.CurvePreferences = []stdtls.CurveID{stdtls.CurveP256}
			}
			server.StartTLS()
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			raw, err := net.Dial("tcp", server.Listener.Addr().String())
			require.NoError(t, err)
			require.NoError(t, raw.SetDeadline(time.Now().Add(5*time.Second)))
			recording := &nativeOrderRecordingConn{Conn: raw}
			profile := nativeOrderTestProfile()
			if scenario.ech {
				profile.Extensions = append(profile.Extensions, 65037)
			}
			client, err := newTLSFingerprintClient(recording, profile, &utls.Config{RootCAs: roots, ServerName: "example.com"})
			require.NoError(t, err)
			defer func() { _ = client.Close() }()
			require.NoError(t, client.HandshakeContext(context.Background()))
			require.Equal(t, scenario.version, client.ConnectionState().Version)
			reader := bufio.NewReader(client)
			for range 2 {
				request, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", strings.NewReader(body))
				require.NoError(t, err)
				request.Header.Set("User-Agent", "CLIENT-UA-TEST")
				request.Header.Set("Chatgpt-Account-Id", "ACCOUNT-TEST")
				require.NoError(t, request.Write(client))
				response, err := http.ReadResponse(reader, request)
				require.NoError(t, err)
				_, err = io.Copy(io.Discard, response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Equal(t, "CLIENT-UA-TEST|ACCOUNT-TEST|"+body, <-received)
			}
			var orders [][]uint16
			for data := recording.writes; len(data) >= 5; {
				size := 5 + (int(data[3]) << 8) + int(data[4])
				require.LessOrEqual(t, size, len(data))
				record := data[:size]
				data = data[size:]
				if record[0] == 22 && len(record) > 5 && record[5] == 1 {
					hello, err := ParseCapturedClientHello(record)
					require.NoError(t, err)
					orders = append(orders, slices.Clone(hello.Extensions))
				}
			}
			if scenario.hrr {
				require.Len(t, orders, 2)
				require.Equal(t, orders[0], orders[1], "普通 HRR 保持首包的排序种子效果")
			} else {
				require.Len(t, orders, 1, "同一连接的第二个请求不应重新握手")
			}
			if scenario.ech {
				require.Equal(t, uint16(65037), orders[0][len(orders[0])-1])
			}
		})
	}
}
