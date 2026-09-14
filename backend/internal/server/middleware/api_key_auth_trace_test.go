//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/pkg/latencytrace"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 两种运行模式的认证结束都应先于下游 handler，诊断关闭时不挂载采样状态。
func TestAPIKeyAuthTraceStopsBeforeDownstream(t *testing.T) {
	for _, mode := range []string{config.RunModeSimple, config.RunModeStandard} {
		for _, enabled := range []bool{false, true} {
			cfg := &config.Config{RunMode: mode}
			key := &service.APIKey{ID: 941, UserID: 942, Key: "unit-key", Status: service.StatusActive,
				User: &service.User{ID: 942, Status: service.StatusActive, Balance: 100, Concurrency: 2}}
			repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) { return key, nil }}
			svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			r := gin.New()
			r.Use(TTFTDiagnostics(enabled))
			r.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(svc, nil, cfg)))
			called := false
			r.POST("/v1/responses", func(c *gin.Context) {
				called = true
				rec := latencytrace.FromContext(c.Request.Context())
				if !enabled {
					require.Nil(t, rec)
				} else {
					require.NotNil(t, rec)
					events := rec.Snapshot().Events
					require.Len(t, events, 2)
					require.Equal(t, "api_key_auth_started", events[0].Phase)
					require.Equal(t, "api_key_auth_done", events[1].Phase)
					require.False(t, events[1].Failed)
					// handler 内的等待不得让已经结束的认证阶段继续增长。
					time.Sleep(10 * time.Millisecond)
					require.Equal(t, events, rec.Snapshot().Events)
				}
				c.Status(http.StatusOK)
			})
			req := httptest.NewRequest("POST", "/v1/responses", nil)
			req.Header.Set("Authorization", "Bearer unit-key")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.True(t, called)
		}
	}
}
