package admin

import (
	"net/http"

	"github.com/TokenFlux/TokenRouter/internal/pkg/response"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
)

// CodexAttestationCollectorHandler 提供管理员显式启动、创建会话和读取
// app-server 证明摘要的接口。原始 opaque proof 不通过管理 API 返回。
type CodexAttestationCollectorHandler struct {
	collector *service.CodexAppServerAttestationCollector
}

// NewCodexAttestationCollectorHandler 创建采集器管理处理器。
func NewCodexAttestationCollectorHandler(gateway *service.OpenAIGatewayService, collectors ...*service.CodexAppServerAttestationCollector) *CodexAttestationCollectorHandler {
	var collector *service.CodexAppServerAttestationCollector
	if len(collectors) > 0 {
		collector = collectors[0]
	}
	if gateway != nil {
		if collector == nil {
			collector = gateway.CodexAppServerAttestationCollector()
		}
	}
	return &CodexAttestationCollectorHandler{collector: collector}
}

// Status 返回采集器状态。
func (h *CodexAttestationCollectorHandler) Status(c *gin.Context) {
	if h == nil || h.collector == nil {
		response.Error(c, http.StatusServiceUnavailable, "Codex attestation collector is unavailable")
		return
	}
	response.Success(c, h.collector.Status())
}

// Start 开启管理员显式采集。
func (h *CodexAttestationCollectorHandler) Start(c *gin.Context) {
	if h == nil || h.collector == nil {
		response.Error(c, http.StatusServiceUnavailable, "Codex attestation collector is unavailable")
		return
	}
	response.Success(c, h.collector.Start())
}

// Stop 停止采集并清除内存摘要。
func (h *CodexAttestationCollectorHandler) Stop(c *gin.Context) {
	if h == nil || h.collector == nil {
		response.Error(c, http.StatusServiceUnavailable, "Codex attestation collector is unavailable")
		return
	}
	h.collector.Stop()
	response.Success(c, h.collector.Status())
}

// CreateSession 创建一次短期 bridge 采集会话。
func (h *CodexAttestationCollectorHandler) CreateSession(c *gin.Context) {
	if h == nil || h.collector == nil {
		response.Error(c, http.StatusServiceUnavailable, "Codex attestation collector is unavailable")
		return
	}
	session, err := h.collector.CreateSession()
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, session)
}

// ListCaptures 返回会话内的 initialize/generate 摘要。
func (h *CodexAttestationCollectorHandler) ListCaptures(c *gin.Context) {
	if h == nil || h.collector == nil {
		response.Error(c, http.StatusServiceUnavailable, "Codex attestation collector is unavailable")
		return
	}
	records, err := h.collector.ListCaptures(c.Param("token"))
	if err != nil {
		response.NotFound(c, err.Error())
		return
	}
	response.Success(c, records)
}

// DeleteSession 删除会话。
func (h *CodexAttestationCollectorHandler) DeleteSession(c *gin.Context) {
	if h == nil || h.collector == nil {
		response.Error(c, http.StatusServiceUnavailable, "Codex attestation collector is unavailable")
		return
	}
	h.collector.DeleteSession(c.Param("token"))
	response.Success(c, gin.H{"deleted": true})
}
