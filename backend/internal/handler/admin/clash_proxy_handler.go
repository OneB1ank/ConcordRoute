package admin

import (
	"context"
	"strconv"

	"github.com/TokenFlux/TokenRouter/internal/clashproxy"
	"github.com/TokenFlux/TokenRouter/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// ClashProxyHandler 提供 Clash 节点、策略、运行时和账号绑定接口。
type ClashProxyHandler struct {
	service *clashproxy.Service
}

func NewClashProxyHandler(service *clashproxy.Service) *ClashProxyHandler {
	return &ClashProxyHandler{service: service}
}

func (h *ClashProxyHandler) ListNodes(c *gin.Context) {
	items, err := h.service.ListNodes(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, items)
}

func (h *ClashProxyHandler) CreateNode(c *gin.Context) {
	var req clashproxy.CreateNodeInput
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	item, err := h.service.CreateManualNode(c.Request.Context(), req)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Created(c, item)
}

func (h *ClashProxyHandler) ImportNodes(c *gin.Context) {
	var req clashproxy.ImportNodesInput
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	items, err := h.service.ImportNodes(c.Request.Context(), req)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, items)
}

func (h *ClashProxyHandler) DeleteNode(c *gin.Context) {
	id, ok := clashProxyID(c, "node")
	if !ok {
		return
	}
	if err := h.service.DeleteNode(c.Request.Context(), id); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, gin.H{"deleted": true})
}

func (h *ClashProxyHandler) ListProfiles(c *gin.Context) {
	items, err := h.service.ListProfiles(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, items)
}

func (h *ClashProxyHandler) CreateProfile(c *gin.Context) {
	var req clashproxy.CreateProfileInput
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	item, err := h.service.CreateProfile(c.Request.Context(), req)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Created(c, item)
}

func (h *ClashProxyHandler) UpdateProfile(c *gin.Context) {
	id, ok := clashProxyID(c, "profile")
	if !ok {
		return
	}
	var req clashproxy.UpdateProfileInput
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	item, err := h.service.UpdateProfile(c.Request.Context(), id, req)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, item)
}

func (h *ClashProxyHandler) StartProfile(c *gin.Context) {
	h.profileRuntimeAction(c, h.service.StartProfile)
}

func (h *ClashProxyHandler) StopProfile(c *gin.Context) {
	h.profileRuntimeAction(c, h.service.StopProfile)
}

func (h *ClashProxyHandler) RestartProfile(c *gin.Context) {
	h.profileRuntimeAction(c, h.service.RestartProfile)
}

func (h *ClashProxyHandler) TestProfile(c *gin.Context) {
	id, ok := clashProxyID(c, "profile")
	if !ok {
		return
	}
	item, err := h.service.TestProfile(c.Request.Context(), id)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, item)
}

func (h *ClashProxyHandler) GetProfileRuntime(c *gin.Context) {
	id, ok := clashProxyID(c, "profile")
	if !ok {
		return
	}
	item, err := h.service.GetRuntime(c.Request.Context(), id)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, item)
}

func (h *ClashProxyHandler) GetRuntimeStatus(c *gin.Context) {
	item, err := h.service.GetRuntimeStatus(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *ClashProxyHandler) ListBindings(c *gin.Context) {
	items, err := h.service.ListBindings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, items)
}

func (h *ClashProxyHandler) CreateBinding(c *gin.Context) {
	var req clashproxy.CreateBindingInput
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	item, err := h.service.CreateBinding(c.Request.Context(), req)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Created(c, item)
}

func (h *ClashProxyHandler) DeleteBinding(c *gin.Context) {
	id, ok := clashProxyID(c, "binding")
	if !ok {
		return
	}
	if err := h.service.DeleteBinding(c.Request.Context(), id); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, gin.H{"deleted": true})
}

func (h *ClashProxyHandler) profileRuntimeAction(c *gin.Context, action func(context.Context, int64) (*clashproxy.RuntimeView, error)) {
	id, ok := clashProxyID(c, "profile")
	if !ok {
		return
	}
	item, err := action(c, id)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, item)
}

func clashProxyID(c *gin.Context, label string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid "+label+" ID")
		return 0, false
	}
	return id, true
}
