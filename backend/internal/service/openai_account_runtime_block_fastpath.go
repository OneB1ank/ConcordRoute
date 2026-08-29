package service

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const (
	openAIAccountStateUpdateTimeout       = 5 * time.Second
	openAIOAuth429FallbackCooldown        = 5 * time.Second
	openAIStopSchedulingBridgeCooldown    = 2 * time.Minute
	openAIOAuth429StormWindow             = 10 * time.Second
	openAIOAuth429StormThreshold          = 20
	openAIOAuth429StormMaxAccountSwitches = 1
)

// OpenAIOAuth429FailoverState 跟踪首次 Grok OAuth 429 后的请求级后续预算。
// 发生该 429 后只允许再尝试一个不同账号，后续账号的任何失败都会终止切换。
type OpenAIOAuth429FailoverState struct {
	grokOAuth429FollowupPending bool
}

func openAIAccountStateContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(base, openAIAccountStateUpdateTimeout)
}

func isOpenAIOAuthAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth
}

func isGrokOAuthAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformGrok && account.Type == AccountTypeOAuth
}

func isOpenAIAccount(account *Account) bool {
	return account != nil && (account.Platform == PlatformOpenAI || account.Platform == PlatformGrok)
}

// isOpenAIContentPolicyRejection 识别只由当前请求内容触发的 OpenAI 安全拒绝。
// 这类错误即使通过流内事件被推断为 502，也不能命中账号级自定义错误策略。
func isOpenAIContentPolicyRejection(responseBody []byte) bool {
	if len(responseBody) == 0 {
		return false
	}
	for _, path := range []string{
		"error.code",
		"error.type",
		"response.error.code",
		"response.error.type",
	} {
		marker := strings.ToLower(strings.TrimSpace(gjson.GetBytes(responseBody, path).String()))
		if strings.Contains(marker, "content_policy") ||
			strings.Contains(marker, "content_filter") ||
			strings.Contains(marker, "safety") ||
			strings.Contains(marker, "moderation") {
			return true
		}
	}
	for _, path := range []string{"error.message", "response.error.message", "message"} {
		message := strings.ToLower(strings.TrimSpace(gjson.GetBytes(responseBody, path).String()))
		if strings.Contains(message, "content policy") ||
			strings.Contains(message, "blocked by policy") ||
			strings.Contains(message, "safety system") ||
			strings.Contains(message, "violates our policies") {
			return true
		}
	}
	return false
}

// isOpenAIAccountPolicyRequestScopedError 识别只与当前请求有关、不能修改账号健康状态的错误。
// 413 请求体限制可能是账号上游代理的独有限制，仍允许切换账号，因此不在这里统一排除。
func isOpenAIAccountPolicyRequestScopedError(account *Account, statusCode int, responseBody []byte) bool {
	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(responseBody))
	if hit, _, _ := detectOpenAICyberPolicy(responseBody); hit {
		return true
	}
	if IsOpenAICyberWarningPayload(responseBody, upstreamMsg) ||
		isOpenAIContentPolicyRejection(responseBody) ||
		isOpenAIClientInvalidRequestError(statusCode, upstreamMsg, responseBody) ||
		isOpenAIContextWindowError(upstreamMsg, responseBody) {
		return true
	}
	return account != nil && account.Platform == PlatformGrok && isGrokContentPolicyRejection(statusCode, responseBody)
}

// handleOpenAIAccountUpstreamError 的 canonicalModel 必须是账号映射恰好应用一次后，
// 实际用于调度的模型。
func (s *OpenAIGatewayService) handleOpenAIAccountUpstreamError(ctx context.Context, account *Account, statusCode int, headers http.Header, responseBody []byte, canonicalModel ...string) bool {
	return s.applyOpenAIAccountUpstreamError(ctx, account, statusCode, headers, responseBody, canonicalModel...).StopScheduling
}

// applyOpenAIAccountUpstreamError 返回完整策略决策，供调用方区分池模式绕过、
// 自定义错误码未命中和真正停止调度的显式策略。
func (s *OpenAIGatewayService) applyOpenAIAccountUpstreamError(ctx context.Context, account *Account, statusCode int, headers http.Header, responseBody []byte, canonicalModel ...string) UpstreamErrorDecision {
	return s.applyOpenAIAccountUpstreamErrorInternal(ctx, account, statusCode, headers, responseBody, false, canonicalModel...)
}

// applyOpenAIAccountStreamRateLimitError 对 HTTP 200 流内限流仅应用显式策略，
// 不使用正常配额快照响应头写入默认的账号级冷却状态。
func (s *OpenAIGatewayService) applyOpenAIAccountStreamRateLimitError(ctx context.Context, account *Account, statusCode int, headers http.Header, responseBody []byte, canonicalModel ...string) UpstreamErrorDecision {
	return s.applyOpenAIAccountUpstreamErrorInternal(ctx, account, statusCode, headers, responseBody, true, canonicalModel...)
}

func (s *OpenAIGatewayService) applyOpenAIAccountUpstreamErrorInternal(
	ctx context.Context,
	account *Account,
	statusCode int,
	headers http.Header,
	responseBody []byte,
	suppressDefaultRateLimitState bool,
	canonicalModel ...string,
) UpstreamErrorDecision {
	if isOpenAIAccountPolicyRequestScopedError(account, statusCode, responseBody) {
		return UpstreamErrorDecision{Policy: ErrorPolicyNone}
	}
	// 任意非 2xx 上游响应都表示模型请求已实际发送。
	if s != nil {
		scheduleOllamaCloudUsageActivity(s.deferredService, account)
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()

	if s == nil || account == nil {
		return UpstreamErrorDecision{Policy: ErrorPolicyNone}
	}
	stateCtx = withTempUnschedulableModel(stateCtx, canonicalModel)
	decision := upstreamErrorDecisionWithoutPersistence(account, statusCode)
	if s.rateLimitService != nil {
		if account.IsPoolMode() || account.IsCustomErrorCodesEnabled() {
			decision.Policy = s.rateLimitService.ApplyExplicitErrorPolicy(stateCtx, account, statusCode, responseBody, canonicalModel...)
			decision.StopScheduling = decision.Policy == ErrorPolicyCustomMatched || decision.Policy == ErrorPolicyTempUnscheduled
		} else {
			decision = UpstreamErrorDecision{Policy: ErrorPolicyNone}
		}
	}
	switch decision.Policy {
	case ErrorPolicyCustomMatched:
		decision.StopScheduling = true
		s.BlockAccountScheduling(account, time.Time{}, "upstream_disable")
		return decision
	case ErrorPolicyTempUnscheduled:
		decision.StopScheduling = true
		return decision
	case ErrorPolicyCustomSkipped, ErrorPolicyPoolBypassed:
		return decision
	}

	if !suppressDefaultRateLimitState && isOpenAIImageRateLimitError(statusCode, responseBody) {
		if s.rateLimitService != nil {
			_ = s.rateLimitService.HandleOpenAIImageRateLimit(stateCtx, account, statusCode, headers, responseBody)
		}
		return decision
	}
	preferredModel := ""
	if len(canonicalModel) > 0 {
		preferredModel = canonicalModel[0]
	}
	if statusCode == http.StatusTooManyRequests && s.rateLimitService != nil &&
		s.rateLimitService.HandleOpenAICodexSparkRateLimit(stateCtx, account, preferredModel, statusCode, headers, responseBody) {
		return decision
	}
	if statusCode == http.StatusTooManyRequests && s.codexQuotaOverdraft != nil &&
		s.codexQuotaOverdraft.HandleQuota429(stateCtx, account, headers, responseBody, preferredModel) {
		// 明确的订阅额度 429 交给账号级 CPA 透支状态机；普通瞬时 429 仍走既有路径。
		return decision
	}
	if s.rateLimitService != nil && len(canonicalModel) > 0 && s.rateLimitService.HandleUpstreamModelNotFound(stateCtx, account, canonicalModel[0], statusCode, responseBody) {
		decision.StopScheduling = true
		return decision
	}
	// 普通账号先保留模型不存在等精确处理，再应用管理员临时规则。
	// 已知模型的规则只暂停账号与模型组合；模型未知时仍同步整号运行时阻断。
	if s.rateLimitService != nil && statusCode != http.StatusUnauthorized &&
		!account.IsPoolMode() && !account.IsCustomErrorCodesEnabled() &&
		s.rateLimitService.HandleTempUnschedulable(stateCtx, account, statusCode, responseBody, canonicalModel...) {
		decision.Policy = ErrorPolicyTempUnscheduled
		decision.StopScheduling = true
		if len(canonicalModel) == 0 || strings.TrimSpace(canonicalModel[0]) == "" {
			s.BlockAccountScheduling(account, time.Time{}, "upstream_disable")
		}
		return decision
	}
	if suppressDefaultRateLimitState && statusCode == http.StatusTooManyRequests {
		return decision
	}
	if statusCode == http.StatusTooManyRequests {
		s.markOpenAIOAuth429RateLimited(stateCtx, account, headers, responseBody)
	}
	if s.rateLimitService == nil {
		return decision
	}
	decision.StopScheduling = s.rateLimitService.handleDefaultUpstreamError(stateCtx, account, statusCode, headers, responseBody)
	modelTempMatched := statusCode != http.StatusUnauthorized && tempUnschedulableModel(stateCtx, nil) != "" &&
		len(matchTempUnschedulableRules(account, statusCode, responseBody)) > 0
	if decision.StopScheduling && !modelTempMatched {
		s.BlockAccountScheduling(account, time.Time{}, "upstream_disable")
	}
	// pool 模式可重试的上游错误已受请求级同账号重试预算约束；若在此记录通用的
	// 账号+模型瞬态冷却，会在预算用完前阻止下一次已获准的重试。
	poolModeRetryable := account.IsPoolMode() && account.IsPoolModeRetryableStatus(statusCode)
	if !decision.StopScheduling && account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey &&
		shouldCooldownOpenAITransientUpstreamError(statusCode, responseBody) && !poolModeRetryable {
		model := ""
		if len(canonicalModel) > 0 {
			model = canonicalModel[0]
		}
		s.recordOpenAICompatibleModelTransientFailure(account, model)
	}
	return decision
}

func shouldCooldownOpenAITransientUpstreamError(statusCode int, responseBody []byte) bool {
	switch statusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 520, 521, 522, 523, 524:
		return true
	case http.StatusBadRequest:
		return isOpenAITransientProcessingError(statusCode, "", responseBody)
	default:
		return false
	}
}

func (s *OpenAIGatewayService) markOpenAIOAuth429RateLimited(ctx context.Context, account *Account, headers http.Header, responseBody []byte) {
	if s == nil || !isOpenAIOAuthAccount(account) {
		return
	}
	// Spark 影子：不按 /responses 429 的 global x-codex-* 信号做内存运行时熔断(同 handle429,外审第8轮 P1)。
	// 同时避免把 spark 的 429 计入全局 429 storm 计数(recordOpenAIOAuth429),否则会误伤母账号 failover 决策。
	if account.IsShadow() {
		return
	}
	s.recordOpenAIOAuth429()

	cooldownUntil := time.Now().Add(openAIOAuth429FallbackCooldown)
	if s.rateLimitService != nil {
		if resetAt := s.rateLimitService.calculateOpenAI429ResetTime(headers); resetAt != nil && resetAt.After(time.Now()) {
			cooldownUntil = *resetAt
		} else if resetUnix := parseOpenAIRateLimitResetTime(responseBody); resetUnix != nil {
			if resetAt := time.Unix(*resetUnix, 0); resetAt.After(time.Now()) {
				cooldownUntil = resetAt
			}
		} else if cooldown, ok := s.rateLimitService.get429FallbackCooldown(ctx, account); ok && cooldown > 0 {
			cooldownUntil = time.Now().Add(cooldown)
		}
	}
	s.BlockAccountScheduling(account, cooldownUntil, "429")
}

func (s *OpenAIGatewayService) BlockAccountScheduling(account *Account, until time.Time, reason string) {
	if s == nil || !isOpenAIAccount(account) {
		return
	}
	mu := s.openAIAccountRuntimeBlockLock(account.ID)
	mu.Lock()
	defer mu.Unlock()
	_, _ = s.blockAccountSchedulingLocked(account, until, reason)
}

func (s *OpenAIGatewayService) openAIAccountRuntimeBlockLock(accountID int64) *sync.Mutex {
	actual, _ := s.openaiAccountRuntimeBlockLocks.LoadOrStore(accountID, &sync.Mutex{})
	mu, ok := actual.(*sync.Mutex)
	if !ok {
		mu = &sync.Mutex{}
		s.openaiAccountRuntimeBlockLocks.Store(accountID, mu)
	}
	return mu
}

func (s *OpenAIGatewayService) blockAccountSchedulingLocked(account *Account, until time.Time, reason string) (uint64, bool) {
	generation := s.openaiAccountRuntimeBlockSequence.Add(1)
	s.openaiAccountRuntimeBlockGeneration.Store(account.ID, generation)
	now := time.Now()
	blockUntil := until
	if blockUntil.IsZero() || !blockUntil.After(now) {
		blockUntil = now.Add(openAIStopSchedulingBridgeCooldown)
	}
	current, loaded := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
	currentUntil, currentOK := current.(time.Time)
	reasons := s.openAIAccountRuntimeBlockReasonsLocked(account.ID)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "unspecified"
	}
	if existing, ok := reasons[reason]; !ok || blockUntil.After(existing) {
		reasons[reason] = blockUntil
	}
	effectiveUntil := latestOpenAIAccountRuntimeBlockUntil(reasons)
	s.openaiAccountRuntimeBlockReasons.Store(account.ID, reasons)
	s.openaiAccountRuntimeBlockUntil.Store(account.ID, effectiveUntil)
	return generation, !loaded || !currentOK || currentUntil.IsZero() || effectiveUntil.After(currentUntil)
}

func (s *OpenAIGatewayService) openAIAccountRuntimeBlockReasonsLocked(accountID int64) map[string]time.Time {
	reasons := make(map[string]time.Time)
	if raw, ok := s.openaiAccountRuntimeBlockReasons.Load(accountID); ok {
		if existing, valid := raw.(map[string]time.Time); valid {
			for reason, until := range existing {
				reasons[reason] = until
			}
		}
	}
	return reasons
}

func latestOpenAIAccountRuntimeBlockUntil(reasons map[string]time.Time) time.Time {
	latest := time.Time{}
	for _, until := range reasons {
		if until.After(latest) {
			latest = until
		}
	}
	return latest
}

func (s *OpenAIGatewayService) ClearAccountSchedulingBlock(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	mu := s.openAIAccountRuntimeBlockLock(accountID)
	mu.Lock()
	defer mu.Unlock()
	s.openaiAccountRuntimeBlockUntil.Delete(accountID)
	s.openaiAccountRuntimeBlockReasons.Delete(accountID)
	s.openaiAccountRuntimeBlockGeneration.Store(accountID, s.openaiAccountRuntimeBlockSequence.Add(1))
}

// ClearAccountSchedulingBlockIfReason 只清除仍由 expectedReason 创建的运行时阻断。
// 调用与写入共用账号锁，避免开启透支时误删并发产生的新认证或限流阻断。
func (s *OpenAIGatewayService) ClearAccountSchedulingBlockIfReason(accountID int64, expectedReason string) bool {
	if s == nil || accountID <= 0 {
		return false
	}
	mu := s.openAIAccountRuntimeBlockLock(accountID)
	mu.Lock()
	defer mu.Unlock()
	reasons := s.openAIAccountRuntimeBlockReasonsLocked(accountID)
	expectedReason = strings.TrimSpace(expectedReason)
	if _, ok := reasons[expectedReason]; !ok {
		return false
	}
	delete(reasons, expectedReason)
	now := time.Now()
	for reason, until := range reasons {
		if !until.After(now) {
			delete(reasons, reason)
		}
	}
	if len(reasons) == 0 {
		s.openaiAccountRuntimeBlockUntil.Delete(accountID)
		s.openaiAccountRuntimeBlockReasons.Delete(accountID)
	} else {
		s.openaiAccountRuntimeBlockReasons.Store(accountID, reasons)
		s.openaiAccountRuntimeBlockUntil.Store(accountID, latestOpenAIAccountRuntimeBlockUntil(reasons))
	}
	s.openaiAccountRuntimeBlockGeneration.Store(accountID, s.openaiAccountRuntimeBlockSequence.Add(1))
	return true
}

func (s *OpenAIGatewayService) isOpenAIAccountRuntimeBlocked(account *Account) bool {
	if s == nil || !isOpenAIAccount(account) {
		return false
	}
	mu := s.openAIAccountRuntimeBlockLock(account.ID)
	mu.Lock()
	defer mu.Unlock()
	value, ok := s.openaiAccountRuntimeBlockUntil.Load(account.ID)
	if !ok {
		return false
	}
	if reasons := s.openAIAccountRuntimeBlockReasonsLocked(account.ID); len(reasons) > 0 {
		now := time.Now()
		for reason, until := range reasons {
			if !until.After(now) {
				delete(reasons, reason)
			}
		}
		if len(reasons) == 0 {
			s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
			s.openaiAccountRuntimeBlockReasons.Delete(account.ID)
			s.openaiAccountRuntimeBlockGeneration.Store(account.ID, s.openaiAccountRuntimeBlockSequence.Add(1))
			return false
		}
		cooldownUntil := latestOpenAIAccountRuntimeBlockUntil(reasons)
		s.openaiAccountRuntimeBlockReasons.Store(account.ID, reasons)
		s.openaiAccountRuntimeBlockUntil.Store(account.ID, cooldownUntil)
		return true
	}
	cooldownUntil, ok := value.(time.Time)
	if !ok || cooldownUntil.IsZero() {
		s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
		s.openaiAccountRuntimeBlockReasons.Delete(account.ID)
		s.openaiAccountRuntimeBlockGeneration.Store(account.ID, s.openaiAccountRuntimeBlockSequence.Add(1))
		return false
	}
	if time.Now().Before(cooldownUntil) {
		return true
	}
	s.openaiAccountRuntimeBlockUntil.Delete(account.ID)
	s.openaiAccountRuntimeBlockReasons.Delete(account.ID)
	s.openaiAccountRuntimeBlockGeneration.Store(account.ID, s.openaiAccountRuntimeBlockSequence.Add(1))
	return false
}

func (s *OpenAIGatewayService) getOpenAIAccountModelTransientState() *openAIAccountModelTransientState {
	if s == nil {
		return nil
	}
	s.openaiModelTransientOnce.Do(func() {
		if s.openaiModelTransient == nil {
			s.openaiModelTransient = newOpenAIAccountModelTransientState(openAIModelTransientDefaultMax)
		}
	})
	return s.openaiModelTransient
}

func canonicalOpenAIAccountSchedulingModel(account *Account, requestedModel string) string {
	model := strings.TrimSpace(requestedModel)
	if account == nil || model == "" {
		return model
	}
	if mapped := strings.TrimSpace(account.GetMappedModel(model)); mapped != "" {
		model = mapped
	}
	if account.IsOpenAICompatible() {
		return normalizeOpenAIModelForUpstream(account, model)
	}
	return model
}

func openAIAccountModelTransientModel(canonicalModel string) string {
	return normalizeOpenAIAccountModelTransientModel(canonicalModel)
}

func (s *OpenAIGatewayService) recordOpenAIAccountModelTransientFailure(account *Account, canonicalModel string, now time.Time) openAIAccountModelTransientDecision {
	if s == nil || account == nil {
		return openAIAccountModelTransientDecision{}
	}
	state := s.getOpenAIAccountModelTransientState()
	if state == nil {
		return openAIAccountModelTransientDecision{}
	}
	return state.recordFailure(account.ID, openAIAccountModelTransientModel(canonicalModel), now)
}

// recordOpenAICompatibleModelTransientFailure 统一记录 OpenAI 兼容平台的账号与模型瞬态失败，
// 调用方必须传入已经完成账号映射和平台规范化的最终上游模型。
func (s *OpenAIGatewayService) recordOpenAICompatibleModelTransientFailure(account *Account, canonicalModel string) {
	decision := s.recordOpenAIAccountModelTransientFailure(account, canonicalModel, time.Now())
	if decision.FailureStreak == 0 {
		return
	}
	slog.Warn("openai_model_transient_state",
		"account_id", account.ID,
		"platform", account.Platform,
		"model", openAIAccountModelTransientModel(canonicalModel),
		"failure_streak", decision.FailureStreak,
		"cooldown_ms", decision.Cooldown.Milliseconds(),
		"block_scope", "account_model",
	)
}

func (s *OpenAIGatewayService) clearOpenAIAccountModelTransientState(accountID int64, model string) {
	state := s.getOpenAIAccountModelTransientState()
	if state == nil {
		return
	}
	state.recordSuccess(accountID, model)
}

func (s *OpenAIGatewayService) isOpenAIAccountModelRuntimeBlocked(account *Account, requestedModel string) bool {
	if s == nil || account == nil {
		return false
	}
	state := s.getOpenAIAccountModelTransientState()
	if state == nil {
		return false
	}
	canonicalModel := canonicalOpenAIAccountSchedulingModel(account, requestedModel)
	return state.isBlocked(account.ID, openAIAccountModelTransientModel(canonicalModel), time.Now())
}

func (s *OpenAIGatewayService) isOpenAIAccountRequestRuntimeBlocked(_ context.Context, account *Account, requestedModel string) bool {
	if s == nil {
		return false
	}
	return s.isOpenAIAccountRuntimeBlocked(account) ||
		s.isOpenAIAccountModelRuntimeBlocked(account, requestedModel)
}

func (s *OpenAIGatewayService) recordOpenAIOAuth429() {
	if s == nil {
		return
	}
	now := time.Now()
	windowStart := s.openaiOAuth429WindowStartUnixNano.Load()
	if windowStart == 0 || now.Sub(time.Unix(0, windowStart)) >= openAIOAuth429StormWindow {
		if s.openaiOAuth429WindowStartUnixNano.CompareAndSwap(windowStart, now.UnixNano()) {
			s.openaiOAuth429WindowCount.Store(1)
			return
		}
	}
	s.openaiOAuth429WindowCount.Add(1)
}

func (s *OpenAIGatewayService) isOpenAIOAuth429Storm() bool {
	if s == nil {
		return false
	}
	windowStart := s.openaiOAuth429WindowStartUnixNano.Load()
	if windowStart == 0 || time.Since(time.Unix(0, windowStart)) >= openAIOAuth429StormWindow {
		return false
	}
	return s.openaiOAuth429WindowCount.Load() >= openAIOAuth429StormThreshold
}

func (s *OpenAIGatewayService) ShouldStopOpenAIOAuth429Failover(account *Account, statusCode int, failedSwitches int, state *OpenAIOAuth429FailoverState) bool {
	if failedSwitches < openAIOAuth429StormMaxAccountSwitches {
		return false
	}
	if state != nil && state.grokOAuth429FollowupPending {
		// 后续预算由 Grok OAuth 429 激活；任一后续账号失败都要消耗预算，
		// 即使混合池下一次选中了 API-key 账号。
		return true
	}
	if isGrokOAuthAccount(account) {
		if state == nil {
			// 尚未采用请求级状态契约的调用方继续沿用旧阈值。
			return statusCode == http.StatusTooManyRequests && failedSwitches >= 2
		}
		if statusCode == http.StatusTooManyRequests {
			state.grokOAuth429FollowupPending = true
		}
		return false
	}
	if statusCode != http.StatusTooManyRequests || !isOpenAIOAuthAccount(account) {
		return false
	}
	return s.isOpenAIOAuth429Storm()
}
