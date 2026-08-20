//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCodexQuotaOverdraftInjection(t *testing.T) {
	ctx := WithCodexQuotaOverdraftScheduling(context.Background())
	svc := &OpenAIGatewayService{}
	reset := time.Now().UTC().Add(time.Hour)
	oauth := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		CodexQuotaOverdraftEnabledExtraKey: true,
		"codex_5h_used_percent":            100,
		"codex_5h_reset_at":                reset.Format(time.RFC3339),
	}}
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)

	updated := svc.prepareCodexQuotaOverdraftBody(ctx, oauth, false, body)
	require.NotEqual(t, string(body), string(updated))
	require.True(t, codexQuotaOverdraftWasInjected(ctx, oauth.ID))

	var document codexQuotaOverdraftDocument
	require.NoError(t, json.Unmarshal(updated, &document))
	require.Len(t, document.Input, 3)
	var call, output codexQuotaOverdraftInputItem
	require.NoError(t, json.Unmarshal(document.Input[1], &call))
	require.NoError(t, json.Unmarshal(document.Input[2], &output))
	require.Equal(t, "custom_tool_call", call.Type)
	require.Equal(t, "custom_tool_call_output", output.Type)
	require.True(t, strings.HasPrefix(call.CallID, codexQuotaOverdraftCallIDPrefix))
	require.Equal(t, call.CallID, output.CallID)

	again := svc.prepareCodexQuotaOverdraftBody(ctx, oauth, false, updated)
	require.Equal(t, string(updated), string(again), "重复处理不能再次注入")
}

func TestCodexQuotaOverdraftInjectionGuards(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user"}]}`)
	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc := &OpenAIGatewayService{}

	// 开关缺失时默认关闭。
	require.Equal(t, string(body), string(svc.prepareCodexQuotaOverdraftBody(WithCodexQuotaOverdraftScheduling(context.Background()), oauth, false, body)))

	oauth.Extra = map[string]any{CodexQuotaOverdraftEnabledExtraKey: true}
	require.Equal(t, string(body), string(svc.prepareCodexQuotaOverdraftBody(context.Background(), oauth, false, body)), "未标记的端点不能注入")
	require.Equal(t, string(body), string(svc.prepareCodexQuotaOverdraftBody(WithCodexQuotaOverdraftScheduling(context.Background()), oauth, true, body)), "compact 不能注入")
	require.Equal(t, string(body), string(svc.prepareCodexQuotaOverdraftBody(WithCodexQuotaOverdraftScheduling(context.Background()), oauth, false, body)), "仅开启开关、额度未耗尽时不能注入")

	ctx := WithCodexQuotaOverdraftScheduling(context.Background())
	for _, account := range []*Account{
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: int64PtrForCodexQuotaOverdraftTest(1)},
	} {
		require.Equal(t, string(body), string(svc.prepareCodexQuotaOverdraftBody(ctx, account, false, body)))
	}
	reset := time.Now().UTC().Add(time.Hour)
	oauth.Extra["codex_5h_used_percent"] = 100
	oauth.Extra["codex_5h_reset_at"] = reset.Format(time.RFC3339)
	agentIdentity := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{openAIAuthModeCredentialKey: OpenAIAuthModeAgentIdentity}, Extra: map[string]any{
		CodexQuotaOverdraftEnabledExtraKey: true,
		"codex_5h_used_percent":            100,
		"codex_5h_reset_at":                reset.Format(time.RFC3339),
	}}
	require.NotEqual(t, string(body), string(svc.prepareCodexQuotaOverdraftBody(ctx, agentIdentity, false, body)), "Agent Identity 必须支持透支请求注入")

	notUserLast := []byte(`{"input":[{"type":"message","role":"assistant"}]}`)
	require.Equal(t, string(notUserLast), string(svc.prepareCodexQuotaOverdraftBody(ctx, oauth, false, notUserLast)))
	invalid := []byte(`{"input":`)
	require.Equal(t, string(invalid), string(svc.prepareCodexQuotaOverdraftBody(ctx, oauth, false, invalid)))
	oversized := make([]byte, codexQuotaOverdraftMaxBodyBytes+1)
	require.Equal(t, oversized, svc.prepareCodexQuotaOverdraftBody(ctx, oauth, false, oversized))
}

func TestCodexQuotaOverdraftInjectionStartsAtPrearmThreshold(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Minute)
	base := func() *Account {
		return &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
			CodexQuotaOverdraftEnabledExtraKey: true,
		}}
	}

	below := base()
	below.Extra["codex_5h_used_percent"] = codexQuotaOverdraftPrearmPercent - 0.01
	below.Extra["codex_5h_reset_at"] = future.Format(time.RFC3339)
	require.False(t, codexQuotaOverdraftRequestActive(below, now))

	atThreshold := base()
	atThreshold.Extra["codex_5h_used_percent"] = codexQuotaOverdraftPrearmPercent
	atThreshold.Extra["codex_5h_reset_at"] = future.Format(time.RFC3339)
	require.True(t, codexQuotaOverdraftRequestActive(atThreshold, now))

	nearThreshold := base()
	nearThreshold.Extra["codex_5h_used_percent"] = 99.5
	nearThreshold.Extra["codex_5h_reset_at"] = future.Format(time.RFC3339)
	require.True(t, codexQuotaOverdraftRequestActive(nearThreshold, now))

	exhausted := base()
	exhausted.Extra["codex_5h_used_percent"] = 100.0
	exhausted.Extra["codex_5h_reset_at"] = future.Format(time.RFC3339)
	require.True(t, codexQuotaOverdraftRequestActive(exhausted, now))

	expired := base()
	expired.Extra["codex_5h_used_percent"] = 100.0
	expired.Extra["codex_5h_reset_at"] = past.Format(time.RFC3339)
	require.False(t, codexQuotaOverdraftRequestActive(expired, now))

	fallbackPassed := base()
	fallbackPassed.Extra[CodexQuotaOverdraftProbeExtraKey] = CodexQuotaOverdraftProbeState{
		Status:      codexQuotaOverdraftProbePassed,
		QuotaWindow: "multiple",
		CycleKey:    "multiple:" + fmt.Sprint(future.Unix()),
		RecoverAt:   codexQuotaOverdraftTimePtr(future),
	}
	require.True(t, codexQuotaOverdraftRequestActive(fallbackPassed, now), "明确额度 429 建立的 fallback 周期仍应生效")

	fallbackFailed := cloneCodexQuotaOverdraftAccount(fallbackPassed)
	failed, ok := codexQuotaOverdraftStateFromAccount(fallbackFailed)
	require.True(t, ok)
	failed.Status = codexQuotaOverdraftProbeFailed
	fallbackFailed.Extra[CodexQuotaOverdraftProbeExtraKey] = failed
	require.False(t, codexQuotaOverdraftRequestActive(fallbackFailed, now))

	recovered := cloneCodexQuotaOverdraftAccount(exhausted)
	recovered.Extra[CodexQuotaOverdraftProbeExtraKey] = CodexQuotaOverdraftProbeState{
		Status:    codexQuotaOverdraftProbeRecovered,
		CycleKey:  "5h:" + fmt.Sprint(future.Unix()),
		RecoverAt: codexQuotaOverdraftTimePtr(future),
	}
	require.False(t, codexQuotaOverdraftRequestActive(recovered, now), "已恢复状态不能因残留的阈值快照重新注入")
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
	require.False(t, account.IsSchedulableForModelWithContext(quotaCtx, "gpt-5.4"), "真实 429 限流仍必须生效")
	account.RateLimitResetAt = nil
	account.TempUnschedulableUntil = &reset
	account.TempUnschedulableReason = BuildTempUnschedReasonPayload("oauth_401", "unauthorized")
	require.Same(t, account, normalizeCodexQuotaOverdraftAccountForScheduling(quotaCtx, account), "其他临时暂停不能绕过")

	account.TempUnschedulableReason = BuildAccountSchedulingThresholdReason("")
	normalized := normalizeCodexQuotaOverdraftAccountForScheduling(quotaCtx, account)
	require.NotSame(t, account, normalized)
	require.Nil(t, normalized.TempUnschedulableUntil)
	require.Empty(t, normalized.TempUnschedulableReason)
	require.NotNil(t, account.TempUnschedulableUntil, "不能修改缓存或数据库账号原对象")
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
	require.Equal(t, 1, accountRepo.tempCalls, "普通端点仍应持久化阈值暂停")
	require.Empty(t, runtimeBlocker.reasons, "阈值暂停不能误写为所有请求共享的 runtime blocker")
}

func int64PtrForCodexQuotaOverdraftTest(value int64) *int64 {
	return &value
}
