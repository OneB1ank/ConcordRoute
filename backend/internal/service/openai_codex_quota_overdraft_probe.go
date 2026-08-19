package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
	"github.com/TokenFlux/TokenRouter/internal/pkg/tlsfingerprint"
)

const (
	CodexQuotaOverdraftProbeExtraKey = "codex_quota_overdraft_probe"

	codexQuotaOverdraftProbePending      = "pending"
	codexQuotaOverdraftProbePassed       = "passed"
	codexQuotaOverdraftProbeFailed       = "failed"
	codexQuotaOverdraftProbeInconclusive = "inconclusive"
	codexQuotaOverdraftProbeRecovered    = "recovered"

	codexQuotaOverdraftProbeAttemptLimit    = 5
	CodexQuotaOverdraftPassedRecheckSeconds = 30
	codexQuotaOverdraftProbeAttemptTimeout  = 20 * time.Second
	codexQuotaOverdraftProbePlanTimeout     = codexQuotaOverdraftProbeAttemptLimit * codexQuotaOverdraftProbeAttemptTimeout
	codexQuotaOverdraftProbeRetryDelay      = time.Minute
	codexQuotaOverdraftProbeBodyLimit       = 256 << 10
	codexQuotaOverdraftPauseSource          = "codex_quota_overdraft"

	codexQuotaOverdraftFallbackModel      = "gpt-5.5"
	codexQuotaOverdraftCompatibilityModel = "gpt-5.4-mini"
)

// CodexQuotaOverdraftProbeState 持久化在 accounts.extra 中，使额度周期状态可跨重启恢复。
type CodexQuotaOverdraftProbeState struct {
	Status             string     `json:"status"`
	QuotaWindow        string     `json:"quota_window"`
	CycleKey           string     `json:"cycle_key"`
	Attempts           int        `json:"attempts"`
	Limit              int        `json:"limit"`
	Model              string     `json:"model,omitempty"`
	ReasonCode         string     `json:"reason_code,omitempty"`
	StartedAt          time.Time  `json:"started_at"`
	TestedAt           *time.Time `json:"tested_at,omitempty"`
	RetryAt            *time.Time `json:"retry_at,omitempty"`
	RecoverAt          *time.Time `json:"recover_at,omitempty"`
	FiveHourRecoverAt  *time.Time `json:"five_hour_recover_at,omitempty"`
	SevenDayRecoverAt  *time.Time `json:"seven_day_recover_at,omitempty"`
	OverdraftStartedAt *time.Time `json:"overdraft_started_at,omitempty"`
	FiveHourStartedAt  *time.Time `json:"five_hour_overdraft_started_at,omitempty"`
	SevenDayStartedAt  *time.Time `json:"seven_day_overdraft_started_at,omitempty"`
	ObservedRateLimit  *time.Time `json:"observed_rate_limit_reset_at,omitempty"`
}

type codexQuotaOverdraftSignal struct {
	Window            string
	CycleKey          string
	RecoverAt         time.Time
	FiveHourRecoverAt *time.Time
	SevenDayRecoverAt *time.Time
}

type codexQuotaOverdraftProbeResult struct {
	Status     string
	ReasonCode string
	StatusCode int
	Model      string
	Headers    http.Header
	Body       []byte
}

type codexQuotaOverdraftProbeClaimer interface {
	ClaimCodexQuotaOverdraftProbe(context.Context, int64, *CodexQuotaOverdraftProbeState) (bool, error)
}

type codexQuotaOverdraftStateUpdater interface {
	UpdateCodexQuotaOverdraftProbeState(context.Context, int64, *CodexQuotaOverdraftProbeState) (bool, error)
}

type codexQuotaOverdraftRateLimitClearer interface {
	ClearCodexQuotaOverdraftRateLimit(context.Context, int64, *time.Time) (bool, error)
}

// CodexQuotaOverdraftCoordinator 管理账号级真实请求探测与额度周期状态。
type CodexQuotaOverdraftCoordinator struct {
	accountRepo         AccountRepository
	httpUpstream        HTTPUpstream
	openAITokenProvider *OpenAITokenProvider
	openAIGateway       *OpenAIGatewayService
	tempUnschedCache    TempUnschedCache
	runtimeBlocker      AccountRuntimeBlocker
	rateLimitService    *RateLimitService
	agentIdentityWS     agentIdentityWSConnectionInvalidator
	agentIdentityTaskMu sync.Mutex
	running             sync.Map
	now                 func() time.Time
	probeAttemptForTest func(context.Context, *Account, string) codexQuotaOverdraftProbeResult
}

func NewCodexQuotaOverdraftCoordinator(
	accountRepo AccountRepository,
	httpUpstream HTTPUpstream,
	openAITokenProvider *OpenAITokenProvider,
	tempUnschedCache TempUnschedCache,
	runtimeBlocker AccountRuntimeBlocker,
	rateLimitService *RateLimitService,
) *CodexQuotaOverdraftCoordinator {
	coordinator := &CodexQuotaOverdraftCoordinator{
		accountRepo:         accountRepo,
		httpUpstream:        httpUpstream,
		openAITokenProvider: openAITokenProvider,
		tempUnschedCache:    tempUnschedCache,
		runtimeBlocker:      runtimeBlocker,
		rateLimitService:    rateLimitService,
		now:                 time.Now,
	}
	coordinator.agentIdentityWS, _ = runtimeBlocker.(agentIdentityWSConnectionInvalidator)
	coordinator.openAIGateway, _ = runtimeBlocker.(*OpenAIGatewayService)
	return coordinator
}

func (c *CodexQuotaOverdraftCoordinator) enabled() bool {
	return c != nil && c.accountRepo != nil && c.httpUpstream != nil
}

// ObserveAccount 在 5h/7d 快照达到 100% 时启动探测，并在额度恢复后关闭旧周期。
func (c *CodexQuotaOverdraftCoordinator) ObserveAccount(account *Account, preferredModel string) {
	if !c.enabled() || !isCodexQuotaOverdraftAccount(account) || account.ID <= 0 {
		return
	}
	accountCopy := cloneCodexQuotaOverdraftAccount(account)
	go c.observeAccount(accountCopy, preferredModel)
}

func (c *CodexQuotaOverdraftCoordinator) observeAccount(account *Account, preferredModel string) {
	if account == nil {
		return
	}
	now := c.currentTime()
	state, _ := codexQuotaOverdraftStateFromAccount(account)
	if state != nil && clearRecoveredCodexQuotaOverdraftWindows(state, account, now) {
		if !c.persistState(account.ID, state) {
			return
		}
		mergeAccountExtra(account, map[string]any{CodexQuotaOverdraftProbeExtraKey: state})
	}
	signal, exhausted := codexQuotaOverdraftSignalFromAccount(account, state, now)
	if !exhausted {
		c.recoverCycle(account, state, now)
		return
	}
	c.startProbe(account, signal, preferredModel, false)
}

// HandleQuota429 intercepts only definite subscription-quota 429s. Ordinary
// transient 429s continue through the existing rate-limit policy unchanged.
func (c *CodexQuotaOverdraftCoordinator) HandleQuota429(
	ctx context.Context,
	account *Account,
	headers http.Header,
	body []byte,
	preferredModel string,
) bool {
	if !c.enabled() || !isCodexQuotaOverdraftAccount(account) || account.ID <= 0 ||
		!codexQuotaOverdraftResponseIsQuotaLimited(headers, body) {
		return false
	}

	accountCopy := cloneCodexQuotaOverdraftAccount(account)
	if snapshot := ParseCodexRateLimitHeaders(headers); snapshot != nil {
		updates := buildCodexUsageExtraUpdates(snapshot, c.currentTime())
		mergeAccountExtra(accountCopy, updates)
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		_ = c.accountRepo.UpdateExtra(persistCtx, account.ID, updates)
		cancel()
	}
	state, _ := codexQuotaOverdraftStateFromAccount(accountCopy)
	signal, exhausted := codexQuotaOverdraftSignalFromAccount(accountCopy, state, c.currentTime())
	if !exhausted {
		signal = codexQuotaOverdraftFallbackSignal(headers, body, state, c.currentTime())
	}
	if signal.CycleKey == "" {
		return false
	}
	c.startProbe(accountCopy, signal, preferredModel, true)
	return true
}

func (c *CodexQuotaOverdraftCoordinator) startProbe(account *Account, signal codexQuotaOverdraftSignal, preferredModel string, forcePassedRecheck bool) {
	if account == nil || signal.CycleKey == "" {
		return
	}
	current, hasCurrent := codexQuotaOverdraftStateFromAccount(account)
	if hasCurrent && codexQuotaOverdraftStateCoversSignal(current, signal) {
		switch current.Status {
		case codexQuotaOverdraftProbePassed:
			if !codexQuotaOverdraftPassedRecheckDue(current, c.currentTime(), forcePassedRecheck) {
				return
			}
		case codexQuotaOverdraftProbeFailed:
			c.ensureFailedPause(account, current)
			return
		case codexQuotaOverdraftProbePending:
			if c.currentTime().Sub(current.StartedAt) < 2*time.Minute {
				return
			}
		case codexQuotaOverdraftProbeInconclusive:
			if current.RetryAt != nil && c.currentTime().Before(*current.RetryAt) {
				return
			}
		}
	}

	startedAt := c.currentTime().UTC()
	state := &CodexQuotaOverdraftProbeState{
		Status:            codexQuotaOverdraftProbePending,
		QuotaWindow:       signal.Window,
		CycleKey:          signal.CycleKey,
		Limit:             codexQuotaOverdraftProbeAttemptLimit,
		StartedAt:         startedAt,
		RecoverAt:         codexQuotaOverdraftTimePtr(signal.RecoverAt),
		FiveHourRecoverAt: cloneTimePtr(signal.FiveHourRecoverAt),
		SevenDayRecoverAt: cloneTimePtr(signal.SevenDayRecoverAt),
		ObservedRateLimit: cloneTimePtr(account.RateLimitResetAt),
	}
	carryCodexQuotaOverdraftWindowStarts(state, current, signal, startedAt)
	claimed, err := c.claimProbe(account.ID, state)
	if err != nil {
		slog.Warn("codex_quota_overdraft_probe_claim_failed", "account_id", account.ID, "error", err)
		return
	}
	if !claimed {
		return
	}
	mergeAccountExtra(account, map[string]any{CodexQuotaOverdraftProbeExtraKey: state})

	taskKey := fmt.Sprintf("%d:%s", account.ID, signal.CycleKey)
	if _, loaded := c.running.LoadOrStore(taskKey, struct{}{}); loaded {
		return
	}
	go func() {
		defer c.running.Delete(taskKey)
		c.runProbePlan(account.ID, signal, preferredModel, state)
	}()
}

func codexQuotaOverdraftPassedRecheckDue(state *CodexQuotaOverdraftProbeState, now time.Time, forced bool) bool {
	if !forced || state == nil || state.Status != codexQuotaOverdraftProbePassed {
		return false
	}
	if state.TestedAt == nil {
		return true
	}
	return !now.Before(state.TestedAt.Add(time.Duration(CodexQuotaOverdraftPassedRecheckSeconds) * time.Second))
}

func (c *CodexQuotaOverdraftCoordinator) claimProbe(accountID int64, state *CodexQuotaOverdraftProbeState) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if claimer, ok := c.accountRepo.(codexQuotaOverdraftProbeClaimer); ok {
		return claimer.ClaimCodexQuotaOverdraftProbe(ctx, accountID, state)
	}
	if err := c.accountRepo.UpdateExtra(ctx, accountID, map[string]any{CodexQuotaOverdraftProbeExtraKey: state}); err != nil {
		return false, err
	}
	return true, nil
}

func (c *CodexQuotaOverdraftCoordinator) runProbePlan(
	accountID int64,
	signal codexQuotaOverdraftSignal,
	preferredModel string,
	state *CodexQuotaOverdraftProbeState,
) {
	planCtx, cancel := context.WithTimeout(context.Background(), codexQuotaOverdraftProbePlanTimeout)
	defer cancel()

	account, err := c.accountRepo.GetByID(planCtx, accountID)
	if err != nil || !isCodexQuotaOverdraftAccount(account) {
		c.finishInconclusive(accountID, state, "upstream_unavailable")
		return
	}
	models := codexQuotaOverdraftProbeModels(preferredModel)
	lastReason := "invalid_response"
	quotaLimitedAttempts := 0
	for attempt := 0; attempt < codexQuotaOverdraftProbeAttemptLimit; attempt++ {
		if planCtx.Err() != nil {
			c.finishInconclusive(accountID, state, "request_timeout")
			return
		}
		model := models[attempt%len(models)]
		attemptCtx, attemptCancel := context.WithTimeout(planCtx, codexQuotaOverdraftProbeAttemptTimeout)
		result := c.runProbeAttempt(attemptCtx, account, model)
		attemptCancel()
		state.Attempts = attempt + 1
		state.Model = result.Model
		state.ReasonCode = result.ReasonCode
		testedAt := c.currentTime().UTC()
		state.TestedAt = &testedAt
		lastReason = result.ReasonCode
		if result.ReasonCode == "quota_limited" {
			quotaLimitedAttempts++
		}
		if !c.persistState(accountID, state) {
			slog.Info("codex_quota_overdraft_probe_superseded", "account_id", accountID, "cycle_key", state.CycleKey)
			return
		}

		if result.Status == "available" {
			state.Status = codexQuotaOverdraftProbePassed
			state.ReasonCode = "model_response_ok"
			state.RetryAt = nil
			startCodexQuotaOverdraftWindows(state, signal, testedAt)
			if !c.persistState(accountID, state) {
				return
			}
			c.clearQuotaPause(accountID, state)
			slog.Info("codex_quota_overdraft_probe_passed", "account_id", accountID, "attempts", state.Attempts, "model", state.Model, "quota_window", signal.Window)
			return
		}
		if result.Status == "inconclusive" {
			c.finishInconclusive(accountID, state, result.ReasonCode)
			return
		}
		if result.Status == "authentication_failed" {
			if c.rateLimitService != nil {
				c.rateLimitService.HandleUpstreamError(context.Background(), account, result.StatusCode, result.Headers, result.Body)
			}
			c.finishInconclusive(accountID, state, result.ReasonCode)
			return
		}
	}
	if quotaLimitedAttempts < codexQuotaOverdraftProbeAttemptLimit {
		c.finishInconclusive(accountID, state, lastReason)
		return
	}

	state.Status = codexQuotaOverdraftProbeFailed
	state.ReasonCode = lastReason
	state.RetryAt = nil
	if !c.persistState(accountID, state) {
		return
	}
	c.ensureFailedPause(account, state)
	slog.Warn("codex_quota_overdraft_probe_failed", "account_id", accountID, "attempts", state.Attempts, "quota_window", signal.Window, "recover_at", state.RecoverAt)
}

func (c *CodexQuotaOverdraftCoordinator) finishInconclusive(accountID int64, state *CodexQuotaOverdraftProbeState, reason string) {
	now := c.currentTime().UTC()
	retryAt := now.Add(codexQuotaOverdraftProbeRetryDelay)
	state.Status = codexQuotaOverdraftProbeInconclusive
	state.ReasonCode = reason
	state.TestedAt = &now
	state.RetryAt = &retryAt
	c.persistState(accountID, state)
	slog.Warn("codex_quota_overdraft_probe_inconclusive", "account_id", accountID, "attempts", state.Attempts, "reason", reason)
}

func (c *CodexQuotaOverdraftCoordinator) runProbeAttempt(ctx context.Context, account *Account, model string) codexQuotaOverdraftProbeResult {
	if c.probeAttemptForTest != nil {
		return c.probeAttemptForTest(ctx, account, model)
	}
	result := codexQuotaOverdraftProbeResult{Model: model}
	upstreamModel := normalizeOpenAIModelForUpstream(account, account.GetMappedModel(model))
	payload := map[string]any{
		"model": upstreamModel,
		"input": []map[string]any{{
			"type": "message",
			"role": "user",
			"content": []map[string]string{{
				"type": "input_text",
				"text": "hi",
			}},
		}},
		"instructions": "Reply with OK only.",
		"stream":       true,
		"store":        false,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		result.Status, result.ReasonCode = "inconclusive", "experimental_probe_unavailable"
		return result
	}
	payloadBytes, changed, err := injectCodexQuotaOverdraft(payloadBytes)
	if err != nil || !changed {
		result.Status, result.ReasonCode = "inconclusive", "experimental_probe_unavailable"
		return result
	}

	req, err := http.NewRequestWithContext(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileOpenAI), http.MethodPost, chatgptCodexAPIURL, bytes.NewReader(payloadBytes))
	if err != nil {
		result.Status, result.ReasonCode = "inconclusive", "upstream_unavailable"
		return result
	}
	req.Host = "chatgpt.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	identityUA, routerMatch := c.resolveProbeIdentity(account)
	canonical := resolveCodexOutboundIdentity(identityUA)
	req.Header.Set("Originator", canonical.originator)
	req.Header.Set("Version", canonical.version)
	req.Header.Set("User-Agent", canonical.userAgent)

	if account.IsOpenAIAgentIdentity() {
		authHeaders, authErr := buildAgentIdentityAuthenticationHeaders(ctx, c.accountRepo, c.agentIdentityWS, &c.agentIdentityTaskMu, account)
		if authErr != nil {
			result.Status, result.ReasonCode = "inconclusive", "authentication_unavailable"
			return result
		}
		for key, values := range authHeaders {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	} else {
		token := account.GetOpenAIAccessToken()
		if c.openAITokenProvider != nil {
			if refreshed, tokenErr := c.openAITokenProvider.GetAccessToken(ctx, account); tokenErr == nil {
				token = refreshed
			} else if strings.TrimSpace(token) == "" {
				result.Status, result.ReasonCode = "inconclusive", "authentication_unavailable"
				return result
			}
		}
		if strings.TrimSpace(token) == "" {
			result.Status, result.ReasonCode = "inconclusive", "authentication_unavailable"
			return result
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	setOpenAIChatGPTAccountHeaders(req.Header, account)
	enforceCodexIdentityHeadersWithUA(req.Header, identityUA)
	account.ApplyHeaderOverrides(req.Header)

	// 与正常推理共用账号唯一出口；账号已绑定代理但对象缺失时保持
	// fail-closed，避免后台探测静默从服务器公网 IP 直连。
	proxyURL := resolveAccountProxyURL(account)
	var tlsProfile = (*tlsfingerprint.Profile)(nil)
	if c.openAIGateway != nil {
		tlsProfile = c.openAIGateway.resolveOpenAITLSProfile(account, routerMatch)
	}
	resp, err := c.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
	if err != nil {
		result.Status = "inconclusive"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.ReasonCode = "request_timeout"
		} else {
			result.ReasonCode = "upstream_unavailable"
		}
		return result
	}
	defer func() { _ = resp.Body.Close() }()
	result.StatusCode = resp.StatusCode
	result.Headers = resp.Header.Clone()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, codexQuotaOverdraftProbeBodyLimit+1))
	if readErr != nil || len(body) > codexQuotaOverdraftProbeBodyLimit {
		result.Status, result.ReasonCode = "inconclusive", "invalid_response"
		return result
	}
	result.Body = body
	result.Status, result.ReasonCode = classifyCodexQuotaOverdraftProbe(resp.StatusCode, resp.Header, body)
	return result
}

// resolveProbeIdentity 让透支探测复用正常推理的账号 UA、TLS Router 与 TLS Profile 决策链。
func (c *CodexQuotaOverdraftCoordinator) resolveProbeIdentity(account *Account) (string, TLSFingerprintRouterMatchResult) {
	if c != nil && c.openAIGateway != nil {
		return c.openAIGateway.resolveOpenAIBackgroundIdentity(account)
	}
	baseUA := CodexCanonicalUserAgent()
	if account != nil && strings.TrimSpace(account.GetOpenAIUserAgent()) != "" {
		baseUA = strings.TrimSpace(account.GetOpenAIUserAgent())
	}
	return resolveCodexOutboundIdentity(baseUA).userAgent, TLSFingerprintRouterMatchResult{}
}

func classifyCodexQuotaOverdraftProbe(statusCode int, headers http.Header, body []byte) (string, string) {
	if statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices {
		lower := bytes.ToLower(body)
		if bytes.Contains(lower, []byte(`"type":"error"`)) || bytes.Contains(lower, []byte(`"type": "error"`)) ||
			bytes.Contains(lower, []byte(`"response.failed"`)) {
			return "retry", "invalid_response"
		}
		if bytes.Contains(lower, []byte(`"response.completed"`)) || bytes.Contains(lower, []byte(`"response.output_item.done"`)) {
			return "available", "model_response_ok"
		}
		return "retry", "invalid_response"
	}
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication_failed", "authentication_failed"
	case http.StatusPaymentRequired:
		lower := strings.ToLower(string(body))
		if strings.Contains(lower, "deactivated_workspace") || strings.Contains(lower, "account_deactivated") {
			return "authentication_failed", "account_deactivated"
		}
		if codexQuotaOverdraftResponseIsQuotaLimited(headers, body) {
			return "retry", "quota_limited"
		}
		return "retry", "invalid_response"
	case http.StatusTooManyRequests:
		if codexQuotaOverdraftResponseIsQuotaLimited(headers, body) {
			return "retry", "quota_limited"
		}
		return "inconclusive", "transient_failure"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return "inconclusive", "request_timeout"
	default:
		if statusCode >= http.StatusInternalServerError {
			return "inconclusive", "upstream_unavailable"
		}
		if statusCode == http.StatusBadRequest || statusCode == http.StatusNotFound {
			return "inconclusive", "model_not_found"
		}
		return "inconclusive", "invalid_response"
	}
}

func (c *CodexQuotaOverdraftCoordinator) persistState(accountID int64, state *CodexQuotaOverdraftProbeState) bool {
	if c == nil || c.accountRepo == nil || state == nil || accountID <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if updater, ok := c.accountRepo.(codexQuotaOverdraftStateUpdater); ok {
		updated, err := updater.UpdateCodexQuotaOverdraftProbeState(ctx, accountID, state)
		if err != nil {
			slog.Warn("codex_quota_overdraft_state_persist_failed", "account_id", accountID, "cycle_key", state.CycleKey, "error", err)
			return false
		}
		return updated
	}
	if err := c.accountRepo.UpdateExtra(ctx, accountID, map[string]any{CodexQuotaOverdraftProbeExtraKey: state}); err != nil {
		slog.Warn("codex_quota_overdraft_state_persist_failed", "account_id", accountID, "error", err)
		return false
	}
	return true
}

func (c *CodexQuotaOverdraftCoordinator) ensureFailedPause(account *Account, state *CodexQuotaOverdraftProbeState) {
	if account == nil || state == nil || state.RecoverAt == nil || !state.RecoverAt.After(c.currentTime()) {
		return
	}
	reason := BuildTempUnschedReasonPayload(codexQuotaOverdraftPauseSource, "five real overdraft probes confirmed quota exhaustion")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fresh, err := c.accountRepo.GetByID(ctx, account.ID)
	if err != nil || fresh == nil {
		return
	}
	current, ok := codexQuotaOverdraftStateFromAccount(fresh)
	if !ok || current.CycleKey != state.CycleKey || current.Status != codexQuotaOverdraftProbeFailed {
		return
	}
	account = fresh
	if err := c.accountRepo.SetTempUnschedulable(ctx, account.ID, *state.RecoverAt, reason); err != nil {
		slog.Warn("codex_quota_overdraft_pause_failed", "account_id", account.ID, "error", err)
		return
	}
	if c.tempUnschedCache != nil {
		_ = c.tempUnschedCache.SetTempUnsched(ctx, account.ID, &TempUnschedState{
			UntilUnix:       state.RecoverAt.Unix(),
			TriggeredAtUnix: c.currentTime().Unix(),
			StatusCode:      http.StatusTooManyRequests,
			ErrorMessage:    "five real overdraft probes confirmed quota exhaustion",
		})
	}
	if c.runtimeBlocker != nil {
		c.runtimeBlocker.BlockAccountScheduling(account, *state.RecoverAt, codexQuotaOverdraftPauseSource)
	}
}

func (c *CodexQuotaOverdraftCoordinator) clearQuotaPause(accountID int64, state *CodexQuotaOverdraftProbeState) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	account, err := c.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		return
	}
	current, ok := codexQuotaOverdraftStateFromAccount(account)
	if state == nil || !ok || current.CycleKey != state.CycleKey || current.Status != state.Status {
		return
	}
	clearedSchedulingState := false
	runtimeReasonsToClear := make([]string, 0, 2)
	probeProvedAvailable := state != nil &&
		(state.Status == codexQuotaOverdraftProbePassed || state.Status == codexQuotaOverdraftProbeRecovered)
	if probeProvedAvailable && account.RateLimitResetAt != nil && state.ObservedRateLimit != nil {
		staleResetAt := account.RateLimitResetAt.UTC()
		clearer, ok := c.accountRepo.(codexQuotaOverdraftRateLimitClearer)
		if !ok {
			slog.Warn("codex_quota_overdraft_rate_limit_cas_unavailable", "account_id", accountID)
		} else if cleared, err := clearer.ClearCodexQuotaOverdraftRateLimit(ctx, accountID, state.ObservedRateLimit); err != nil {
			slog.Warn("codex_quota_overdraft_stale_rate_limit_clear_failed", "account_id", accountID, "reset_at", staleResetAt, "error", err)
		} else if cleared {
			clearedSchedulingState = true
			runtimeReasonsToClear = append(runtimeReasonsToClear, "429")
			slog.Info("codex_quota_overdraft_stale_rate_limit_cleared", "account_id", accountID, "reset_at", staleResetAt, "probe_status", state.Status)
		}
	}
	thresholdPause := IsAccountSchedulingThresholdReason(account.TempUnschedulableReason)
	overdraftPause := codexQuotaOverdraftPauseReason(account.TempUnschedulableReason)
	if thresholdPause || overdraftPause {
		_ = c.accountRepo.ClearTempUnschedulable(ctx, accountID)
		clearedSchedulingState = true
		if thresholdPause {
			runtimeReasonsToClear = append(runtimeReasonsToClear, "account_scheduling_threshold")
		}
		if overdraftPause {
			runtimeReasonsToClear = append(runtimeReasonsToClear, codexQuotaOverdraftPauseSource)
		}
		if c.tempUnschedCache != nil {
			_ = c.tempUnschedCache.DeleteTempUnsched(ctx, accountID)
		}
	}
	if clearedSchedulingState && c.runtimeBlocker != nil {
		if clearer, ok := c.runtimeBlocker.(accountRuntimeConditionalBlockClearer); ok {
			for _, reason := range runtimeReasonsToClear {
				clearer.ClearAccountSchedulingBlockIfReason(accountID, reason)
			}
		}
	}
}

func (c *CodexQuotaOverdraftCoordinator) recoverCycle(account *Account, state *CodexQuotaOverdraftProbeState, now time.Time) {
	if account == nil || state == nil || state.Status == "" || state.Status == codexQuotaOverdraftProbeRecovered {
		return
	}
	state.Status = codexQuotaOverdraftProbeRecovered
	state.ReasonCode = "quota_recovered"
	state.RetryAt = nil
	state.OverdraftStartedAt = nil
	state.FiveHourStartedAt = nil
	state.SevenDayStartedAt = nil
	testedAt := now.UTC()
	state.TestedAt = &testedAt
	if !c.persistState(account.ID, state) {
		return
	}
	c.clearQuotaPause(account.ID, state)
}

func (c *CodexQuotaOverdraftCoordinator) currentTime() time.Time {
	if c != nil && c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

func codexQuotaOverdraftProbeModels(preferred string) []string {
	models := []string{strings.TrimSpace(preferred), codexQuotaOverdraftFallbackModel, codexQuotaOverdraftCompatibilityModel}
	if models[0] == "" {
		models[0] = openai.CodexUsageProbeModel
	}
	out := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		key := strings.ToLower(strings.TrimSpace(model))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	return out
}

func codexQuotaOverdraftResponseIsQuotaLimited(headers http.Header, body []byte) bool {
	if snapshot := ParseCodexRateLimitHeaders(headers); snapshot != nil {
		if normalized := snapshot.Normalize(); normalized != nil &&
			(normalized.Used5hPercent != nil && *normalized.Used5hPercent >= 100 ||
				normalized.Used7dPercent != nil && *normalized.Used7dPercent >= 100) {
			return true
		}
	}
	text := strings.ToLower(strings.Join(strings.Fields(string(body)), " "))
	for _, marker := range []string{"usage_limit_reached", "usage limit has been reached", "quota exhausted", "weekly limit reached"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func codexQuotaOverdraftSignalFromAccount(account *Account, state *CodexQuotaOverdraftProbeState, now time.Time) (codexQuotaOverdraftSignal, bool) {
	if account == nil || len(account.Extra) == 0 {
		return codexQuotaOverdraftSignal{}, false
	}
	fiveUsed := parseExtraFloat64(account.Extra["codex_5h_used_percent"])
	sevenUsed := parseExtraFloat64(account.Extra["codex_7d_used_percent"])
	fiveReset := codexQuotaOverdraftResetAt(account.Extra["codex_5h_reset_at"], now)
	sevenReset := codexQuotaOverdraftResetAt(account.Extra["codex_7d_reset_at"], now)
	if state != nil {
		fiveReset = stabilizeCodexQuotaOverdraftReset(fiveReset, state.FiveHourRecoverAt, now)
		sevenReset = stabilizeCodexQuotaOverdraftReset(sevenReset, state.SevenDayRecoverAt, now)
	}
	fiveExhausted := fiveUsed >= 100 && (fiveReset == nil || fiveReset.After(now))
	sevenExhausted := sevenUsed >= 100 && (sevenReset == nil || sevenReset.After(now))
	if !fiveExhausted && !sevenExhausted {
		return codexQuotaOverdraftSignal{}, false
	}
	if fiveExhausted && fiveReset == nil {
		fallback := now.Add(5 * time.Hour)
		if state != nil && state.FiveHourRecoverAt != nil && state.FiveHourRecoverAt.After(now) {
			fallback = *state.FiveHourRecoverAt
		}
		fiveReset = &fallback
	}
	if sevenExhausted && sevenReset == nil {
		fallback := now.Add(7 * 24 * time.Hour)
		if state != nil && state.SevenDayRecoverAt != nil && state.SevenDayRecoverAt.After(now) {
			fallback = *state.SevenDayRecoverAt
		}
		sevenReset = &fallback
	}

	signal := codexQuotaOverdraftSignal{FiveHourRecoverAt: fiveReset, SevenDayRecoverAt: sevenReset}
	switch {
	case fiveExhausted && sevenExhausted:
		signal.Window = "multiple"
		signal.CycleKey = fmt.Sprintf("5h:%d|7d:%d", fiveReset.Unix(), sevenReset.Unix())
		signal.RecoverAt = laterTime(*fiveReset, *sevenReset)
	case fiveExhausted:
		signal.Window = "5h"
		signal.CycleKey = fmt.Sprintf("5h:%d", fiveReset.Unix())
		signal.RecoverAt = *fiveReset
		signal.SevenDayRecoverAt = nil
	default:
		signal.Window = "7d"
		signal.CycleKey = fmt.Sprintf("7d:%d", sevenReset.Unix())
		signal.RecoverAt = *sevenReset
		signal.FiveHourRecoverAt = nil
	}
	return signal, true
}

func clearRecoveredCodexQuotaOverdraftWindows(state *CodexQuotaOverdraftProbeState, account *Account, now time.Time) bool {
	if state == nil || account == nil {
		return false
	}
	changed := false
	fiveUsed := parseExtraFloat64(account.Extra["codex_5h_used_percent"])
	sevenUsed := parseExtraFloat64(account.Extra["codex_7d_used_percent"])
	if state.FiveHourStartedAt != nil && (fiveUsed < 100 || state.FiveHourRecoverAt == nil || !state.FiveHourRecoverAt.After(now)) {
		state.FiveHourStartedAt = nil
		state.FiveHourRecoverAt = nil
		changed = true
	}
	if state.SevenDayStartedAt != nil && (sevenUsed < 100 || state.SevenDayRecoverAt == nil || !state.SevenDayRecoverAt.After(now)) {
		state.SevenDayStartedAt = nil
		state.SevenDayRecoverAt = nil
		changed = true
	}
	state.OverdraftStartedAt = earlierCodexQuotaOverdraftStart(state.FiveHourStartedAt, state.SevenDayStartedAt)
	return changed
}

func codexQuotaOverdraftFallbackSignal(headers http.Header, body []byte, state *CodexQuotaOverdraftProbeState, now time.Time) codexQuotaOverdraftSignal {
	recoverAt := now.Add(5 * time.Hour)
	if resetAt := calculateOpenAI429ResetTime(headers); resetAt != nil && resetAt.After(now) {
		recoverAt = *resetAt
	} else if resetUnix := parseOpenAIRateLimitResetTime(body); resetUnix != nil {
		if parsed := time.Unix(*resetUnix, 0).UTC(); parsed.After(now) {
			recoverAt = parsed
		}
	} else if state != nil && state.RecoverAt != nil && state.RecoverAt.After(now) {
		recoverAt = *state.RecoverAt
	}
	return codexQuotaOverdraftSignal{
		Window:            "multiple",
		CycleKey:          fmt.Sprintf("multiple:%d", recoverAt.Unix()),
		RecoverAt:         recoverAt,
		FiveHourRecoverAt: codexQuotaOverdraftTimePtr(recoverAt),
		SevenDayRecoverAt: codexQuotaOverdraftTimePtr(recoverAt),
	}
}

func codexQuotaOverdraftStateFromAccount(account *Account) (*CodexQuotaOverdraftProbeState, bool) {
	if account == nil || account.Extra == nil {
		return nil, false
	}
	raw, ok := account.Extra[CodexQuotaOverdraftProbeExtraKey]
	if !ok || raw == nil {
		return nil, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var state CodexQuotaOverdraftProbeState
	if err := json.Unmarshal(encoded, &state); err != nil || state.CycleKey == "" {
		return nil, false
	}
	return &state, true
}

func codexQuotaOverdraftSchedulingAllowed(account *Account, now time.Time) bool {
	if !isCodexQuotaOverdraftAccount(account) {
		return false
	}
	state, _ := codexQuotaOverdraftStateFromAccount(account)
	signal, exhausted := codexQuotaOverdraftSignalFromAccount(account, state, now)
	if !exhausted || state == nil || !codexQuotaOverdraftStateCoversSignal(state, signal) {
		return true
	}
	return state.Status != codexQuotaOverdraftProbeFailed
}

func codexQuotaOverdraftSnapshotExhausted(updates map[string]any) bool {
	if len(updates) == 0 {
		return false
	}
	return parseExtraFloat64(updates["codex_5h_used_percent"]) >= 100 ||
		parseExtraFloat64(updates["codex_7d_used_percent"]) >= 100
}

func applyCodexQuotaOverdraftUsage(
	ctx context.Context,
	repo UsageLogRepository,
	account *Account,
	usage *UsageInfo,
	now time.Time,
) {
	if repo == nil || account == nil || usage == nil {
		return
	}
	state, ok := codexQuotaOverdraftStateFromAccount(account)
	if !ok || state.Status == codexQuotaOverdraftProbeRecovered {
		return
	}
	apply := func(progress *UsageProgress, startedAt, recoverAt *time.Time) {
		if progress == nil || progress.Utilization < 100 || startedAt == nil || recoverAt == nil || !recoverAt.After(now) {
			return
		}
		stats, err := repo.GetAccountWindowStats(ctx, account.ID, *startedAt)
		if err != nil {
			return
		}
		progress.OverdraftActive = true
		progress.OverdraftStats = windowStatsFromAccountStats(stats)
		progress.OverdraftStarted = cloneTimePtr(startedAt)
		progress.OverdraftRecover = cloneTimePtr(recoverAt)
	}
	fiveStarted, sevenStarted := codexQuotaOverdraftWindowStarts(state)
	apply(usage.FiveHour, fiveStarted, state.FiveHourRecoverAt)
	apply(usage.SevenDay, sevenStarted, state.SevenDayRecoverAt)
}

func stabilizeCodexQuotaOverdraftReset(current, persisted *time.Time, now time.Time) *time.Time {
	if current == nil || persisted == nil || !persisted.After(now) {
		return current
	}
	delta := current.Sub(*persisted)
	if delta < 0 {
		delta = -delta
	}
	if delta <= 2*time.Minute {
		return cloneTimePtr(persisted)
	}
	return current
}

func codexQuotaOverdraftStateCoversSignal(state *CodexQuotaOverdraftProbeState, signal codexQuotaOverdraftSignal) bool {
	if state == nil || signal.CycleKey == "" {
		return false
	}
	if state.CycleKey == signal.CycleKey {
		return true
	}
	resetMatches := func(current, persisted *time.Time) bool {
		if current == nil || persisted == nil {
			return false
		}
		delta := current.Sub(*persisted)
		if delta < 0 {
			delta = -delta
		}
		return delta <= 2*time.Minute
	}
	switch signal.Window {
	case "5h":
		return resetMatches(signal.FiveHourRecoverAt, state.FiveHourRecoverAt)
	case "7d":
		return resetMatches(signal.SevenDayRecoverAt, state.SevenDayRecoverAt)
	case "multiple":
		return resetMatches(signal.FiveHourRecoverAt, state.FiveHourRecoverAt) &&
			resetMatches(signal.SevenDayRecoverAt, state.SevenDayRecoverAt)
	default:
		return false
	}
}

func carryCodexQuotaOverdraftWindowStarts(target, current *CodexQuotaOverdraftProbeState, signal codexQuotaOverdraftSignal, now time.Time) {
	if target == nil || current == nil {
		return
	}
	if current.FiveHourRecoverAt != nil && current.FiveHourRecoverAt.After(now) && signal.FiveHourRecoverAt != nil {
		delta := signal.FiveHourRecoverAt.Sub(*current.FiveHourRecoverAt)
		if delta >= -2*time.Minute && delta <= 2*time.Minute {
			target.FiveHourStartedAt = cloneTimePtr(current.FiveHourStartedAt)
		}
	}
	if current.SevenDayRecoverAt != nil && current.SevenDayRecoverAt.After(now) && signal.SevenDayRecoverAt != nil {
		delta := signal.SevenDayRecoverAt.Sub(*current.SevenDayRecoverAt)
		if delta >= -2*time.Minute && delta <= 2*time.Minute {
			target.SevenDayStartedAt = cloneTimePtr(current.SevenDayStartedAt)
		}
	}
	target.OverdraftStartedAt = earlierCodexQuotaOverdraftStart(target.FiveHourStartedAt, target.SevenDayStartedAt)
}

func startCodexQuotaOverdraftWindows(state *CodexQuotaOverdraftProbeState, signal codexQuotaOverdraftSignal, testedAt time.Time) {
	if state == nil {
		return
	}
	switch signal.Window {
	case "5h":
		if state.FiveHourStartedAt == nil {
			state.FiveHourStartedAt = codexQuotaOverdraftTimePtr(testedAt)
		}
	case "7d":
		if state.SevenDayStartedAt == nil {
			state.SevenDayStartedAt = codexQuotaOverdraftTimePtr(testedAt)
		}
	default:
		if state.FiveHourStartedAt == nil {
			state.FiveHourStartedAt = codexQuotaOverdraftTimePtr(testedAt)
		}
		if state.SevenDayStartedAt == nil {
			state.SevenDayStartedAt = codexQuotaOverdraftTimePtr(testedAt)
		}
	}
	state.OverdraftStartedAt = earlierCodexQuotaOverdraftStart(state.FiveHourStartedAt, state.SevenDayStartedAt)
}

func codexQuotaOverdraftWindowStarts(state *CodexQuotaOverdraftProbeState) (*time.Time, *time.Time) {
	if state == nil {
		return nil, nil
	}
	fiveStarted := cloneTimePtr(state.FiveHourStartedAt)
	sevenStarted := cloneTimePtr(state.SevenDayStartedAt)
	if fiveStarted == nil && sevenStarted == nil && state.OverdraftStartedAt != nil {
		switch state.QuotaWindow {
		case "5h":
			fiveStarted = cloneTimePtr(state.OverdraftStartedAt)
		case "7d":
			sevenStarted = cloneTimePtr(state.OverdraftStartedAt)
		default:
			fiveStarted = cloneTimePtr(state.OverdraftStartedAt)
			sevenStarted = cloneTimePtr(state.OverdraftStartedAt)
		}
	}
	return fiveStarted, sevenStarted
}

func earlierCodexQuotaOverdraftStart(left, right *time.Time) *time.Time {
	switch {
	case left == nil:
		return cloneTimePtr(right)
	case right == nil:
		return cloneTimePtr(left)
	case left.Before(*right):
		return cloneTimePtr(left)
	default:
		return cloneTimePtr(right)
	}
}

func codexQuotaOverdraftPauseReason(reason string) bool {
	payload, ok := parseTempUnschedReasonPayload(reason)
	return ok && payload.Source == codexQuotaOverdraftPauseSource
}

func codexQuotaOverdraftResetAt(raw any, now time.Time) *time.Time {
	if raw == nil {
		return nil
	}
	parsed, err := parseTime(strings.TrimSpace(fmt.Sprint(raw)))
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func cloneCodexQuotaOverdraftAccount(account *Account) *Account {
	if account == nil {
		return nil
	}
	clone := *account
	clone.Extra = shallowCopyMap(account.Extra)
	clone.Credentials = shallowCopyMap(account.Credentials)
	return &clone
}

func laterTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

func codexQuotaOverdraftTimePtr(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}
