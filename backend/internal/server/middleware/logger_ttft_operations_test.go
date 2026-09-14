package middleware

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/pkg/latencytrace"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 通过真实访问日志出口验证请求级排队/重试事件不是只停留在内存。
func TestLoggerTTFTOperationsOptionalAndRepeated(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[enabled], func(t *testing.T) {
			sink := initMiddlewareTestLogger(t)
			router := gin.New()
			router.Use(Logger())
			router.GET("/trace-operations", func(c *gin.Context) {
				service.InitTTFTStageTiming(c, enabled)
				for i := 0; i < 2; i++ {
					latencytrace.Start(c.Request.Context(), "account_slot")(nil)
				}
				c.Status(200)
			})
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/trace-operations", nil))
			for _, event := range sink.list() {
				if event.Message != "http request completed" {
					continue
				}
				if !enabled {
					require.NotContains(t, event.Fields, "ttft_operations")
					return
				}
				raw, err := json.Marshal(event.Fields)
				require.NoError(t, err)
				var decoded struct {
					Operations latencytrace.Snapshot `json:"ttft_operations"`
				}
				require.NoError(t, json.Unmarshal(raw, &decoded))
				require.Len(t, decoded.Operations.Events, 4)
				return
			}
			t.Fatal("missing access log")
		})
	}
}
