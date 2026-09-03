package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/domain"
	middleware2 "github.com/TokenFlux/TokenRouter/internal/server/middleware"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// ResponsesInputTokens 处理 Codex 原生 POST /v1/responses/input_tokens。
// 该入口复用现有模型映射与账号调度，但跳过生成并发、usage 记录和结算管线。
func (h *OpenAIGatewayHandler) ResponsesInputTokens(c *gin.Context) {
	requestStart := time.Now()
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.responses_input_tokens",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}

	// 与真实 Responses 生成保持相同的提示词替换顺序，使本地估算覆盖最终出站内容。
	body = h.gatewayService.ApplyUserPromptReplacement(c.Request.Context(), body, "openai_responses")
	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || strings.TrimSpace(modelResult.String()) == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	reqModel := strings.TrimSpace(modelResult.String())
	reqLog = reqLog.With(zap.String("model", reqModel))
	setOpsRequestContext(c, reqModel, false)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(false, false)))

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai_input_tokens.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.errorResponse(c, status, code, message)
		return
	}

	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
	routingModel := strings.TrimSpace(channelMapping.MappedModel)
	forwardBody := body
	if routingModel == "" {
		routingModel = reqModel
	} else if channelMapping.Mapped {
		forwardBody = h.gatewayService.ReplaceModelInBody(body, routingModel)
	}
	requestPlatform := openAICompatibleRequestPlatform(apiKey)
	sessionHash := h.gatewayService.GenerateSessionHash(c, body)
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())

	failedAccountIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	var lastFailoverErr *service.UpstreamFailoverError
	switchCount := 0
	for {
		routingStart := time.Now()
		selection, _, selectErr := h.gatewayService.SelectAccountWithSchedulerForCapability(
			c.Request.Context(),
			apiKey.GroupID,
			"",
			sessionHash,
			routingModel,
			failedAccountIDs,
			service.OpenAIUpstreamTransportAny,
			service.OpenAIEndpointCapabilityChatCompletions,
			false,
			false,
			requestPlatform,
		)
		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		if selectErr != nil {
			reqLog.Warn("openai_input_tokens.account_select_failed", zap.Error(openAICompatibleSelectionErrorForLog(selectErr, requestPlatform)))
			if lastFailoverErr != nil {
				h.handleFailoverExhausted(c, lastFailoverErr, false)
				return
			}
			if h.handleOpenAISelectionBusinessError(c, selectErr, false) {
				return
			}
			cls := classifyOpenAICompatibleNoAccountErrorFromGin(c, h.gatewayService, apiKey, routingModel, reqModel)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimitedIfNoAvailable(c, selectErr)
			}
			h.errorResponse(c, cls.Status, cls.ErrType, cls.Message)
			return
		}
		if selection == nil || selection.Account == nil {
			if lastFailoverErr != nil {
				h.handleFailoverExhausted(c, lastFailoverErr, false)
				return
			}
			cls := classifyOpenAICompatibleNoAccountErrorFromGin(c, h.gatewayService, apiKey, routingModel, reqModel)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimited(c)
			}
			h.errorResponse(c, cls.Status, cls.ErrType, cls.Message)
			return
		}

		account := selection.Account
		setOpsSelectedAccount(c, account.ID, account.Platform)
		forwardStart := time.Now()
		forwardErr := func() error {
			if selection.Acquired && selection.ReleaseFunc != nil {
				defer selection.ReleaseFunc()
			}
			return h.gatewayService.ForwardResponsesInputTokens(c.Request.Context(), c, account, forwardBody)
		}()
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, time.Since(forwardStart).Milliseconds())
		if forwardErr == nil {
			return
		}

		var failoverErr *service.UpstreamFailoverError
		if !errors.As(forwardErr, &failoverErr) {
			reqLog.Error("openai_input_tokens.forward_failed", zap.Int64("account_id", account.ID), zap.Error(forwardErr))
			return
		}
		if !failoverErr.ShouldRetryNextAccount() {
			h.handleFailoverExhausted(c, failoverErr, false)
			return
		}
		if failoverErr.RetryableOnSameAccount && sameAccountRetryCount[account.ID] < account.GetPoolModeRetryCount() {
			sameAccountRetryCount[account.ID]++
			select {
			case <-c.Request.Context().Done():
				return
			case <-time.After(sameAccountRetryDelay):
			}
			continue
		}
		failedAccountIDs[account.ID] = struct{}{}
		lastFailoverErr = failoverErr
		if switchCount >= h.maxAccountSwitches {
			h.handleFailoverExhausted(c, failoverErr, false)
			return
		}
		switchCount++
	}
}

// GrokCountTokens 在本地处理 Anthropic 兼容的 count_tokens 请求。路由中间件已完成
// API Key 认证和分组解析，因此此处理器不选择账号，也不执行计费检查。
func (h *OpenAIGatewayHandler) GrokCountTokens(c *gin.Context) {
	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.anthropicErrorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	if len(body) == 0 {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	bodyRef := service.NewRequestBodyRef(body)
	parsedReq, err := service.ParseGatewayRequest(bodyRef, domain.PlatformAnthropic)
	if err != nil {
		logRequestBodyParseFailure(requestLogger(c, "handler.openai_gateway.grok_count_tokens"), body, err)
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}
	if parsedReq.Model == "" {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	estimated, err := service.EstimateGrokCountTokens(parsedReq.Body.Bytes())
	if err != nil {
		requestLogger(c, "handler.openai_gateway.grok_count_tokens").Warn("grok_count_tokens.local_estimate_failed", zap.Error(err))
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}

	setOpsRequestContext(c, parsedReq.Model, false)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(false, false)))
	c.JSON(http.StatusOK, gin.H{"input_tokens": estimated})
}

// CountTokens 处理 OpenAI 分组的 Anthropic 兼容 POST /v1/messages/count_tokens。
// 它校验计费并转到 OpenAI token-count bridge，不占用并发槽位，也不记录用量。
func (h *OpenAIGatewayHandler) CountTokens(c *gin.Context) {
	requestStart := time.Now()

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.anthropicErrorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.anthropicErrorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.count_tokens",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)

	if apiKey.Group != nil && !apiKey.Group.AllowsClientProtocol(service.GroupClientProtocolAnthropicMessages) {
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
		h.anthropicErrorResponse(c, http.StatusForbidden, "permission_error",
			"This group does not allow Anthropic Messages requests")
		return
	}

	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.anthropicErrorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	if len(body) == 0 {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	setOpsRequestContext(c, "", false)

	body = h.gatewayService.ApplyUserPromptReplacement(c.Request.Context(), body, "anthropic_messages")
	bodyRef := service.NewRequestBodyRef(body)
	parsedReq, err := service.ParseGatewayRequest(bodyRef, domain.PlatformAnthropic)
	if err != nil {
		logRequestBodyParseFailure(reqLog, body, err)
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}
	body = parsedReq.Body.Bytes()
	if parsedReq.Model == "" {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	reqModel := parsedReq.Model
	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", parsedReq.Stream))

	setOpsRequestContext(c, reqModel, false)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(false, false)))

	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
	channelMappedModel := channelMapping.MappedModel
	if channelMappedModel == "" {
		channelMappedModel = reqModel
	}
	accountLayerModel := resolveOpenAIMessagesAccountLayerModelForRequest(c.Request.Context(), apiKey, channelMappedModel)
	mappedBodyForMessages := newOpenAIModelMappedBodyCache(body, h.gatewayService.ReplaceModelInBody)

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	requestPlatform := openAICompatibleRequestPlatform(apiKey)
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai_count_tokens.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.anthropicErrorResponse(c, status, code, message)
		return
	}

	sessionHash := h.gatewayService.GenerateSessionHash(c, body)
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	forwardBody := mappedBodyForMessages(channelMapping.Mapped, channelMapping.MappedModel)
	failedAccountIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	var lastFailoverErr *service.UpstreamFailoverError
	switchCount := 0

	for {
		routingStart := time.Now()
		selection, _, selectErr := h.gatewayService.SelectAccountWithSchedulerForCapabilityAndRoutingModel(
			c.Request.Context(),
			apiKey.GroupID,
			"",
			sessionHash,
			reqModel,
			accountLayerModel,
			failedAccountIDs,
			service.OpenAIUpstreamTransportAny,
			service.OpenAIEndpointCapabilityChatCompletions,
			false,
			false,
			requestPlatform,
		)
		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		if selectErr != nil {
			reqLog.Warn("openai_count_tokens.account_select_failed", zap.Error(openAICompatibleSelectionErrorForLog(selectErr, requestPlatform)))
			if lastFailoverErr != nil {
				h.handleAnthropicFailoverExhausted(c, lastFailoverErr, false)
				return
			}
			if h.handleOpenAISelectionBusinessError(c, selectErr, false) {
				return
			}
			cls := classifyOpenAICompatibleResolvedRoutingNoAccountErrorFromGin(c, h.gatewayService, apiKey, accountLayerModel, reqModel)
			cls = classifySelectionFailureError(selectErr, cls)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimitedIfNoAvailable(c, selectErr)
			}
			h.anthropicErrorResponse(c, cls.Status, cls.ErrType, cls.Message)
			return
		}
		if selection == nil || selection.Account == nil {
			if lastFailoverErr != nil {
				h.handleAnthropicFailoverExhausted(c, lastFailoverErr, false)
				return
			}
			cls := classifyOpenAICompatibleResolvedRoutingNoAccountErrorFromGin(c, h.gatewayService, apiKey, accountLayerModel, reqModel)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimited(c)
			}
			h.anthropicErrorResponse(c, cls.Status, cls.ErrType, cls.Message)
			return
		}

		account := selection.Account
		setOpsSelectedAccount(c, account.ID, account.Platform)
		forwardStart := time.Now()
		forwardErr := func() error {
			if selection.Acquired && selection.ReleaseFunc != nil {
				defer selection.ReleaseFunc()
			}
			return h.gatewayService.ForwardCountTokensAsAnthropic(c.Request.Context(), c, account, forwardBody, accountLayerModel)
		}()
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, time.Since(forwardStart).Milliseconds())
		if forwardErr == nil {
			return
		}
		var failoverErr *service.UpstreamFailoverError
		if !errors.As(forwardErr, &failoverErr) {
			reqLog.Error("openai_count_tokens.forward_failed", zap.Int64("account_id", account.ID), zap.Error(forwardErr))
			return
		}
		if !failoverErr.ShouldRetryNextAccount() {
			h.handleAnthropicFailoverExhausted(c, failoverErr, false)
			return
		}
		if failoverErr.RetryableOnSameAccount && sameAccountRetryCount[account.ID] < account.GetPoolModeRetryCount() {
			sameAccountRetryCount[account.ID]++
			select {
			case <-c.Request.Context().Done():
				return
			case <-time.After(sameAccountRetryDelay):
			}
			continue
		}
		failedAccountIDs[account.ID] = struct{}{}
		lastFailoverErr = failoverErr
		if switchCount >= h.maxAccountSwitches {
			h.handleAnthropicFailoverExhausted(c, failoverErr, false)
			return
		}
		switchCount++
	}
}
