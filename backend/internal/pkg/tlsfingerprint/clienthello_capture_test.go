package tlsfingerprint

import (
	"bufio"
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHTTPSProxyTunnelPreservesTargetUTLSProfile(t *testing.T) {
	profile := &Profile{
		Name:                "HTTPS proxy target fingerprint",
		EnableGREASE:        true,
		CipherSuites:        []uint16{0x1301, 0x1302, 0x1303},
		Curves:              []uint16{0x001d, 0x0017},
		PointFormats:        []uint16{0},
		SignatureAlgorithms: []uint16{0x0403, 0x0804, 0x0401},
		ALPNProtocols:       []string{"h2", "http/1.1"},
		SupportedVersions:   []uint16{0x0304, 0x0303},
		KeyShareGroups:      []uint16{0x001d},
		PSKModes:            []uint16{1},
		Extensions:          []uint16{0, 10, 11, 13, 16, 43, 45, 51},
	}
	type captureResult struct {
		hello *CapturedClientHello
		err   error
	}
	resultCh := make(chan captureResult, 1)
	proxyServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "upstream.example:443" {
			resultCh <- captureResult{err: &url.Error{Op: r.Method, URL: r.Host, Err: http.ErrNotSupported}}
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			resultCh <- captureResult{err: http.ErrNotSupported}
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			resultCh <- captureResult{err: err}
			return
		}
		defer func() { _ = conn.Close() }()
		if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err == nil {
			err = rw.Flush()
		}
		if err != nil {
			resultCh <- captureResult{err: err}
			return
		}

		header, err := rw.Peek(5)
		if err != nil {
			resultCh <- captureResult{err: err}
			return
		}
		recordLen := int(header[3])<<8 | int(header[4])
		record, err := rw.Peek(5 + recordLen)
		if err != nil {
			resultCh <- captureResult{err: err}
			return
		}
		copied := append([]byte(nil), record...)
		hello, err := ParseCapturedClientHello(copied)
		resultCh <- captureResult{hello: hello, err: err}
	}))
	defer proxyServer.Close()

	proxyURL, err := url.Parse(proxyServer.URL)
	require.NoError(t, err)
	dialer := NewHTTPProxyDialer(profile, proxyURL)
	proxyRoots := x509.NewCertPool()
	proxyRoots.AddCert(proxyServer.Certificate())
	dialer.proxyTLSConfig = &stdtls.Config{RootCAs: proxyRoots, MinVersion: stdtls.VersionTLS12}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, dialErr := dialer.DialTLSContext(ctx, "tcp", "upstream.example:443")
	if conn != nil {
		_ = conn.Close()
	}
	require.Error(t, dialErr, "捕获 ClientHello 后代理会主动关闭隧道")

	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		require.NotNil(t, result.hello)
		require.Equal(t, profile.CipherSuites, result.hello.CipherSuites)
		require.Equal(t, profile.ALPNProtocols, result.hello.ALPNProtocols)
		require.Equal(t, profile.Curves, result.hello.Curves)
		require.Equal(t, profile.SupportedVersions, result.hello.SupportedVersions)
		require.Equal(t, profile.KeyShareGroups, result.hello.KeyShareGroups)
		require.True(t, result.hello.EnableGREASE)
	case <-ctx.Done():
		t.Fatal("等待 HTTPS 代理内层 ClientHello 超时")
	}
}

func TestParseCapturedClientHelloFromUTLSDialer(t *testing.T) {
	profile := &Profile{
		Name:                "capture test",
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
	recordCh := make(chan []byte, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		reader := bufio.NewReader(conn)
		header, err := reader.Peek(5)
		if err != nil {
			return
		}
		recordLen := int(header[3])<<8 | int(header[4])
		record, err := reader.Peek(5 + recordLen)
		if err != nil {
			return
		}
		copied := make([]byte, len(record))
		copy(copied, record)
		recordCh <- copied
	}()

	dialer := NewDialer(profile, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dialer.DialTLSContext(ctx, "tcp", ln.Addr().String())
	if conn != nil {
		_ = conn.Close()
	}
	require.Error(t, err)

	record := <-recordCh
	captured, err := ParseCapturedClientHello(record)
	require.NoError(t, err)
	require.Equal(t, []uint16{0x1301, 0x1302, 0x1303}, captured.CipherSuites)
	require.Equal(t, uint16(0x0a0a), captured.Extensions[0])
	require.Subset(t, captured.Extensions, []uint16{10, 11, 13, 16, 43, 45, 51})
	require.Equal(t, []uint16{0x001d, 0x0017}, captured.Curves)
	require.Equal(t, []uint16{0}, captured.PointFormats)
	require.Equal(t, []uint16{0x0403, 0x0804, 0x0401}, captured.SignatureAlgorithms)
	require.Equal(t, []string{"h2", "http/1.1"}, captured.ALPNProtocols)
	require.Equal(t, []uint16{0x0304, 0x0303}, captured.SupportedVersions)
	require.Equal(t, []uint16{0x001d}, captured.KeyShareGroups)
	require.Equal(t, []uint16{1}, captured.PSKModes)
	require.True(t, captured.EnableGREASE)
	require.NotEmpty(t, captured.JA3Raw)
	require.Len(t, captured.JA3Hash, 32)
}
