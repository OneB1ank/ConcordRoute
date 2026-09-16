package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/platform/liveattestation"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 证明来源状态随连接更新，但不应把可选证明来源误当成 Live 传输前置条件。
func TestGetLiveCapabilityTracksAvailableSource(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("local macOS provider may be available")
	}
	t.Setenv("CONCORDROUTE_LIVE_ATTESTATION_HELPER", "")
	previous := liveattestation.AppServerAttestationTransportEnabled()
	t.Cleanup(func() { liveattestation.SetAppServerAttestationTransport(previous) })
	gin.SetMode(gin.TestMode)
	for _, connected := range []bool{false, true, false} {
		liveattestation.SetAppServerAttestationTransport(connected)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/groups/live-capability", nil)
		(&GroupHandler{}).GetLiveCapability(c)

		var body struct {
			Data struct {
				Supported                  bool   `json:"supported"`
				ServerSupported            bool   `json:"server_supported"`
				TransportSupported         bool   `json:"live_transport_supported"`
				AttestationPolicy          string `json:"attestation_policy"`
				AttestationSourceAvailable bool   `json:"attestation_source_available"`
				ClientRelay                bool   `json:"client_attestation_relay"`
				Reason                     string `json:"reason"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		require.True(t, body.Data.Supported)
		require.True(t, body.Data.ServerSupported)
		require.True(t, body.Data.TransportSupported)
		require.Equal(t, "if_available", body.Data.AttestationPolicy)
		require.Equal(t, connected, body.Data.AttestationSourceAvailable)
		require.True(t, body.Data.ClientRelay)
		require.Empty(t, body.Data.Reason)
	}
}

func TestGetLiveCapabilitySeparatesLinuxRelayFromServerProvider(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("server-provider semantics differ on macOS")
	}
	t.Setenv("CONCORDROUTE_LIVE_ATTESTATION_HELPER", "")
	previous := liveattestation.AppServerAttestationTransportEnabled()
	liveattestation.SetAppServerAttestationTransport(false)
	t.Cleanup(func() { liveattestation.SetAppServerAttestationTransport(previous) })

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/groups/live-capability", nil)

	(&GroupHandler{}).GetLiveCapability(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	body := recorder.Body.String()
	require.Contains(t, body, `"server_supported":true`)
	require.Contains(t, body, `"live_transport_supported":true`)
	require.Contains(t, body, `"attestation_source_available":false`)
	require.Contains(t, body, `"live_devicecheck_server":false`)
	require.Contains(t, body, `"client_attestation_relay":true`)
	require.Contains(t, body, `"server_attestation_provider":"none"`)
	require.Contains(t, body, `"app_server_attestation_transport":false`)
	require.Contains(t, body, `"live_attestation_mode":"none"`)
}
