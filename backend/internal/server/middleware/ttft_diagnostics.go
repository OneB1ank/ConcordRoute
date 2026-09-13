package middleware

import (
	"net/http"
	"strings"

	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
)

// TTFTDiagnostics enables opt-in request phase sampling for model gateway
// endpoints. It records only elapsed phase timestamps; request bodies,
// authorization values and response payloads are never captured.
func TTFTDiagnostics(enabled bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if enabled && isTTFTGatewayPath(c) {
			service.InitTTFTStageTiming(c, true)
			defer service.MarkTTFTStage(c, "request_completed")
		}
		c.Next()
	}
}

func isTTFTGatewayPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	path := strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	if path == "" {
		return false
	}
	switch {
	case path == "/responses", path == "/v1/responses", path == "/backend-api/codex/responses":
		return true
	case path == "/chat/completions", path == "/v1/chat/completions":
		return true
	case path == "/messages", path == "/v1/messages", path == "/antigravity/v1/messages":
		return true
	case strings.HasSuffix(path, "/responses/compact"), strings.HasSuffix(path, "/responses/input_tokens"):
		return true
	case c.Request.Method == http.MethodGet && strings.Contains(path, "/ws"):
		return true
	default:
		return false
	}
}
