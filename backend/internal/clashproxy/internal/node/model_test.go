package node

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestManualURLSeparatesSecrets(t *testing.T) {
	n, err := ManualURL("Office Proxy", "socks5h://user:pass@127.0.0.1:1080")
	require.NoError(t, err)
	require.Equal(t, "Office Proxy", n.Name)
	require.Equal(t, TypeSOCKS5, n.Type)
	require.Equal(t, SourceManual, n.SourceType)
	require.Equal(t, "127.0.0.1", n.Config["server"])
	require.Equal(t, 1080, n.Config["port"])
	require.Equal(t, "user", n.Secret["username"])
	require.Equal(t, "pass", n.Secret["password"])
	require.NotContains(t, n.Config, "password")
}

func TestManualURLRejectsUnsupportedScheme(t *testing.T) {
	_, err := ManualURL("bad", "ftp://example.com:21")
	require.ErrorContains(t, err, "unsupported node type")
}
