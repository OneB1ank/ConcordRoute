//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/stretchr/testify/require"
)

func TestWithCodexQuotaOverdraftSchedulingIsIdempotent(t *testing.T) {
	base := context.Background()
	require.False(t, CodexQuotaOverdraftSchedulingEnabled(base))

	first := WithCodexQuotaOverdraftScheduling(base)
	second := WithCodexQuotaOverdraftScheduling(first)

	require.True(t, CodexQuotaOverdraftSchedulingEnabled(first))
	require.Same(t, first, second)
}

func TestCodexQuotaOverdraftSchedulingOnlyBypassesQuotaThresholds(t *testing.T) {
	ctx := WithCodexQuotaOverdraftScheduling(context.Background())
	now := time.Now().UTC()
	reset := now.Add(time.Hour)
	account := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			CodexQuotaOverdraftEnabledExtraKey: true,
			"codex_5h_used_percent":            100,
			"codex_5h_reset_at":                reset.Format(time.RFC3339),
		},
	}
	quotaCtx := withOpenAIQuotaAutoPauseSettings(ctx, OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold5h: 0.8})
	paused, _ := shouldAutoPauseOpenAIAccountByQuota(quotaCtx, account)
	require.False(t, paused)

	account.Extra["codex_5h_used_percent"] = 94.9
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(quotaCtx, account)
	require.True(t, paused, "95% 前仍应遵守本地预留阈值")
	account.Extra["codex_5h_used_percent"] = 95.0
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(quotaCtx, account)
	require.False(t, paused, "95% 准备区开始保留普通业务调用")

	account.RateLimitResetAt = &reset
	require.False(t, account.IsSchedulableForModelWithContext(quotaCtx, "gpt-5.4"), "upstream 429 rate limits remain authoritative")
	account.RateLimitResetAt = nil
	account.OverloadUntil = &reset
	require.False(t, account.IsSchedulableForModelWithContext(quotaCtx, "gpt-5.4"), "upstream overload remains authoritative")
	account.OverloadUntil = nil

	account.TempUnschedulableUntil = &reset
	account.TempUnschedulableReason = BuildTempUnschedReasonPayload("oauth_401", "unauthorized")
	require.Same(t, account, normalizeCodexQuotaOverdraftAccountForScheduling(quotaCtx, account), "non-threshold pauses remain authoritative")
	account.TempUnschedulableReason = BuildTempUnschedReasonPayload("maintenance", "manual pause")
	require.Same(t, account, normalizeCodexQuotaOverdraftAccountForScheduling(quotaCtx, account), "manual pauses remain authoritative")

	account.TempUnschedulableReason = BuildAccountSchedulingThresholdReason("")
	normalized := normalizeCodexQuotaOverdraftAccountForScheduling(quotaCtx, account)
	require.NotSame(t, account, normalized)
	require.Nil(t, normalized.TempUnschedulableUntil)
	require.Empty(t, normalized.TempUnschedulableReason)
	require.NotNil(t, account.TempUnschedulableUntil, "normalization must not mutate the cached account")

	account.TempUnschedulableReason = BuildTempUnschedReasonPayload(codexQuotaOverdraftLegacyPauseSource, "legacy probe pause")
	normalized = normalizeCodexQuotaOverdraftAccountForScheduling(quotaCtx, account)
	require.NotSame(t, account, normalized, "legacy probe pauses must not strand accounts after the probe state machine is removed")
	require.Nil(t, normalized.TempUnschedulableUntil)

	account.TempUnschedulableReason = BuildTempUnschedReasonPayload("oauth_401", "unauthorized")
	require.Same(t, account, normalizeCodexQuotaOverdraftAccountForScheduling(quotaCtx, account), "authentication pauses remain authoritative")
}

func TestCodexQuotaOverdraftPrearmRequiresCurrentTrustedSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "account-a",
		},
		Extra: map[string]any{
			CodexQuotaOverdraftEnabledExtraKey: true,
			"chatgpt_account_id":               "account-a",
			"codex_5h_used_percent":            94.9,
			"codex_5h_reset_at":                reset.Format(time.RFC3339),
		},
	}

	require.False(t, codexQuotaOverdraftPrearmReached(account, now))
	account.Extra["codex_5h_used_percent"] = 95.0
	require.True(t, codexQuotaOverdraftPrearmReached(account, now))
	account.Extra["codex_5h_reset_at"] = now.Add(-time.Second).Format(time.RFC3339)
	require.False(t, codexQuotaOverdraftPrearmReached(account, now), "已重置窗口不得继续透支")
	account.Extra["codex_5h_reset_at"] = reset.Format(time.RFC3339)
	account.Extra["chatgpt_account_id"] = "account-b"
	require.False(t, codexQuotaOverdraftPrearmReached(account, now), "身份不匹配的额度快照不得驱动透支")
}

func TestRateLimitServiceCodexQuotaOverdraftDoesNotCreateRuntimeThresholdBlock(t *testing.T) {
	accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
	accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})

	settingsRepo := newMockSettingRepo()
	settingsRepo.data[SettingKeyAccountSchedulingThresholds] = `{"openai":80}`
	accountRepo := &rateLimitAccountRepoStub{}
	runtimeBlocker := &runtimeBlockRecorder{}
	rl := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	rl.SetSettingService(NewSettingService(settingsRepo, &config.Config{}))
	rl.SetAccountRuntimeBlocker(runtimeBlocker)
	reset := time.Now().UTC().Add(time.Hour)
	account := &Account{
		ID:          9001,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			CodexQuotaOverdraftEnabledExtraKey: true,
			"codex_7d_used_percent":            100,
			"codex_7d_reset_at":                reset.Format(time.RFC3339),
		},
	}

	require.True(t, rl.ApplyAccountSchedulingThreshold(context.Background(), account))
	require.Equal(t, 1, accountRepo.tempCalls, "threshold pauses remain persisted for non-overdraft endpoints")
	require.Empty(t, runtimeBlocker.reasons, "threshold pauses must not become a global runtime block")
}

func TestBuildCodexQuotaOverdraftStateUsesPassive95And98Gates(t *testing.T) {
	now := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	fiveReset := now.Add(2 * time.Hour)
	sevenReset := now.Add(4 * 24 * time.Hour)
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Extra:    map[string]any{CodexQuotaOverdraftEnabledExtraKey: true},
	}
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 94.9, ResetsAt: &fiveReset},
		SevenDay: &UsageProgress{Utilization: 20, ResetsAt: &sevenReset},
	}

	require.Nil(t, buildCodexQuotaOverdraftState(account, usage, now))

	usage.FiveHour.Utilization = 95
	preparing := buildCodexQuotaOverdraftState(account, usage, now)
	require.NotNil(t, preparing)
	require.Equal(t, CodexQuotaOverdraftStatusPreparing, preparing.Status)
	require.Equal(t, "5h", preparing.QuotaWindow)
	require.Equal(t, 95.0, preparing.UsedPercent)
	require.Equal(t, CodexQuotaOverdraftPrearmPercent, preparing.PrearmPercent)
	require.Equal(t, CodexQuotaOverdraftStartPercent, preparing.StartPercent)
	require.Equal(t, fiveReset, *preparing.RecoverAt)

	usage.FiveHour.Utilization = 98
	usage.SevenDay.Utilization = 99
	active := buildCodexQuotaOverdraftState(account, usage, now)
	require.NotNil(t, active)
	require.Equal(t, CodexQuotaOverdraftStatusActive, active.Status)
	require.Equal(t, "multiple", active.QuotaWindow)
	require.Equal(t, 99.0, active.UsedPercent)
	require.Equal(t, sevenReset, *active.RecoverAt)
}

func TestBuildCodexQuotaOverdraftStateIgnoresInactiveOrExpiredWindows(t *testing.T) {
	now := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Second)
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Extra:    map[string]any{CodexQuotaOverdraftEnabledExtraKey: true},
	}
	usage := &UsageInfo{FiveHour: &UsageProgress{Utilization: 99, ResetsAt: &expired}}

	require.Nil(t, buildCodexQuotaOverdraftState(account, usage, now))
	account.Status = StatusDisabled
	usage.FiveHour.ResetsAt = nil
	require.Nil(t, buildCodexQuotaOverdraftState(account, usage, now))
	account.Status = StatusActive
	parentID := int64(1)
	account.ParentAccountID = &parentID
	require.Nil(t, buildCodexQuotaOverdraftState(account, usage, now))
}

func TestBuildCodexQuotaOverdraftStateReal429TerminatesCycle(t *testing.T) {
	now := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	reset := now.Add(90 * time.Minute)
	account := &Account{
		Platform:         PlatformOpenAI,
		Type:             AccountTypeOAuth,
		Status:           StatusActive,
		RateLimitResetAt: &reset,
		Extra:            map[string]any{CodexQuotaOverdraftEnabledExtraKey: true},
	}
	state := buildCodexQuotaOverdraftState(account, &UsageInfo{}, now)

	require.NotNil(t, state)
	require.Equal(t, CodexQuotaOverdraftStatusTerminated, state.Status)
	require.Equal(t, "multiple", state.QuotaWindow)
	require.Equal(t, reset, *state.RecoverAt)
}
