package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/audioduration"
	"github.com/TokenFlux/TokenRouter/internal/pkg/ip"
	"github.com/TokenFlux/TokenRouter/internal/pkg/logger"
	middleware2 "github.com/TokenFlux/TokenRouter/internal/server/middleware"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// AudioTranscriptions 提供非流式听写；入口独立鉴权、限流与计费，不复用 Live 开关。
// @project-doc docs/interfaces/audio_transcription.md#transcription_gateway
func (h *OpenAIGatewayHandler) AudioTranscriptions(c *gin.Context) {
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || key == nil {
		h.errorResponse(c, 401, "authentication_error", "Invalid API key")
		return
	}
	if key.Group == nil || key.Group.Platform != service.PlatformOpenAI {
		h.errorResponse(c, 404, "not_found_error", "Audio transcription requires an OpenAI group")
		return
	}
	if !key.Group.AllowAudioTranscription {
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
		h.errorResponse(c, 403, "permission_error", "Audio transcription is disabled for this group")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, 500, "api_error", "User context not found")
		return
	}
	log := requestLogger(c, "handler.openai_gateway.audio_transcriptions")
	if !h.ensureResponsesDependencies(c, log) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	// 音频可能很大，在读入和解码前占用用户槽并复核权益，限制同用户并发内存。
	started := false
	releaseUser, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &started, log)
	if !acquired {
		return
	}
	if releaseUser != nil {
		defer releaseUser()
	}
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if err := h.billingCacheService.CheckBillingEligibility(ctx, key.User, key, key.Group, subscription, service.QuotaPlatform(ctx, key)); err != nil {
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.errorResponse(c, status, code, message)
		return
	}
	if enc := strings.TrimSpace(c.GetHeader("Content-Encoding")); enc != "" && enc != "identity" {
		h.errorResponse(c, 415, "invalid_request_error", "compressed multipart bodies are not supported")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, service.OpenAIAudioTranscriptionMaxBodySize))
	if err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			h.errorResponse(c, 413, "invalid_request_error", "audio upload exceeds 26 MiB")
		} else {
			h.errorResponse(c, 400, "invalid_request_error", "failed to read audio upload")
		}
		return
	}
	desktop := c.Request.URL.Path == "/transcribe" || c.Request.URL.Path == "/backend-api/transcribe"
	parsed, err := service.ParseOpenAIAudioTranscriptionRequest(c.GetHeader("Content-Type"), body, desktop)
	if err != nil {
		h.writeAudioTranscriptionError(c, err)
		return
	}
	if !parsed.ModelProvided {
		// 默认模型在进入处理器前尚不存在，补做一次 Key 级映射；显式模型已由认证中间件处理。
		ctx, parsed.Model = apiKeyModelRedirectContext(ctx, key, parsed.Model)
		c.Request = c.Request.WithContext(ctx)
	}
	setOpsRequestContext(c, parsed.Model, false)
	setOpsEndpointContext(c, "", int16(service.RequestTypeSync))
	mapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(ctx, key.GroupID, parsed.Model)
	if h.gatewayService.AudioTranscriptionModelRestricted(ctx, key.GroupID, nil, parsed.Model) {
		h.errorResponse(c, 403, "permission_error", "transcription model is restricted by this channel")
		return
	}
	// 只审查文本提示；音频与转录正文不进入普通请求快照。
	promptBody, _ := json.Marshal(map[string]any{"model": parsed.Model,
		"messages": []map[string]string{{"role": "user", "content": parsed.Prompt}}})
	if decision := h.checkContentModeration(c, log, key, subject, service.ContentModerationProtocolOpenAIChat, parsed.Model, promptBody); decision != nil && decision.Blocked {
		h.errorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
		return
	}
	// 先计算实际时长，再上传；解码失败不产生上游费用，更不按压缩码率猜测费用。
	parsed.DurationSeconds, err = audioduration.Measure(ctx, parsed.Audio)
	if err != nil {
		if ctx.Err() == context.Canceled {
			return
		}
		status := 422
		if errors.Is(err, audioduration.ErrBusy) || errors.Is(err, audioduration.ErrUnavailable) {
			status = 503
		}
		if errors.Is(err, context.DeadlineExceeded) {
			status = 504
		}
		h.errorResponse(c, status, "audio_duration_error", err.Error())
		return
	}
	// OAuth 的 ASR 别名不在文本模型目录里；渠道准入已单独执行，API Key 仍复核模型白名单。
	excluded := map[int64]struct{}{}
	for attempts := 0; attempts < 8; attempts++ {
		selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(ctx, key.GroupID,
			"", "", "", excluded, service.OpenAIUpstreamTransportHTTPSSE,
			service.OpenAIEndpointCapabilityAudioTranscriptions, false, false, service.PlatformOpenAI)
		if err != nil || selection == nil || selection.Account == nil {
			h.errorResponse(c, 503, "api_error", "No eligible transcription accounts")
			return
		}
		account := selection.Account
		release, acquired := h.acquireResponsesAccountSlot(c, key.GroupID, "", selection, false, &started, log)
		if !acquired {
			return
		}
		if (account.Type == service.AccountTypeAPIKey && !account.IsModelSupported(mapping.MappedModel)) ||
			(account.Type == service.AccountTypeOAuth && parsed.Prompt != "") ||
			h.gatewayService.AudioTranscriptionModelRestricted(ctx, key.GroupID, account, parsed.Model) {
			if release != nil {
				release()
			}
			excluded[account.ID] = struct{}{}
			continue
		}
		setOpsSelectedAccount(c, account.ID, account.Platform)
		result, forwardErr := func() (*service.OpenAIForwardResult, error) {
			if release != nil {
				defer release()
			}
			match := h.gatewayService.MatchOpenAITLSFingerprintRouterForRequest(c, account)
			if err := h.gatewayService.EnforceOpenAIClientPolicyForRequest(ctx, c, account, promptBody, match); err != nil {
				return nil, err
			}
			return h.gatewayService.ForwardAudioTranscription(ctx, c, account, parsed, mapping.MappedModel, match)
		}()
		if forwardErr != nil {
			if !c.Writer.Written() && ctx.Err() != context.Canceled {
				h.writeAudioTranscriptionError(c, forwardErr)
			}
			return
		}
		h.recordAudioTranscriptionUsage(c, key, account, subscription, mapping, parsed.PayloadHash, result)
		return
	}
	h.errorResponse(c, 503, "api_error", "No eligible transcription accounts for these options")
}

func (h *OpenAIGatewayHandler) writeAudioTranscriptionError(c *gin.Context, err error) {
	status, message := service.AudioTranscriptionErrorStatus(err)
	h.errorResponse(c, status, "transcription_error", message)
}

// recordAudioTranscriptionUsage 异步闭包仅保留摘要及计量数据，不持有上传录音。
func (h *OpenAIGatewayHandler) recordAudioTranscriptionUsage(c *gin.Context, key *service.APIKey,
	account *service.Account, subscription *service.UserSubscription, mapping service.ChannelMappingResult,
	hash string, result *service.OpenAIForwardResult,
) {
	if result == nil {
		return
	}
	input := &service.OpenAIRecordUsageInput{
		Result: result, APIKey: key, User: key.User, Account: account, Subscription: subscription,
		InboundEndpoint: GetInboundEndpoint(c), UpstreamEndpoint: result.UpstreamEndpoint,
		UserAgent: c.GetHeader("User-Agent"), IPAddress: ip.GetClientIP(c), RequestPayloadHash: hash,
		APIKeyService: h.apiKeyService, QuotaPlatform: service.QuotaPlatform(c.Request.Context(), key),
		ChannelUsageFields: clientRequestedUsageFields(c, mapping, result.Model, result.UpstreamModel),
	}
	h.submitMandatoryUsageRecordTask(c, func(ctx context.Context) {
		if err := h.gatewayService.RecordUsage(ctx, input); err != nil {
			logger.L().Error("audio_transcriptions.record_usage_failed", zap.Int64("api_key_id", key.ID), zap.Error(err))
		}
	})
}
