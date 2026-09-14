package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 经过真实访问日志中间件验证 JSON 结构，避免只测试内存采样却遗漏日志接线。
func TestLoggerTTFTAttemptsAreIsolatedAndOptional(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			sink := initMiddlewareTestLogger(t)
			router := gin.New()
			router.Use(Logger())
			router.GET("/ttft-test", func(c *gin.Context) {
				service.InitTTFTStageTiming(c, enabled)
				first := service.BeginTTFTUpstreamAttempt(c, 11)
				first("stream_completed")
				second := service.BeginTTFTUpstreamAttempt(c, 22)
				second("first_content_received")
				second("first_content_flush_completed")
				second("stream_completed")
				c.Status(http.StatusOK)
			})
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/ttft-test", nil))
			for _, event := range sink.list() {
				if event.Message != "http request completed" {
					continue
				}
				if !enabled {
					require.NotContains(t, event.Fields, "ttft_attempts")
					require.NotContains(t, event.Fields, "ttft_stages_ms")
					return
				}
				raw, err := json.Marshal(event.Fields)
				require.NoError(t, err)
				var decoded struct {
					Attempts []service.TTFTAttemptSnapshot `json:"ttft_attempts"`
					Stages   map[string]int64              `json:"ttft_stages_ms"`
				}
				require.NoError(t, json.Unmarshal(raw, &decoded))
				require.Len(t, decoded.Attempts, 2)
				require.Equal(t, int64(11), decoded.Attempts[0].AccountID)
				require.Equal(t, int64(22), decoded.Attempts[1].AccountID)
				require.NotContains(t, decoded.Attempts[0].StagesMS, "first_content_received")
				require.Contains(t, decoded.Attempts[1].StagesMS, "first_content_received")
				require.Contains(t, decoded.Stages, "first_content_flush_completed")
				return
			}
			t.Fatal("缺少请求完成日志")
		})
	}
}
