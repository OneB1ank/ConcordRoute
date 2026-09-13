package handler

import (
	"net/http"

	middleware2 "github.com/TokenFlux/TokenRouter/internal/server/middleware"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
)

// CodexAppServerBridge 接收 Codex app-server 的独立 JSON-RPC WebSocket。
// 它与 Responses WebSocket 使用不同路由，避免 response.create 帧和
// attestation/generate 控制消息混用。
func (h *OpenAIGatewayHandler) CodexAppServerBridge(c *gin.Context) {
	if h == nil || h.gatewayService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "app-server bridge is unavailable"}})
		return
	}
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil || apiKey.ID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "Invalid API key"}})
		return
	}
	if apiKey.Group == nil || apiKey.Group.Platform != service.PlatformOpenAI {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "app-server bridge is not supported for this platform"}})
		return
	}
	h.gatewayService.HandleCodexAppServerBridge(c, apiKey.ID)
}
