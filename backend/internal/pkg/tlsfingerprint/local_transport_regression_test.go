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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type localClientHelloResult struct {
	hello *CapturedClientHello
	err   error
}

func localRegressionTLSProfile() *Profile {
	return &Profile{
		Name:                "local transport regression",
		EnableGREASE:        true,
		CipherSuites:        []uint16{0x1301, 0x1302, 0x1303},
		Curves:              []uint16{0x001d, 0x0017},
		PointFormats:        []uint16{0},
		SignatureAlgorithms: []uint16{0x0403, 0x0804, 0x0401},
		ALPNProtocols:       []string{"h2", "http/1.1"},
		SupportedVersions:   []uint16{0x0304, 0x0303},
		KeyShareGroups:      []uint16{0x001d},
		PSKModes:            []uint16{1},
		Extensions:          []uint16{0x0a0a, 0, 10, 11, 13, 16, 43, 45, 51},
	}
}

func captureLocalClientHello(reader *bufio.Reader) (*CapturedClientHello, error) {
	header, err := reader.Peek(5)
	if err != nil {
		return nil, err
	}
	recordLen := int(header[3])<<8 | int(header[4])
	record, err := reader.Peek(5 + recordLen)
	if err != nil {
		return nil, err
	}
	return ParseCapturedClientHello(append([]byte(nil), record...))
}

func awaitLocalClientHello(
	t *testing.T,
	dial func(context.Context) (net.Conn, error),
	resultCh <-chan localClientHelloResult,
) *CapturedClientHello {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, dialErr := dial(ctx)
	if conn != nil {
		_ = conn.Close()
	}
	require.Error(t, dialErr, "capture endpoint closes before completing TLS")
	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		require.NotNil(t, result.hello)
		return result.hello
	case <-ctx.Done():
		t.Fatal("timed out waiting for local ClientHello")
		return nil
	}
}

func startLocalDirectHelloCapture(t *testing.T) (string, <-chan localClientHelloResult) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	resultCh := make(chan localClientHelloResult, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			resultCh <- localClientHelloResult{err: acceptErr}
			return
		}
		defer func() { _ = conn.Close() }()
		hello, captureErr := captureLocalClientHello(bufio.NewReader(conn))
		resultCh <- localClientHelloResult{hello: hello, err: captureErr}
	}()
	return listener.Addr().String(), resultCh
}

func localCONNECTCaptureHandler(resultCh chan<- localClientHelloResult) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "upstream.example:443" {
			http.Error(w, "unexpected CONNECT target", http.StatusBadRequest)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			resultCh <- localClientHelloResult{err: fmt.Errorf("response writer does not support hijacking")}
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			resultCh <- localClientHelloResult{err: err}
			return
		}
		defer func() { _ = conn.Close() }()
		if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err == nil {
			err = rw.Flush()
		}
		if err != nil {
			resultCh <- localClientHelloResult{err: err}
			return
		}
		hello, captureErr := captureLocalClientHello(rw.Reader)
		resultCh <- localClientHelloResult{hello: hello, err: captureErr}
	}
}

func startLocalSOCKSHelloCapture(t *testing.T) (*url.URL, <-chan localClientHelloResult) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	resultCh := make(chan localClientHelloResult, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			resultCh <- localClientHelloResult{err: acceptErr}
			return
		}
		defer func() { _ = conn.Close() }()
		reader := bufio.NewReader(conn)
		greeting := make([]byte, 2)
		if _, err = io.ReadFull(reader, greeting); err != nil || greeting[0] != 5 {
			resultCh <- localClientHelloResult{err: err}
			return
		}
		methods := make([]byte, int(greeting[1]))
		if _, err = io.ReadFull(reader, methods); err != nil {
			resultCh <- localClientHelloResult{err: err}
			return
		}
		if _, err = conn.Write([]byte{5, 0}); err != nil {
			resultCh <- localClientHelloResult{err: err}
			return
		}
		request := make([]byte, 4)
		if _, err = io.ReadFull(reader, request); err != nil || request[0] != 5 || request[1] != 1 {
			resultCh <- localClientHelloResult{err: err}
			return
		}
		addressBytes := 0
		switch request[3] {
		case 1:
			addressBytes = net.IPv4len
		case 3:
			length, readErr := reader.ReadByte()
			if readErr != nil {
				resultCh <- localClientHelloResult{err: readErr}
				return
			}
			addressBytes = int(length)
		case 4:
			addressBytes = net.IPv6len
		default:
			resultCh <- localClientHelloResult{err: http.ErrNotSupported}
			return
		}
		addressAndPort := make([]byte, addressBytes+2)
		if _, err = io.ReadFull(reader, addressAndPort); err != nil {
			resultCh <- localClientHelloResult{err: err}
			return
		}
		if _, err = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
			resultCh <- localClientHelloResult{err: err}
			return
		}
		hello, captureErr := captureLocalClientHello(reader)
		resultCh <- localClientHelloResult{hello: hello, err: captureErr}
	}()
	proxyURL, err := url.Parse("socks5h://" + listener.Addr().String())
	require.NoError(t, err)
	return proxyURL, resultCh
}

func requireLocalProfileOnWire(t *testing.T, profile *Profile, hello *CapturedClientHello) {
	t.Helper()
	require.Equal(t, profile.CipherSuites, hello.CipherSuites)
	require.Equal(t, profile.Extensions, hello.Extensions)
	require.Equal(t, profile.Curves, hello.Curves)
	require.Equal(t, profile.PointFormats, hello.PointFormats)
	require.Equal(t, profile.SignatureAlgorithms, hello.SignatureAlgorithms)
	require.Equal(t, profile.ALPNProtocols, hello.ALPNProtocols)
	require.Equal(t, profile.SupportedVersions, hello.SupportedVersions)
	require.Equal(t, profile.KeyShareGroups, hello.KeyShareGroups)
	require.Equal(t, profile.PSKModes, hello.PSKModes)
	require.Equal(t, profile.EnableGREASE, hello.EnableGREASE)
	require.NotEmpty(t, hello.JA3Raw)
	require.Len(t, hello.JA3Hash, 32)
}

func TestLocalTransportClientHelloParity(t *testing.T) {
	profile := localRegressionTLSProfile()
	captured := make(map[string]*CapturedClientHello)

	t.Run("direct", func(t *testing.T) {
		address, resultCh := startLocalDirectHelloCapture(t)
		networkDialer := &net.Dialer{}
		dialer := NewDialer(profile, func(ctx context.Context, network, _ string) (net.Conn, error) {
			return networkDialer.DialContext(ctx, network, address)
		})
		captured["direct"] = awaitLocalClientHello(t, func(ctx context.Context) (net.Conn, error) {
			return dialer.DialTLSContext(ctx, "tcp", "upstream.example:443")
		}, resultCh)
	})

	t.Run("http_connect", func(t *testing.T) {
		resultCh := make(chan localClientHelloResult, 1)
		proxyServer := httptest.NewServer(localCONNECTCaptureHandler(resultCh))
		defer proxyServer.Close()
		proxyURL, err := url.Parse(proxyServer.URL)
		require.NoError(t, err)
		dialer := NewHTTPProxyDialer(profile, proxyURL)
		captured["http_connect"] = awaitLocalClientHello(t, func(ctx context.Context) (net.Conn, error) {
			return dialer.DialTLSContext(ctx, "tcp", "upstream.example:443")
		}, resultCh)
	})

	t.Run("https_connect", func(t *testing.T) {
		resultCh := make(chan localClientHelloResult, 1)
		proxyServer := httptest.NewTLSServer(localCONNECTCaptureHandler(resultCh))
		defer proxyServer.Close()
		proxyURL, err := url.Parse(proxyServer.URL)
		require.NoError(t, err)
		dialer := NewHTTPProxyDialer(profile, proxyURL)
		roots := x509.NewCertPool()
		roots.AddCert(proxyServer.Certificate())
		dialer.proxyTLSConfig = &stdtls.Config{RootCAs: roots, MinVersion: stdtls.VersionTLS12}
		captured["https_connect"] = awaitLocalClientHello(t, func(ctx context.Context) (net.Conn, error) {
			return dialer.DialTLSContext(ctx, "tcp", "upstream.example:443")
		}, resultCh)
	})

	t.Run("socks5", func(t *testing.T) {
		proxyURL, resultCh := startLocalSOCKSHelloCapture(t)
		dialer := NewSOCKS5ProxyDialer(profile, proxyURL)
		captured["socks5"] = awaitLocalClientHello(t, func(ctx context.Context) (net.Conn, error) {
			return dialer.DialTLSContext(ctx, "tcp", "upstream.example:443")
		}, resultCh)
	})

	baseline := captured["direct"]
	require.NotNil(t, baseline)
	for name, hello := range captured {
		t.Run("assert_"+name, func(t *testing.T) {
			requireLocalProfileOnWire(t, profile, hello)
			require.Equal(t, baseline.JA3Raw, hello.JA3Raw)
			require.Equal(t, baseline.JA3Hash, hello.JA3Hash)
		})
	}
}

func TestLocalWebSocketProfileKeepsALPNExtension16(t *testing.T) {
	base := localRegressionTLSProfile()
	wsProfile := HTTP1OnlyProfile(base)
	require.NotSame(t, base, wsProfile)
	require.Equal(t, []string{"http/1.1"}, wsProfile.ALPNProtocols)
	require.Contains(t, wsProfile.Extensions, uint16(16))
	require.Equal(t, base.CipherSuites, wsProfile.CipherSuites)
	require.Equal(t, base.Curves, wsProfile.Curves)
	require.Equal(t, base.SignatureAlgorithms, wsProfile.SignatureAlgorithms)
	require.Equal(t, base.SupportedVersions, wsProfile.SupportedVersions)
	require.Equal(t, base.KeyShareGroups, wsProfile.KeyShareGroups)

	address, resultCh := startLocalDirectHelloCapture(t)
	networkDialer := &net.Dialer{}
	dialer := NewDialer(wsProfile, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return networkDialer.DialContext(ctx, network, address)
	})
	hello := awaitLocalClientHello(t, func(ctx context.Context) (net.Conn, error) {
		return dialer.DialTLSContext(ctx, "tcp", "upstream.example:443")
	}, resultCh)
	requireLocalProfileOnWire(t, wsProfile, hello)
	require.Equal(t, []string{"http/1.1"}, hello.ALPNProtocols)
	require.Contains(t, hello.Extensions, uint16(16), "ALPN extension must remain on the wire")
}
