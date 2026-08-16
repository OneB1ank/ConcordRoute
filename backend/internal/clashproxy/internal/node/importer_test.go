package node

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImportClashYAMLNormalizesStaticProxies(t *testing.T) {
	nodes, err := ImportClashYAML([]byte(`
external-controller: 0.0.0.0:9090
tun:
  enable: true
dns:
  enable: true
proxies:
  - name: Proxy A
    type: http
    server: proxy.example.com
    port: 8080
    username: user
    password: pass
  - name: Proxy A
    type: socks5
    server: 127.0.0.1
    port: 1080
    password: secret
proxy-groups:
  - name: dangerous
    type: select
    proxies: [Proxy A]
`))
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	require.Equal(t, "Proxy A", nodes[0].Name)
	require.Equal(t, "Proxy A (2)", nodes[1].Name)
	require.Equal(t, TypeHTTP, nodes[0].Type)
	require.Equal(t, SourceClashSubscription, nodes[0].SourceType)
	require.Equal(t, "proxy.example.com", nodes[0].Config["server"])
	require.Equal(t, 8080, nodes[0].Config["port"])
	require.Equal(t, "user", nodes[0].Secret["username"])
	require.Equal(t, "pass", nodes[0].Secret["password"])
	require.NotContains(t, nodes[0].Config, "password")
	require.NotContains(t, nodes[0].Config, "external-controller")
	require.NotContains(t, nodes[0].Config, "tun")
	require.NotContains(t, nodes[0].Config, "dns")
}

func TestImportClashYAMLNormalizesStaticProxyProviders(t *testing.T) {
	nodes, err := ImportClashYAML([]byte(`
proxy-providers:
  remote:
    type: http
    url: http://127.0.0.1/sub.yaml
    path: ./danger.yaml
    health-check:
      enable: true
    proxies:
      - name: Provider Proxy
        type: trojan
        server: trojan.example.com
        port: 443
        password: provider-secret
`))
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Equal(t, "Provider Proxy", nodes[0].Name)
	require.Equal(t, TypeTrojan, nodes[0].Type)
	require.Equal(t, "trojan.example.com", nodes[0].Config["server"])
	require.Equal(t, 443, nodes[0].Config["port"])
	require.Equal(t, "provider-secret", nodes[0].Secret["password"])
	require.NotContains(t, nodes[0].Config, "url")
	require.NotContains(t, nodes[0].Config, "path")
	require.NotContains(t, nodes[0].Config, "health-check")
}

func TestImportClashYAMLRejectsUnsupportedProxyType(t *testing.T) {
	_, err := ImportClashYAML([]byte(`
proxies:
  - name: Bad
    type: unknown
    server: proxy.example.com
    port: 8080
`))
	require.ErrorContains(t, err, "unsupported clash proxy type")
}

func TestImportClashYAMLPreservesAdvancedNodeOptions(t *testing.T) {
	nodes, err := ImportClashYAML([]byte(`
proxies:
  - name: WS Node
    type: vless
    server: edge.example.com
    port: 443
    uuid: 00000000-0000-0000-0000-000000000001
    network: ws
    tls: true
    ws-opts:
      path: /gateway
      headers:
        Host: cdn.example.com
`))
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Equal(t, "ws", nodes[0].Config["network"])
	require.Contains(t, nodes[0].Config, "ws-opts")
	require.Equal(t, "00000000-0000-0000-0000-000000000001", nodes[0].Secret["uuid"])
}

func TestImportURISupportsManualSchemes(t *testing.T) {
	n, err := ImportURI("http://user:pass@proxy.example.com:8080")
	require.NoError(t, err)
	require.Equal(t, TypeHTTP, n.Type)
	require.Equal(t, SourceURI, n.SourceType)
	require.Equal(t, "pass", n.Secret["password"])

	n, err = ImportURI("trojan://password@example.com:443?sni=edge.example.com#Edge")
	require.NoError(t, err)
	require.Equal(t, "Edge", n.Name)
	require.Equal(t, TypeTrojan, n.Type)
	require.Equal(t, "example.com", n.Config["server"])
	require.Equal(t, 443, n.Config["port"])
	require.Equal(t, "edge.example.com", n.Config["sni"])
	require.Equal(t, "password", n.Secret["password"])
}

func TestImportURISupportsVMessBase64JSON(t *testing.T) {
	payload := base64.RawStdEncoding.EncodeToString([]byte(`{
		"ps":"VMess Edge",
		"add":"vmess.example.com",
		"port":"443",
		"id":"00000000-0000-0000-0000-000000000001",
		"aid":"0",
		"net":"ws",
		"tls":"tls",
		"sni":"vmess-sni.example.com"
	}`))
	n, err := ImportURI("vmess://" + payload)
	require.NoError(t, err)
	require.Equal(t, "VMess Edge", n.Name)
	require.Equal(t, TypeVMess, n.Type)
	require.Equal(t, SourceURI, n.SourceType)
	require.Equal(t, "vmess.example.com", n.Config["server"])
	require.Equal(t, 443, n.Config["port"])
	require.Equal(t, "ws", n.Config["network"])
	require.Equal(t, "vmess-sni.example.com", n.Config["sni"])
	require.Equal(t, "00000000-0000-0000-0000-000000000001", n.Secret["uuid"])
}

func TestImportURIRejectsUnsupportedScheme(t *testing.T) {
	_, err := ImportURI("ftp://example.com/file")
	require.ErrorContains(t, err, "unsupported proxy URI scheme")
}
