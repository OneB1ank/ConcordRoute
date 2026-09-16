package liveattestation

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCurrentCapabilityStatusKeepsClientRelaySeparateFromLiveProvider(t *testing.T) {
	t.Setenv("CONCORDROUTE_LIVE_ATTESTATION_HELPER", "")
	previous := AppServerAttestationTransportEnabled()
	SetAppServerAttestationTransport(false)
	t.Cleanup(func() { SetAppServerAttestationTransport(previous) })
	status := CurrentCapabilityStatus(context.Background())

	require.True(t, status.ClientAttestationRelay)
	require.Equal(t, "windows_codex_app_server", status.ClientAttestationSource)
	require.True(t, status.LiveClientSupported)
	if runtime.GOOS != "darwin" {
		require.Equal(t, "none", status.LiveAttestationMode)
	}
	require.True(t, status.LiveTransportSupported)
	require.Equal(t, runtime.GOOS, status.ServerPlatform)
	require.Equal(t, "windows", status.TLSProfilePlatform)
	require.Equal(t, []string{"windows"}, status.SupportedClientPlatforms)
	if runtime.GOOS != "darwin" {
		require.False(t, status.LiveDeviceCheckServer)
		require.NotEmpty(t, status.LiveDeviceCheckReason)
	}
}

// 提供器检查只验证本地前置条件，测试不得生成或探测真实证明。
type capabilityProviderStub struct{ err error }

func (p capabilityProviderStub) Check(context.Context) error { return p.err }
func (p capabilityProviderStub) Generate(context.Context) (string, error) {
	panic("capability checks must not generate attestations")
}

func TestCapabilitySourceCombinations(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider bool
		bridge   bool
		mode     string
	}{
		{"neither", false, false, "none"},
		{"client_only", false, true, "client_relay"},
		{"server_only", true, false, "server_provider"},
		{"both", true, true, "server_and_client_relay"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var providerErr error
			if !test.provider {
				providerErr = errors.New("provider is not configured")
			}
			status := currentCapabilityStatus(context.Background(), capabilityProviderStub{providerErr}, test.bridge)
			require.Equal(t, test.provider || test.bridge, status.AttestationSourceAvailable)
			require.Equal(t, test.provider, status.LiveDeviceCheckServer)
			require.Equal(t, test.bridge, status.AppServerAttestationTransport)
			require.Equal(t, test.mode, status.LiveAttestationMode)
			require.True(t, status.ClientAttestationRelay)
			require.True(t, status.LiveClientSupported)
			require.True(t, status.LiveTransportSupported)
		})
	}
}
