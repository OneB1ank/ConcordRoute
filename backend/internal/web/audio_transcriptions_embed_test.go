//go:build embed

package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 打包态必须先让听写别名到达 API 鉴权，而不是由 SPA 返回成功 HTML。
func TestAudioTranscriptionEmbeddedRouting(t *testing.T) {
	server, err := NewFrontendServer(&mockSettingsProvider{settings: map[string]string{}})
	require.NoError(t, err)
	for name, handler := range map[string]gin.HandlerFunc{
		"settings": server.Middleware(),
		"legacy":   ServeEmbeddedFrontend(),
	} {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{"/v1/audio/transcriptions", "/audio/transcriptions", "/transcribe", "/backend-api/transcribe"} {
				t.Run(path, func(t *testing.T) {
					router := gin.New()
					router.Use(handler)
					router.POST(path, func(c *gin.Context) {
						c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "test API authentication"})
					})
					w := httptest.NewRecorder()
					router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
					require.Equal(t, http.StatusUnauthorized, w.Code)
					require.Contains(t, w.Header().Get("Content-Type"), "application/json")
					require.Contains(t, w.Body.String(), "test API authentication")
				})
			}
			// 精确匹配不会把类似名称的普通页面扩展为 API 前缀。
			for _, path := range []string{"/transcribe-notes", "/audio/transcriptions-help"} {
				router := gin.New()
				router.Use(handler)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				require.Equal(t, http.StatusOK, w.Code)
				require.Contains(t, w.Header().Get("Content-Type"), "text/html")
			}
		})
	}
}
