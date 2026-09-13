package liveattestation

import (
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCurrentCapabilityStatusKeepsClientRelaySeparateFromLiveProvider(t *testing.T) {
	status := CurrentCapabilityStatus(context.Background())

	require.True(t, status.ClientAttestationRelay)
	require.Equal(t, "windows_codex_app_server", status.ClientAttestationSource)
	require.True(t, status.LiveClientSupported)
	if runtime.GOOS != "darwin" {
		require.Equal(t, "client_relay", status.LiveAttestationMode)
	}
	require.Equal(t, runtime.GOOS, status.ServerPlatform)
	require.Equal(t, "windows", status.TLSProfilePlatform)
	require.Equal(t, []string{"windows"}, status.SupportedClientPlatforms)
	if runtime.GOOS != "darwin" {
		require.False(t, status.LiveDeviceCheckServer)
		require.NotEmpty(t, status.LiveDeviceCheckReason)
	}
}
