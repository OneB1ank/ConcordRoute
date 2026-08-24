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
