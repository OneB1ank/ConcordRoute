package middleware

import (
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/ctxkey"
	"github.com/TokenFlux/TokenRouter/internal/pkg/ip"
	"github.com/TokenFlux/TokenRouter/internal/pkg/logger"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Logger 请求日志中间件
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 开始时间
		startTime := time.Now()

		// 请求路径
		path := c.Request.URL.Path

		// 处理请求
		c.Next()

		// 跳过健康检查等高频探针路径的日志
		if path == "/health" || path == "/setup/status" {
			return
		}

		endTime := time.Now()
		latency := endTime.Sub(startTime)

		method := c.Request.Method
		statusCode := c.Writer.Status()
		clientIP := ip.GetClientIP(c)
		protocol := c.Request.Proto
		accountID, hasAccountID := c.Request.Context().Value(ctxkey.AccountID).(int64)
		platform, _ := c.Request.Context().Value(ctxkey.Platform).(string)
		model, _ := c.Request.Context().Value(ctxkey.Model).(string)
		reason, rejected := GetIngressRejectReason(c)
		if rejected {
			recordIngressReject(c, reason)
			allowed, droppedSummary := globalIngressRejectAccessSampler.allow(endTime)
			if droppedSummary > 0 {
				logger.FromContext(c.Request.Context()).Info("ingress rejection access logs dropped",
					zap.String("component", "http.access"),
					zap.Uint64("dropped_count", droppedSummary),
					zap.Bool(logger.OpsSystemLogSkipField, true),
				)
			}
			if !allowed {
				return
			}
		}

		// 在构造诊断快照和 With 序列化之前检查级别，避免关闭 Info 后仍支付完整日志成本。
		// 入口拒绝统计已在上方执行；存在 Gin 错误时继续保留允许的 Warn 日志。
		l := logger.FromContext(c.Request.Context())
		if !l.Core().Enabled(zap.InfoLevel) && (len(c.Errors) == 0 || !l.Core().Enabled(zap.WarnLevel)) {
			return
		}

		fields := []zap.Field{
			zap.String("component", "http.access"),
			zap.Int("status_code", statusCode),
			zap.Int64("latency_ms", latency.Milliseconds()),
			zap.String("client_ip", clientIP),
			zap.String("protocol", protocol),
			zap.String("method", method),
			zap.String("path", path),
		}
		if rejected {
			fields = append(fields,
				zap.String("ingress_reject_reason", string(reason)),
				zap.Bool(logger.OpsSystemLogSkipField, true),
			)
		}
		if hasAccountID && accountID > 0 {
			fields = append(fields, zap.Int64("account_id", accountID))
		}
		if platform != "" {
			fields = append(fields, zap.String("platform", platform))
		}
		if model != "" {
			fields = append(fields, zap.String("model", model))
		}
		if stages := service.TTFTStageTimingSnapshot(c, endTime); len(stages) > 0 {
			fields = append(fields, zap.Any("ttft_stages_ms", stages))
		}
		if attempts := service.TTFTAttemptTimingSnapshots(c); len(attempts) > 0 {
			fields = append(fields, zap.Any("ttft_attempts", attempts))
		}
		if operations := service.TTFTRequestOperationSnapshot(c); operations != nil && len(operations.Events) > 0 {
			fields = append(fields, zap.Any("ttft_operations", operations))
		}

		l = l.With(fields...)
		l.Info("http request completed", zap.Time("completed_at", endTime))

		if len(c.Errors) > 0 {
			l.Warn("http request contains gin errors", zap.String("errors", c.Errors.String()))
		}
	}
}
