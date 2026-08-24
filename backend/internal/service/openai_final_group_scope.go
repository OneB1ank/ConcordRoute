package service

import (
	"context"

	"github.com/TokenFlux/TokenRouter/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
)

// WithOpenAIFinalGroupID attaches the group selected by the current OpenAI
// scheduling attempt.  Later sticky/response binding code must consume this
// value rather than resolving a fallback chain a second time.
func WithOpenAIFinalGroupID(ctx context.Context, groupID int64) context.Context {
	if groupID <= 0 {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.OpenAIFinalGroupID, groupID)
}

// OpenAIFinalGroupIDFromContext returns the scheduler-resolved group scope.
func OpenAIFinalGroupIDFromContext(ctx context.Context) (int64, bool) {
	if ctx == nil {
		return 0, false
	}
	groupID, ok := ctx.Value(ctxkey.OpenAIFinalGroupID).(int64)
	return groupID, ok && groupID > 0
}

// SetOpenAIFinalGroupID updates the request context used by downstream
// forwarding and binding hooks.  Gin contexts are request-local, so this does
// not leak the scope across concurrent requests.
func SetOpenAIFinalGroupID(c *gin.Context, groupID int64) {
	if c == nil || c.Request == nil || groupID <= 0 {
		return
	}
	c.Request = c.Request.WithContext(WithOpenAIFinalGroupID(c.Request.Context(), groupID))
}

// SetOpenAIFinalGroupFromSelection applies the final scope carried by a
// scheduler result.  It is intentionally a no-op for legacy selections that
// did not pass through the OpenAI scheduler.
func SetOpenAIFinalGroupFromSelection(c *gin.Context, selection *AccountSelectionResult, decision OpenAIAccountScheduleDecision) {
	groupID := decision.FinalGroupID
	if groupID <= 0 && selection != nil {
		groupID = selection.FinalGroupID
	}
	SetOpenAIFinalGroupID(c, groupID)
}
