//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/model"
	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type codexOverdraftProbeRepoStub struct {
	AccountRepository
	account         *Account
	states          []*CodexQuotaOverdraftProbeState
	tempPauseCalls  int
	clearTempCalls  int
	clearLimitCalls int
}

type codexOverdraftRuntimeBlockerStub struct {
	clearCalls int
}

type codexOverdraftCASRepoStub struct {
	*codexOverdraftProbeRepoStub
	currentCycle string
	updateCalls  int
}

func (b *codexOverdraftRuntimeBlockerStub) BlockAccountScheduling(*Account, time.Time, string) {}

func (b *codexOverdraftRuntimeBlockerStub) ClearAccountSchedulingBlock(int64) {
	b.clearCalls++
}

func (b *codexOverdraftRuntimeBlockerStub) ClearAccountSchedulingBlockIfReason(int64, string) bool {
	b.clearCalls++
	return true
}

func (r *codexOverdraftCASRepoStub) UpdateCodexQuotaOverdraftProbeState(
	ctx context.Context,
	accountID int64,
	state *CodexQuotaOverdraftProbeState,
) (bool, error) {
	r.updateCalls++
	if state == nil || state.CycleKey != r.currentCycle {
		return false, nil
	}
	return true, r.codexOverdraftProbeRepoStub.UpdateExtra(ctx, accountID, map[string]any{CodexQuotaOverdraftProbeExtraKey: state})
}

func (r *codexOverdraftProbeRepoStub) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func (r *codexOverdraftProbeRepoStub) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if r.account.Extra == nil {
		r.account.Extra = make(map[string]any)
	}
	for key, value := range updates {
		r.account.Extra[key] = value
	}
	if state, ok := updates[CodexQuotaOverdraftProbeExtraKey].(*CodexQuotaOverdraftProbeState); ok {
		clone := *state
		r.states = append(r.states, &clone)
	}
	return nil
}

func (r *codexOverdraftProbeRepoStub) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.tempPauseCalls++
	r.account.TempUnschedulableUntil = codexQuotaOverdraftTimePtr(until)
	r.account.TempUnschedulableReason = reason
	return nil
}

func (r *codexOverdraftProbeRepoStub) ClearTempUnschedulable(context.Context, int64) error {
	r.clearTempCalls++
	r.account.TempUnschedulableUntil = nil
	r.account.TempUnschedulableReason = ""
	return nil
}

func (r *codexOverdraftProbeRepoStub) ClearRateLimit(context.Context, int64) error {
	r.clearLimitCalls++
	r.account.RateLimitResetAt = nil
	return nil
}

func (r *codexOverdraftProbeRepoStub) ClearCodexQuotaOverdraftRateLimit(_ context.Context, _ int64, observed *time.Time) (bool, error) {
	if observed == nil || r.account.RateLimitResetAt == nil || !r.account.RateLimitResetAt.Equal(*observed) {
		return false, nil
	}
	r.clearLimitCalls++
	r.account.RateLimitedAt = nil
	r.account.RateLimitResetAt = nil
	return true, nil
}

func newCodexOverdraftProbeTestAccount(now time.Time) *Account {
	return &Account{
		ID:          77,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			CodexQuotaOverdraftEnabledExtraKey: true,
			"codex_5h_used_percent":            100,
			"codex_5h_reset_at":                now.Add(5 * time.Hour).Format(time.RFC3339),
		},
	}
}

func TestCodexQuotaOverdraftProbeModelsMatchPluginRotation(t *testing.T) {
	defaultModels := codexQuotaOverdraftProbeModels("")
	require.NotEmpty(t, defaultModels)
	require.Equal(t, openai.CodexUsageProbeModel, defaultModels[0])

	models := codexQuotaOverdraftProbeModels("gpt-5.4")
	require.Equal(t, []string{"gpt-5.4", "gpt-5.5", "gpt-5.4-mini"}, models)

	got := make([]string, 0, codexQuotaOverdraftProbeAttemptLimit)
	for attempt := 0; attempt < codexQuotaOverdraftProbeAttemptLimit; attempt++ {
		got = append(got, models[attempt%len(models)])
	}
	require.Equal(t, []string{"gpt-5.4", "gpt-5.5", "gpt-5.4-mini", "gpt-5.4", "gpt-5.5"}, got)
}

func TestCodexQuotaOverdraftPassedRecheckRequiresExplicit429AndCooldown(t *testing.T) {
	now := time.Date(2026, time.August, 19, 10, 0, 0, 0, time.UTC)
	testedAt := now.Add(-time.Duration(CodexQuotaOverdraftPassedRecheckSeconds) * time.Second)
	state := &CodexQuotaOverdraftProbeState{
		Status:   codexQuotaOverdraftProbePassed,
		TestedAt: &testedAt,
	}

	require.False(t, codexQuotaOverdraftPassedRecheckDue(state, now, false), "后台快照观察不能重复探测已通过周期")
	require.True(t, codexQuotaOverdraftPassedRecheckDue(state, now, true), "明确额度 429 在冷却后必须重新确认")

	recent := now.Add(-time.Duration(CodexQuotaOverdraftPassedRecheckSeconds-1) * time.Second)
	state.TestedAt = &recent
	require.False(t, codexQuotaOverdraftPassedRecheckDue(state, now, true), "冷却内的并发 429 不能形成探测风暴")
}

func TestCodexQuotaOverdraftSignalStartsAt995Percent(t *testing.T) {
	now := time.Date(2026, time.August, 19, 10, 0, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour)
	account := newCodexOverdraftProbeTestAccount(now)
	account.Extra["codex_5h_used_percent"] = 99.49

	_, active := codexQuotaOverdraftSignalFromAccount(account, nil, now)
	require.False(t, active)

	account.Extra["codex_5h_used_percent"] = codexQuotaOverdraftStartPercent
	signal, active := codexQuotaOverdraftSignalFromAccount(account, nil, now)
	require.True(t, active)
	require.Equal(t, "5h", signal.Window)
	require.WithinDuration(t, reset, signal.RecoverAt, time.Second)
}

func TestCodexQuotaOverdraftQuotaLimitedHeadersUse995PercentThreshold(t *testing.T) {
	headers := make(http.Header)
	headers.Set("x-codex-primary-used-percent", "99.49")
	require.False(t, codexQuotaOverdraftResponseIsQuotaLimited(headers, nil))

	headers.Set("x-codex-primary-used-percent", "99.5")
	require.True(t, codexQuotaOverdraftResponseIsQuotaLimited(headers, nil))
	require.True(t, codexQuotaOverdraftResponseIsQuotaLimited(nil, []byte(`{"error":{"type":"usage_limit_reached"}}`)))
}

func TestCodexQuotaOverdraftProbeStopsWhenCycleIsSuperseded(t *testing.T) {
	now := time.Date(2026, time.August, 19, 10, 0, 0, 0, time.UTC)
	account := newCodexOverdraftProbeTestAccount(now)
	baseRepo := &codexOverdraftProbeRepoStub{account: account}
	signal, exhausted := codexQuotaOverdraftSignalFromAccount(account, nil, now)
	require.True(t, exhausted)
	repo := &codexOverdraftCASRepoStub{codexOverdraftProbeRepoStub: baseRepo, currentCycle: signal.CycleKey}
	coordinator := &CodexQuotaOverdraftCoordinator{accountRepo: repo, now: func() time.Time { return now }}
	attempts := 0
	coordinator.probeAttemptForTest = func(context.Context, *Account, string) codexQuotaOverdraftProbeResult {
		attempts++
		repo.currentCycle = "7d:newer-cycle"
		return codexQuotaOverdraftProbeResult{Status: "retry", ReasonCode: "quota_limited", StatusCode: http.StatusTooManyRequests, Model: "gpt-5.4"}
	}

	coordinator.runProbePlan(account.ID, signal, "gpt-5.4", newCodexOverdraftPendingState(signal, now))

	require.Equal(t, 1, attempts)
	require.Equal(t, 1, repo.updateCalls)
	require.Zero(t, baseRepo.tempPauseCalls, "旧周期协程不得暂停已进入新周期的账号")
}

func TestCodexQuotaOverdraftSignalsKeepFiveHourAndSevenDayCyclesSeparate(t *testing.T) {
	now := time.Date(2026, time.August, 13, 14, 0, 0, 0, time.UTC)
	fiveReset := now.Add(5 * time.Hour)
	sevenReset := now.Add(6 * 24 * time.Hour)
	account := newCodexOverdraftProbeTestAccount(now)

	five, exhausted := codexQuotaOverdraftSignalFromAccount(account, nil, now)
	require.True(t, exhausted)
	require.Equal(t, "5h", five.Window)
	require.Equal(t, "5h:"+formatCodexOverdraftUnix(fiveReset), five.CycleKey)
	require.WithinDuration(t, fiveReset, five.RecoverAt, time.Second)

	account.Extra["codex_7d_used_percent"] = 100
	account.Extra["codex_7d_reset_at"] = sevenReset.Format(time.RFC3339)
	multiple, exhausted := codexQuotaOverdraftSignalFromAccount(account, nil, now)
	require.True(t, exhausted)
	require.Equal(t, "multiple", multiple.Window)
	require.WithinDuration(t, sevenReset, multiple.RecoverAt, time.Second, "多个额度周期必须等待最晚恢复时间")
	require.Contains(t, multiple.CycleKey, "5h:")
	require.Contains(t, multiple.CycleKey, "|7d:")
}

func TestCodexQuotaOverdraftProbePassesOnAnySuccessfulAttempt(t *testing.T) {
	now := time.Date(2026, time.August, 13, 14, 0, 0, 0, time.UTC)
	account := newCodexOverdraftProbeTestAccount(now)
	repo := &codexOverdraftProbeRepoStub{account: account}
	coordinator := &CodexQuotaOverdraftCoordinator{accountRepo: repo, now: func() time.Time { return now }}
	models := make([]string, 0, 3)
	coordinator.probeAttemptForTest = func(_ context.Context, _ *Account, model string) codexQuotaOverdraftProbeResult {
		models = append(models, model)
		if len(models) == 3 {
			return codexQuotaOverdraftProbeResult{Status: "available", ReasonCode: "model_response_ok", StatusCode: http.StatusOK, Model: model}
		}
		return codexQuotaOverdraftProbeResult{Status: "retry", ReasonCode: "quota_limited", StatusCode: http.StatusTooManyRequests, Model: model}
	}
	signal, exhausted := codexQuotaOverdraftSignalFromAccount(account, nil, now)
	require.True(t, exhausted)
	state := newCodexOverdraftPendingState(signal, now)

	coordinator.runProbePlan(account.ID, signal, "gpt-5.4", state)

	require.Equal(t, codexQuotaOverdraftProbePassed, state.Status)
	require.Equal(t, 3, state.Attempts)
	require.Equal(t, []string{"gpt-5.4", "gpt-5.5", "gpt-5.4-mini"}, models)
	require.NotNil(t, state.FiveHourStartedAt)
	require.Zero(t, repo.tempPauseCalls)
}

func TestCodexQuotaOverdraftProbeRequiresFiveExplicitQuotaFailures(t *testing.T) {
	now := time.Date(2026, time.August, 13, 14, 0, 0, 0, time.UTC)
	account := newCodexOverdraftProbeTestAccount(now)
	repo := &codexOverdraftProbeRepoStub{account: account}
	coordinator := &CodexQuotaOverdraftCoordinator{accountRepo: repo, now: func() time.Time { return now }}
	models := make([]string, 0, codexQuotaOverdraftProbeAttemptLimit)
	coordinator.probeAttemptForTest = func(_ context.Context, _ *Account, model string) codexQuotaOverdraftProbeResult {
		models = append(models, model)
		return codexQuotaOverdraftProbeResult{Status: "retry", ReasonCode: "quota_limited", StatusCode: http.StatusTooManyRequests, Model: model}
	}
	signal, _ := codexQuotaOverdraftSignalFromAccount(account, nil, now)
	state := newCodexOverdraftPendingState(signal, now)

	coordinator.runProbePlan(account.ID, signal, "gpt-5.4", state)

	require.Equal(t, codexQuotaOverdraftProbeFailed, state.Status)
	require.Equal(t, codexQuotaOverdraftProbeAttemptLimit, state.Attempts)
	require.Len(t, models, codexQuotaOverdraftProbeAttemptLimit)
	require.Equal(t, 1, repo.tempPauseCalls)
	require.True(t, codexQuotaOverdraftPauseReason(account.TempUnschedulableReason))
	require.False(t, codexQuotaOverdraftSchedulingAllowed(account, now), "同周期确认失败后不得继续绕过限额")
}

func TestCodexQuotaOverdraftProbeInconclusiveNeverPauses(t *testing.T) {
	now := time.Date(2026, time.August, 13, 14, 0, 0, 0, time.UTC)
	for _, result := range []codexQuotaOverdraftProbeResult{
		{Status: "inconclusive", ReasonCode: "request_timeout", StatusCode: http.StatusGatewayTimeout, Model: "gpt-5.4"},
		{Status: "inconclusive", ReasonCode: "upstream_unavailable", StatusCode: http.StatusServiceUnavailable, Model: "gpt-5.4"},
	} {
		t.Run(result.ReasonCode, func(t *testing.T) {
			account := newCodexOverdraftProbeTestAccount(now)
			repo := &codexOverdraftProbeRepoStub{account: account}
			coordinator := &CodexQuotaOverdraftCoordinator{accountRepo: repo, now: func() time.Time { return now }}
			coordinator.probeAttemptForTest = func(context.Context, *Account, string) codexQuotaOverdraftProbeResult { return result }
			signal, _ := codexQuotaOverdraftSignalFromAccount(account, nil, now)
			state := newCodexOverdraftPendingState(signal, now)

			coordinator.runProbePlan(account.ID, signal, "gpt-5.4", state)

			require.Equal(t, codexQuotaOverdraftProbeInconclusive, state.Status)
			require.Equal(t, 1, state.Attempts)
			require.NotNil(t, state.RetryAt)
			require.Zero(t, repo.tempPauseCalls)
		})
	}
}

func TestCodexQuotaOverdraftNewWindowPreservesExistingWindowBaseline(t *testing.T) {
	now := time.Date(2026, time.August, 13, 14, 0, 0, 0, time.UTC)
	fiveStarted := now.Add(-time.Hour)
	fiveRecover := now.Add(4 * time.Hour)
	sevenRecover := now.Add(6 * 24 * time.Hour)
	current := &CodexQuotaOverdraftProbeState{
		Status:            codexQuotaOverdraftProbePassed,
		QuotaWindow:       "5h",
		CycleKey:          "5h:" + formatCodexOverdraftUnix(fiveRecover),
		FiveHourRecoverAt: codexQuotaOverdraftTimePtr(fiveRecover),
		FiveHourStartedAt: codexQuotaOverdraftTimePtr(fiveStarted),
	}
	signal := codexQuotaOverdraftSignal{
		Window:            "multiple",
		CycleKey:          "5h:" + formatCodexOverdraftUnix(fiveRecover) + "|7d:" + formatCodexOverdraftUnix(sevenRecover),
		RecoverAt:         sevenRecover,
		FiveHourRecoverAt: codexQuotaOverdraftTimePtr(fiveRecover),
		SevenDayRecoverAt: codexQuotaOverdraftTimePtr(sevenRecover),
	}
	target := newCodexOverdraftPendingState(signal, now)
	carryCodexQuotaOverdraftWindowStarts(target, current, signal, now)
	startCodexQuotaOverdraftWindows(target, signal, now)

	require.NotNil(t, target.FiveHourStartedAt)
	require.True(t, target.FiveHourStartedAt.Equal(fiveStarted), "已有 5h 透支统计起点不能因 7d 后续耗尽而重置")
	require.NotNil(t, target.SevenDayStartedAt)
	require.True(t, target.SevenDayStartedAt.Equal(now))
}

func TestCodexQuotaOverdraftFailedMultipleCycleStillCoversRemainingWindow(t *testing.T) {
	now := time.Date(2026, time.August, 13, 14, 0, 0, 0, time.UTC)
	fiveRecover := now.Add(5 * time.Hour)
	sevenRecover := now.Add(6 * 24 * time.Hour)
	state := &CodexQuotaOverdraftProbeState{
		Status:            codexQuotaOverdraftProbeFailed,
		QuotaWindow:       "multiple",
		CycleKey:          "5h:" + formatCodexOverdraftUnix(fiveRecover) + "|7d:" + formatCodexOverdraftUnix(sevenRecover),
		FiveHourRecoverAt: codexQuotaOverdraftTimePtr(fiveRecover),
		SevenDayRecoverAt: codexQuotaOverdraftTimePtr(sevenRecover),
	}
	remaining := codexQuotaOverdraftSignal{
		Window:            "7d",
		CycleKey:          "7d:" + formatCodexOverdraftUnix(sevenRecover),
		RecoverAt:         sevenRecover,
		SevenDayRecoverAt: codexQuotaOverdraftTimePtr(sevenRecover),
	}

	require.True(t, codexQuotaOverdraftStateCoversSignal(state, remaining))
}

func TestCodexQuotaOverdraftRecoveryClearsWindowState(t *testing.T) {
	now := time.Date(2026, time.August, 13, 14, 0, 0, 0, time.UTC)
	account := newCodexOverdraftProbeTestAccount(now)
	account.Extra["codex_5h_used_percent"] = 0
	started := now.Add(-time.Hour)
	recoverAt := now.Add(4 * time.Hour)
	state := &CodexQuotaOverdraftProbeState{
		Status:             codexQuotaOverdraftProbePassed,
		QuotaWindow:        "5h",
		CycleKey:           "5h:" + formatCodexOverdraftUnix(recoverAt),
		RecoverAt:          codexQuotaOverdraftTimePtr(recoverAt),
		FiveHourRecoverAt:  codexQuotaOverdraftTimePtr(recoverAt),
		OverdraftStartedAt: codexQuotaOverdraftTimePtr(started),
		FiveHourStartedAt:  codexQuotaOverdraftTimePtr(started),
	}
	account.Extra[CodexQuotaOverdraftProbeExtraKey] = state
	repo := &codexOverdraftProbeRepoStub{account: account}
	coordinator := &CodexQuotaOverdraftCoordinator{accountRepo: repo, now: func() time.Time { return now }}

	coordinator.observeAccount(account, "gpt-5.4")

	recovered, ok := codexQuotaOverdraftStateFromAccount(account)
	require.True(t, ok)
	require.Equal(t, codexQuotaOverdraftProbeRecovered, recovered.Status)
	require.Equal(t, "quota_recovered", recovered.ReasonCode)
	require.Nil(t, recovered.OverdraftStartedAt)
	require.Nil(t, recovered.FiveHourStartedAt)
	require.Nil(t, recovered.FiveHourRecoverAt)
	require.Zero(t, repo.tempPauseCalls)
}

func TestCodexQuotaOverdraftAvailableStateClearsOnlyObservedRateLimitGeneration(t *testing.T) {
	now := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	for _, status := range []string{codexQuotaOverdraftProbePassed, codexQuotaOverdraftProbeRecovered} {
		t.Run(status, func(t *testing.T) {
			account := newCodexOverdraftProbeTestAccount(now)
			account.RateLimitedAt = codexQuotaOverdraftTimePtr(now.Add(-time.Minute))
			account.RateLimitResetAt = codexQuotaOverdraftTimePtr(now.Add(2 * time.Hour))
			repo := &codexOverdraftProbeRepoStub{account: account}
			blocker := &codexOverdraftRuntimeBlockerStub{}
			coordinator := &CodexQuotaOverdraftCoordinator{accountRepo: repo, runtimeBlocker: blocker}
			state := &CodexQuotaOverdraftProbeState{
				Status:            status,
				CycleKey:          "5h:active-cycle",
				ObservedRateLimit: cloneTimePtr(account.RateLimitResetAt),
			}
			account.Extra[CodexQuotaOverdraftProbeExtraKey] = state

			coordinator.clearQuotaPause(account.ID, state)

			require.Equal(t, 1, repo.clearLimitCalls)
			require.Nil(t, account.RateLimitResetAt)
			require.Equal(t, 1, blocker.clearCalls)
		})
	}
}

func TestCodexQuotaOverdraftAvailableStateDoesNotClearNewerRateLimit(t *testing.T) {
	now := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	account := newCodexOverdraftProbeTestAccount(now)
	account.RateLimitResetAt = codexQuotaOverdraftTimePtr(now.Add(2 * time.Hour))
	repo := &codexOverdraftProbeRepoStub{account: account}
	blocker := &codexOverdraftRuntimeBlockerStub{}
	coordinator := &CodexQuotaOverdraftCoordinator{accountRepo: repo, runtimeBlocker: blocker}
	state := &CodexQuotaOverdraftProbeState{
		Status:            codexQuotaOverdraftProbePassed,
		CycleKey:          "5h:active-cycle",
		ObservedRateLimit: codexQuotaOverdraftTimePtr(now.Add(time.Hour)),
	}
	account.Extra[CodexQuotaOverdraftProbeExtraKey] = state

	coordinator.clearQuotaPause(account.ID, state)

	require.Zero(t, repo.clearLimitCalls)
	require.NotNil(t, account.RateLimitResetAt)
	require.Zero(t, blocker.clearCalls)
}

func TestCodexQuotaOverdraftFailedStateDoesNotClearRateLimit(t *testing.T) {
	now := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	account := newCodexOverdraftProbeTestAccount(now)
	account.RateLimitResetAt = codexQuotaOverdraftTimePtr(now.Add(2 * time.Hour))
	repo := &codexOverdraftProbeRepoStub{account: account}
	blocker := &codexOverdraftRuntimeBlockerStub{}
	coordinator := &CodexQuotaOverdraftCoordinator{accountRepo: repo, runtimeBlocker: blocker}
	state := &CodexQuotaOverdraftProbeState{
		Status:            codexQuotaOverdraftProbeFailed,
		CycleKey:          "5h:active-cycle",
		ObservedRateLimit: codexQuotaOverdraftTimePtr(now.Add(time.Hour)),
	}
	account.Extra[CodexQuotaOverdraftProbeExtraKey] = state

	coordinator.clearQuotaPause(account.ID, state)

	require.Zero(t, repo.clearLimitCalls)
	require.NotNil(t, account.RateLimitResetAt)
	require.Zero(t, blocker.clearCalls)
}

func TestCodexQuotaOverdraftClearPausePreservesUnrelatedRuntimeBlock(t *testing.T) {
	now := time.Date(2026, time.August, 19, 11, 0, 0, 0, time.UTC)
	account := newCodexOverdraftProbeTestAccount(now)
	state := &CodexQuotaOverdraftProbeState{
		Status:   codexQuotaOverdraftProbePassed,
		CycleKey: "5h:active-cycle",
	}
	account.Extra[CodexQuotaOverdraftProbeExtraKey] = state
	account.TempUnschedulableUntil = codexQuotaOverdraftTimePtr(time.Now().Add(time.Hour))
	account.TempUnschedulableReason = BuildTempUnschedReasonPayload(codexQuotaOverdraftPauseSource, "quota exhausted")
	repo := &codexOverdraftProbeRepoStub{account: account}
	gateway := &OpenAIGatewayService{}
	gateway.BlockAccountScheduling(account, time.Now().Add(time.Hour), codexQuotaOverdraftPauseSource)
	gateway.BlockAccountScheduling(account, time.Now().Add(2*time.Hour), "oauth_401")
	coordinator := &CodexQuotaOverdraftCoordinator{accountRepo: repo, runtimeBlocker: gateway}

	coordinator.clearQuotaPause(account.ID, state)

	require.Equal(t, 1, repo.clearTempCalls)
	require.True(t, gateway.isOpenAIAccountRuntimeBlocked(account), "恢复透支不得清除独立的认证阻断")
	reasons := gateway.openAIAccountRuntimeBlockReasonsLocked(account.ID)
	require.NotContains(t, reasons, codexQuotaOverdraftPauseSource)
	require.Contains(t, reasons, "oauth_401")
}

func TestClassifyCodexQuotaOverdraftProbeResponses(t *testing.T) {
	status, reason := classifyCodexQuotaOverdraftProbe(http.StatusOK, nil, []byte(`data: {"type":"response.completed"}`))
	require.Equal(t, "available", status)
	require.Equal(t, "model_response_ok", reason)

	status, reason = classifyCodexQuotaOverdraftProbe(http.StatusTooManyRequests, nil, []byte(`{"error":{"type":"usage_limit_reached"}}`))
	require.Equal(t, "retry", status)
	require.Equal(t, "quota_limited", reason)

	status, reason = classifyCodexQuotaOverdraftProbe(http.StatusServiceUnavailable, nil, nil)
	require.Equal(t, "inconclusive", status)
	require.Equal(t, "upstream_unavailable", reason)

	status, reason = classifyCodexQuotaOverdraftProbe(http.StatusUnauthorized, nil, nil)
	require.Equal(t, "authentication_failed", status)
	require.Equal(t, "authentication_failed", reason)

	status, reason = classifyCodexQuotaOverdraftProbe(http.StatusOK, nil, []byte(`data: {"type":"response.failed"}\ndata: {"type":"response.completed"}`))
	require.Equal(t, "retry", status)
	require.Equal(t, "invalid_response", reason)
}

func TestCodexQuotaOverdraftProbePayloadAcceptsInjection(t *testing.T) {
	payload := map[string]any{
		"model": "gpt-5.4",
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
		}},
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	updated, changed, err := injectCodexQuotaOverdraft(raw)
	require.NoError(t, err)
	require.True(t, changed)
	require.Contains(t, string(updated), `"custom_tool_call"`)
}

func TestCodexQuotaOverdraftProbeReusesNormalRequestIdentityTLSAndProxy(t *testing.T) {
	const userAgent = "codex-tui/0.144.1 (Mac OS 15.6; arm64) Apple_Terminal"
	proxyID := int64(18)
	profileService := &TLSFingerprintProfileService{localCache: map[int64]*model.TLSFingerprintProfile{
		77: {ID: 77, Name: "macOS normal request", ALPNProtocols: []string{"h2", "http/1.1"}},
	}}
	match := TLSFingerprintRouterMatchResult{
		Matched:                 true,
		RouterID:                9,
		RuleName:                "macOS",
		TLSFingerprintProfileID: 77,
		UpstreamUserAgent:       userAgent,
	}
	gateway := &OpenAIGatewayService{tlsFPProfileService: profileService}
	account := &Account{
		ID:          909,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 2,
		Credentials: map[string]any{"access_token": "token"},
		Extra: map[string]any{
			CodexQuotaOverdraftEnabledExtraKey: true,
			CodexFingerprintSeedExtraKey:       "7f63a3b2-3209-4a0e-a122-9ff98e909421",
			codexFingerprintModeExtraKey:       string(codexFingerprintCockpit),
			"enable_tls_fingerprint":           true,
			"tls_fingerprint_router_id":        int64(9),
		},
		ProxyID: &proxyID,
		Proxy:   &Proxy{ID: proxyID, Protocol: "http", Host: "proxy.example", Port: 8080},
	}
	gateway.rememberOpenAIOutboundIdentity(account, userAgent, match)
	upstream := &accountUsageHTTPUpstreamStub{}
	coordinator := &CodexQuotaOverdraftCoordinator{httpUpstream: upstream, openAIGateway: gateway}

	_ = coordinator.runProbeAttempt(context.Background(), account, "gpt-5.4")

	require.NotNil(t, upstream.req)
	require.Equal(t, userAgent, upstream.req.Header.Get("User-Agent"))
	identity := resolveCodexOutboundIdentity(userAgent)
	require.Equal(t, identity.originator, upstream.req.Header.Get("Originator"))
	require.Equal(t, identity.version, upstream.req.Header.Get("Version"))
	require.Equal(t, account.Proxy.URL(), upstream.proxyURL)
	require.Equal(t, account.ID, upstream.accountID)
	require.NotNil(t, upstream.tlsProfile)
	require.Equal(t, "macOS normal request", upstream.tlsProfile.Name)

	bodyBytes, err := io.ReadAll(upstream.req.Body)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(bodyBytes, &body))
	metadata, ok := body["client_metadata"].(map[string]any)
	require.True(t, ok)

	installationID := upstream.req.Header.Get("x-codex-installation-id")
	sessionID := upstream.req.Header.Get("session-id")
	threadID := upstream.req.Header.Get("thread-id")
	windowID := upstream.req.Header.Get("x-codex-window-id")
	require.NotEmpty(t, installationID)
	require.NotEmpty(t, sessionID)
	require.NotEmpty(t, threadID)
	require.Equal(t, threadID+":0", windowID)
	require.Equal(t, installationID, metadata["x-codex-installation-id"])
	require.Equal(t, sessionID, metadata["session_id"])
	require.Equal(t, threadID, metadata["thread_id"])
	require.Equal(t, windowID, metadata["x-codex-window-id"])
	require.Equal(t, body["prompt_cache_key"], upstream.req.Header.Get("conversation_id"))
}

func TestCodexQuotaOverdraftProbeKeepsStableConversationAndRotatesTurn(t *testing.T) {
	account := &Account{
		ID:          911,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "token"},
		Extra: map[string]any{
			CodexQuotaOverdraftEnabledExtraKey: true,
			CodexFingerprintSeedExtraKey:       "3d74c106-8ec2-456f-9024-a204e51b6e98",
			codexFingerprintModeExtraKey:       string(codexFingerprintCockpit),
		},
	}

	capture := func() (http.Header, map[string]any) {
		upstream := &accountUsageHTTPUpstreamStub{}
		coordinator := &CodexQuotaOverdraftCoordinator{httpUpstream: upstream}
		_ = coordinator.runProbeAttempt(context.Background(), account, "gpt-5.4")
		require.NotNil(t, upstream.req)
		raw, err := io.ReadAll(upstream.req.Body)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		metadata, ok := body["client_metadata"].(map[string]any)
		require.True(t, ok)
		return upstream.req.Header.Clone(), metadata
	}

	firstHeader, firstMetadata := capture()
	secondHeader, secondMetadata := capture()
	for _, key := range []string{"x-codex-installation-id", "session-id", "thread-id", "x-codex-window-id", "conversation_id"} {
		require.Equal(t, firstHeader.Get(key), secondHeader.Get(key), key)
		require.NotEmpty(t, firstHeader.Get(key), key)
	}
	require.Equal(t, firstMetadata["session_id"], secondMetadata["session_id"])
	require.Equal(t, firstMetadata["thread_id"], secondMetadata["thread_id"])
	require.NotEqual(t, firstMetadata["turn_id"], secondMetadata["turn_id"])

	normalConversation := resolveCodexFingerprintIDs(account, "real-client-session", codexFingerprintCockpit)
	require.NotNil(t, normalConversation)
	require.Equal(t, normalConversation.installationID, firstHeader.Get("x-codex-installation-id"))
	require.Equal(t, normalConversation.sessionID, firstHeader.Get("session-id"))
	require.NotEqual(t, normalConversation.threadID, firstHeader.Get("thread-id"))
}

func TestCodexQuotaOverdraftProbeFailsClosedWhenConfiguredProxyIsUnavailable(t *testing.T) {
	proxyID := int64(19)
	account := &Account{
		ID:          910,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "token"},
		Extra: map[string]any{
			CodexQuotaOverdraftEnabledExtraKey: true,
		},
		ProxyID: &proxyID,
	}
	upstream := &accountUsageHTTPUpstreamStub{}
	coordinator := &CodexQuotaOverdraftCoordinator{httpUpstream: upstream}

	_ = coordinator.runProbeAttempt(context.Background(), account, "gpt-5.4")

	require.Equal(t, unavailableAccountProxyURL, upstream.proxyURL)
}

func newCodexOverdraftPendingState(signal codexQuotaOverdraftSignal, now time.Time) *CodexQuotaOverdraftProbeState {
	return &CodexQuotaOverdraftProbeState{
		Status:             codexQuotaOverdraftProbePending,
		QuotaWindow:        signal.Window,
		CycleKey:           signal.CycleKey,
		Limit:              codexQuotaOverdraftProbeAttemptLimit,
		StartedAt:          now,
		RecoverAt:          codexQuotaOverdraftTimePtr(signal.RecoverAt),
		FiveHourRecoverAt:  cloneTimePtr(signal.FiveHourRecoverAt),
		SevenDayRecoverAt:  cloneTimePtr(signal.SevenDayRecoverAt),
		OverdraftStartedAt: nil,
	}
}

func formatCodexOverdraftUnix(value time.Time) string {
	return strconv.FormatInt(value.Unix(), 10)
}
