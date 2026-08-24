package service

import (
	"context"
	"time"
)

const (
	// CodexQuotaOverdraftEnabledExtraKey 只启用本地额度策略透支。
	// 它仅绕过本地预留阈值，不绕过上游真实 429。
	CodexQuotaOverdraftEnabledExtraKey = "codex_quota_overdraft_enabled"

	// CodexQuotaOverdraftLegacyProbeExtraKey 仅用于在账号更新和迁移时
	// 清理旧版运行状态。
	CodexQuotaOverdraftLegacyProbeExtraKey = "codex_quota_overdraft_probe"

	codexQuotaOverdraftLegacyPauseSource = "codex_quota_overdraft"
)

func isCodexQuotaOverdraftAccount(account *Account) bool {
	return account != nil &&
		account.Platform == PlatformOpenAI &&
		account.Type == AccountTypeOAuth &&
		!account.IsShadow() &&
		account.IsCodexQuotaOverdraftEnabled()
}

type codexQuotaOverdraftSchedulingCtxKey struct{}

// WithCodexQuotaOverdraftScheduling 标记普通 Codex 推理请求，
// 使其仅可绕过本地配置的预留阈值。
func WithCodexQuotaOverdraftScheduling(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if CodexQuotaOverdraftSchedulingEnabled(ctx) {
		return ctx
	}
	return context.WithValue(ctx, codexQuotaOverdraftSchedulingCtxKey{}, struct{}{})
}

func CodexQuotaOverdraftSchedulingEnabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(codexQuotaOverdraftSchedulingCtxKey{}).(struct{})
	return ok
}

func codexQuotaOverdraftSchedulingEnabled(ctx context.Context) bool {
	return CodexQuotaOverdraftSchedulingEnabled(ctx)
}

func isLegacyCodexQuotaOverdraftPause(reason string) bool {
	payload, ok := parseTempUnschedReasonPayload(reason)
	return ok && payload.Source == codexQuotaOverdraftLegacyPauseSource
}

func isCodexQuotaOverdraftBypassablePause(reason string) bool {
	return IsAccountSchedulingThresholdReason(reason) || isLegacyCodexQuotaOverdraftPause(reason)
}

// normalizeCodexQuotaOverdraftAccountForScheduling 只从内存候选账号中移除
// 本地额度阈值暂停；认证、人工暂停、过载、模型限制、RateLimitResetAt
// 以及其他阻断状态全部保留。
func normalizeCodexQuotaOverdraftAccountForScheduling(ctx context.Context, account *Account) *Account {
	if !codexQuotaOverdraftSchedulingEnabled(ctx) || !isCodexQuotaOverdraftAccount(account) {
		return account
	}
	if account.TempUnschedulableUntil == nil || !time.Now().Before(*account.TempUnschedulableUntil) {
		return account
	}
	if !isCodexQuotaOverdraftBypassablePause(account.TempUnschedulableReason) {
		return account
	}

	clone := *account
	clone.TempUnschedulableUntil = nil
	clone.TempUnschedulableReason = ""
	return &clone
}

func normalizeCodexQuotaOverdraftAccountsForScheduling(ctx context.Context, accounts []Account) []Account {
	for i := range accounts {
		if normalized := normalizeCodexQuotaOverdraftAccountForScheduling(ctx, &accounts[i]); normalized != &accounts[i] {
			accounts[i] = *normalized
		}
	}
	return accounts
}
