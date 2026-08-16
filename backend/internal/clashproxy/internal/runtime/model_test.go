package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstanceProxyURL(t *testing.T) {
	proxyURL, err := Instance{ProfileID: 12, MixedPort: 17012, Status: StatusRunning}.ProxyURL()
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:17012", proxyURL)

	_, err = Instance{ProfileID: 12, MixedPort: 17012, Status: StatusFailed}.ProxyURL()
	require.ErrorContains(t, err, "not running")
}
