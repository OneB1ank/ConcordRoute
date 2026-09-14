package tlsfingerprint

import (
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/latencytrace"
	"github.com/stretchr/testify/require"
)

// 真实直连/HTTP/HTTPS/SOCKS 隧道采集 ClientHello，验证观测开关不改变扩展和协议配置。
// 捕获端在 ClientHello 后主动关闭，因此 TLS 失败应被如实记录，而非标成成功。
func TestLatencyTraceTLSProxyPhasesAndWireParity(t *testing.T) {
	for _, transport := range []string{"direct", "http", "https", "socks5"} {
		for _, enabled := range []bool{false, true} {
			t.Run(transport+map[bool]string{false: "/off", true: "/on"}[enabled], func(t *testing.T) {
				profile := localRegressionTLSProfile()
				before, err := json.Marshal(profile)
				require.NoError(t, err)
				var dial func(context.Context, string, string) (net.Conn, error)
				var capture <-chan localClientHelloResult
				switch transport {
				case "direct":
					address, ch := startLocalDirectHelloCapture(t)
					capture = ch
					dial = NewDialer(profile, func(ctx context.Context, network, _ string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, network, address)
					}).DialTLSContext
				case "socks5":
					proxyURL, ch := startLocalSOCKSHelloCapture(t)
					capture = ch
					dial = NewSOCKS5ProxyDialer(profile, proxyURL).DialTLSContext
				default:
					ch := make(chan localClientHelloResult, 1)
					capture = ch
					handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						time.Sleep(50 * time.Millisecond)
						localCONNECTCaptureHandler(ch)(w, r)
					})
					var server *httptest.Server
					if transport == "https" {
						server = httptest.NewTLSServer(handler)
					} else {
						server = httptest.NewServer(handler)
					}
					defer server.Close()
					proxyURL, err := url.Parse(server.URL)
					require.NoError(t, err)
					dialer := NewHTTPProxyDialer(profile, proxyURL)
					if transport == "https" {
						roots := x509.NewCertPool()
						roots.AddCert(server.Certificate())
						dialer.proxyTLSConfig = &stdtls.Config{RootCAs: roots, MinVersion: stdtls.VersionTLS12}
					}
					dial = dialer.DialTLSContext
				}
				var recorder *latencytrace.Recorder
				if enabled {
					recorder = latencytrace.New(time.Now())
				}
				hello := awaitLocalClientHello(t, func(ctx context.Context) (net.Conn, error) {
					ctx = latencytrace.WithHTTPTrace(latencytrace.WithRecorder(ctx, recorder), nil)
					return dial(ctx, "tcp", "upstream.example:443")
				}, capture)
				requireLocalProfileOnWire(t, profile, hello)
				after, err := json.Marshal(profile)
				require.NoError(t, err)
				require.Equal(t, before, after)
				if !enabled {
					require.Nil(t, recorder.Snapshot())
					return
				}
				data, err := json.Marshal(recorder.Snapshot())
				require.NoError(t, err)
				t.Log(string(data))
				events := map[string]latencytrace.Event{}
				for _, event := range recorder.Snapshot().Events {
					events[event.Phase] = event
				}
				require.Contains(t, events, "tcp_connect_done")
				require.Contains(t, events, "tls_client_handshake_started")
				require.True(t, events["tls_client_handshake_done"].Failed)
				if transport != "direct" {
					require.Contains(t, events, "proxy_tunnel_done")
					require.False(t, events["proxy_tunnel_done"].Failed)
					require.GreaterOrEqual(t, events["tls_client_handshake_started"].AtMS, events["proxy_tunnel_done"].AtMS)
				}
				if transport == "https" {
					require.Contains(t, events, "proxy_tls_done")
				}
				if transport == "http" || transport == "https" {
					require.GreaterOrEqual(t, events["proxy_tunnel_done"].AtMS-events["proxy_tunnel_started"].AtMS, int64(40))
				}
			})
		}
	}
}
