package service

import (
	"context"

	"github.com/TokenFlux/TokenRouter/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
)

// WithOpenAIFinalGroupID 将本次 OpenAI 调度选定的最终分组写入上下文，
// 后续 sticky/response 绑定直接复用该值，避免重复解析回退链。
func WithOpenAIFinalGroupID(ctx context.Context, groupID int64) context.Context {
	if groupID <= 0 {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.OpenAIFinalGroupID, groupID)
}

// OpenAIFinalGroupIDFromContext 返回调度器已解析的最终分组作用域。
func OpenAIFinalGroupIDFromContext(ctx context.Context) (int64, bool) {
	if ctx == nil {
		return 0, false
	}
	groupID, ok := ctx.Value(ctxkey.OpenAIFinalGroupID).(int64)
	return groupID, ok && groupID > 0
}

// SetOpenAIFinalGroupID 更新下游转发与绑定钩子使用的请求上下文。
// Gin 上下文仅属于当前请求，不会把该作用域泄漏到并发请求。
func SetOpenAIFinalGroupID(c *gin.Context, groupID int64) {
	if c == nil || c.Request == nil || groupID <= 0 {
		return
	}
	c.Request = c.Request.WithContext(WithOpenAIFinalGroupID(c.Request.Context(), groupID))
}

// SetOpenAIFinalGroupFromSelection 应用调度结果携带的最终作用域；
// 未经过 OpenAI 调度器的旧选择结果保持原样。
func SetOpenAIFinalGroupFromSelection(c *gin.Context, selection *AccountSelectionResult, decision OpenAIAccountScheduleDecision) {
	groupID := decision.FinalGroupID
	if groupID <= 0 && selection != nil {
		groupID = selection.FinalGroupID
	}
	SetOpenAIFinalGroupID(c, groupID)
}
