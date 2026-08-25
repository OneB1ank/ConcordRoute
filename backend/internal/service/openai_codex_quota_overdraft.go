package service

import (
	"context"
	"time"
)

const (
	// CodexQuotaOverdraftEnabledExtraKey 只启用本地额度策略透支。
	// 它仅绕过本地预留阈值，不绕过上游真实 429。
	CodexQuotaOverdraftEnabledExtraKey = "codex_quota_overdraft_enabled"

	// CodexQuotaOverdraftPrearmPercent 表示开始保留普通业务调用的准备阈值。
	CodexQuotaOverdraftPrearmPercent = 95.0
	// CodexQuotaOverdraftStartPercent 表示进入透支运行状态的阈值。
	CodexQuotaOverdraftStartPercent = 98.0

	// CodexQuotaOverdraftLegacyProbeExtraKey 仅用于在账号更新和迁移时
	// 清理旧版运行状态。
	CodexQuotaOverdraftLegacyProbeExtraKey = "codex_quota_overdraft_probe"

	codexQuotaOverdraftLegacyPauseSource = "codex_quota_overdraft"
)

const (
	CodexQuotaOverdraftStatusPreparing  = "preparing"
	CodexQuotaOverdraftStatusActive     = "active"
	CodexQuotaOverdraftStatusTerminated = "terminated"
)

// CodexQuotaOverdraftState 是管理端使用的只读状态投影。
// 它不落库、不触发探针，也不参与上游请求构造。
type CodexQuotaOverdraftState struct {
	Status        string     `json:"status"`
	QuotaWindow   string     `json:"quota_window"`
	UsedPercent   float64    `json:"used_percent"`
	PrearmPercent float64    `json:"prearm_percent"`
	StartPercent  float64    `json:"start_percent"`
	RecoverAt     *time.Time `json:"recover_at,omitempty"`
}

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
	if !codexQuotaOverdraftSchedulingEnabled(ctx) ||
		!codexQuotaOverdraftPrearmReached(account, time.Now().UTC()) {
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

// codexQuotaOverdraftPrearmReached 只使用可信、未过期的真实额度快照。
// 95% 后允许普通业务请求继续越过本地预留阈值；请求体、缓存键和会话标识保持原样。
func codexQuotaOverdraftPrearmReached(account *Account, now time.Time) bool {
	return codexQuotaOverdraftWindowReached(account, CodexQuotaOverdraftPrearmPercent, now)
}

func codexQuotaOverdraftWindowReached(account *Account, thresholdPercent float64, now time.Time) bool {
	if !isCodexQuotaOverdraftAccount(account) || !openAICodexSnapshotIdentityTrusted(account) {
		return false
	}
	for _, window := range []string{"5h", "7d"} {
		utilization, ok := resolveOpenAIQuotaUtilization(account.Extra, window, now)
		if ok && utilization*100 >= thresholdPercent {
			return true
		}
	}
	return false
}

func normalizeCodexQuotaOverdraftAccountsForScheduling(ctx context.Context, accounts []Account) []Account {
	for i := range accounts {
		if normalized := normalizeCodexQuotaOverdraftAccountForScheduling(ctx, &accounts[i]); normalized != &accounts[i] {
			accounts[i] = *normalized
		}
	}
	return accounts
}

func buildCodexQuotaOverdraftState(account *Account, usage *UsageInfo, now time.Time) *CodexQuotaOverdraftState {
	if !isCodexQuotaOverdraftAccount(account) || !account.IsActive() || usage == nil {
		return nil
	}

	type windowState struct {
		name     string
		used     float64
		recover  *time.Time
		eligible bool
	}
	windows := []windowState{
		{name: "5h", recover: usageResetAfter(usage.FiveHour, now)},
		{name: "7d", recover: usageResetAfter(usage.SevenDay, now)},
	}
	if usage.FiveHour != nil {
		windows[0].used = usage.FiveHour.Utilization
		windows[0].eligible = usageProgressOverdraftEligible(usage.FiveHour, CodexQuotaOverdraftPrearmPercent, now)
	}
	if usage.SevenDay != nil {
		windows[1].used = usage.SevenDay.Utilization
		windows[1].eligible = usageProgressOverdraftEligible(usage.SevenDay, CodexQuotaOverdraftPrearmPercent, now)
	}

	selected := make([]windowState, 0, len(windows))
	for _, window := range windows {
		if window.eligible {
			selected = append(selected, window)
		}
	}
	rateLimited := account.RateLimitResetAt != nil && account.RateLimitResetAt.After(now)
	if len(selected) == 0 && !rateLimited {
		return nil
	}

	state := &CodexQuotaOverdraftState{
		Status:        CodexQuotaOverdraftStatusPreparing,
		QuotaWindow:   "multiple",
		PrearmPercent: CodexQuotaOverdraftPrearmPercent,
		StartPercent:  CodexQuotaOverdraftStartPercent,
	}
	if len(selected) == 1 {
		state.QuotaWindow = selected[0].name
	}
	for _, window := range selected {
		if window.used > state.UsedPercent {
			state.UsedPercent = window.used
		}
		if window.recover != nil && (state.RecoverAt == nil || window.recover.After(*state.RecoverAt)) {
			state.RecoverAt = cloneTimePtr(window.recover)
		}
	}
	if state.UsedPercent >= CodexQuotaOverdraftStartPercent {
		state.Status = CodexQuotaOverdraftStatusActive
	}
	if rateLimited {
		state.Status = CodexQuotaOverdraftStatusTerminated
		state.RecoverAt = cloneTimePtr(account.RateLimitResetAt)
	}
	return state
}

func usageProgressOverdraftEligible(progress *UsageProgress, thresholdPercent float64, now time.Time) bool {
	if progress == nil || progress.Utilization < thresholdPercent {
		return false
	}
	return progress.ResetsAt == nil || progress.ResetsAt.After(now)
}

func usageResetAfter(progress *UsageProgress, now time.Time) *time.Time {
	if progress == nil || progress.ResetsAt == nil || !progress.ResetsAt.After(now) {
		return nil
	}
	return cloneTimePtr(progress.ResetsAt)
}
