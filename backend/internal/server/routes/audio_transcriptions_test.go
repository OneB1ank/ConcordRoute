package routes

import (
	"net/http/httptest"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/stretchr/testify/require"
)

// 路由测试确保别名都进入独立听写门禁，而非前端回退或 Live 放行。
func TestAudioTranscriptionRoutesDisabledAndPlatform(t *testing.T) {
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformAnthropic} {
		r := newGatewayRoutesTestRouter(platform)
		for _, path := range []string{"/v1/audio/transcriptions", "/audio/transcriptions", "/transcribe", "/backend-api/transcribe"} {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
			if platform == service.PlatformOpenAI {
				require.Equal(t, 403, w.Code, path)
				require.Contains(t, w.Body.String(), "Audio transcription is disabled")
			} else {
				require.Equal(t, 404, w.Code, path)
			}
			require.Contains(t, w.Header().Get("Content-Type"), "application/json")
		}
	}
}
