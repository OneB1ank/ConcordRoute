package admin

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGetLiveCapabilitySeparatesLinuxRelayFromServerProvider(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("server-provider semantics differ on macOS")
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/groups/live-capability", nil)

	(&GroupHandler{}).GetLiveCapability(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	body := recorder.Body.String()
	require.Contains(t, body, `"server_supported":true`)
	require.Contains(t, body, `"live_devicecheck_server":false`)
	require.Contains(t, body, `"client_attestation_relay":true`)
	require.Contains(t, body, `"server_attestation_provider":"none"`)
	require.Contains(t, body, `"app_server_attestation_transport":false`)
	require.Contains(t, body, `"live_attestation_mode":"client_relay"`)
}
