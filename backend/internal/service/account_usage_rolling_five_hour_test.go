package service

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

// 用实际用量行按仓库契约聚合，不依赖客户端会话或额度快照的最后一笔。
type rollingFiveHourUsageRepo struct {
	*usageBatchLogRepoStub
	rows []UsageLog
}

func (r *rollingFiveHourUsageRepo) GetAccountWindowStatsRange(_ context.Context, accountID int64, start, end time.Time) (*usagestats.AccountStats, error) {
	stats := &usagestats.AccountStats{}
	for _, row := range r.rows {
		if row.AccountID != accountID || row.CreatedAt.Before(start) || !row.CreatedAt.Before(end) {
			continue
		}
		stats.Requests++
		stats.Tokens += int64(row.InputTokens + row.OutputTokens + row.CacheCreationTokens + row.CacheReadTokens)
		accountCost := row.TotalCost
		if row.AccountStatsCost != nil {
			accountCost = *row.AccountStatsCost
		}
		if row.AccountRateMultiplier != nil {
			accountCost *= *row.AccountRateMultiplier
		}
		stats.Cost += accountCost
		stats.StandardCost += row.TotalCost
		stats.UserCost += row.ActualCost
	}
	return stats, nil
}

func rollingFiveHourFixture(now time.Time) *rollingFiveHourUsageRepo {
	repo := &rollingFiveHourUsageRepo{usageBatchLogRepoStub: &usageBatchLogRepoStub{}}
	for i := 0; i < 7; i++ {
		session := fmt.Sprintf("session-%d", i%3)
		tokens, cost := 64900, 0.09
		at := now.Add(-time.Duration(i+1) * 30 * time.Minute)
		if i == 0 {
			cost = 0.08
		}
		if i == 5 {
			tokens = 65400
		}
		if i == 6 {
			tokens, cost, at = 91600, 0.89, now.Add(-500*time.Millisecond)
		}
		repo.rows = append(repo.rows, UsageLog{
			AccountID: 42, UserID: int64(i%2 + 1), APIKeyID: int64(i%2 + 1),
			RequestID: fmt.Sprintf("request-%d", i), SessionID: &session,
			CreatedAt: at, InputTokens: tokens - 100, OutputTokens: 20, CacheReadTokens: 80,
			TotalCost: cost, ActualCost: cost,
		})
	}
	return repo
}

// 零秒重置会随响应更新，旧逻辑仅统计最新 1 req；本地近 5h 应得到跨会话全部 7 req。
func TestOpenAILocalFiveHourZeroResetDoesNotDropEarlierSessions(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	repo := rollingFiveHourFixture(now)
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "3")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-primary-reset-after-seconds", "518400")
	headers.Set("x-codex-secondary-used-percent", "0")
	headers.Set("x-codex-secondary-window-minutes", "300")
	headers.Set("x-codex-secondary-reset-after-seconds", "0")
	snapshot := ParseCodexRateLimitHeaders(headers)
	require.NotNil(t, snapshot)
	snapshot.UpdatedAt = now.Add(-time.Second).Format(time.RFC3339)
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Extra: buildCodexUsageExtraUpdates(snapshot, now)}
	usage := &UsageInfo{}
	applyExtraToUsage(usage, account.Extra, now)
	svc := &AccountUsageService{usageLogRepo: repo}
	svc.addOpenAIWindowStats(context.Background(), account, usage, now)
	require.NotNil(t, usage.FiveHour)
	t.Logf("5h=%+v; 7d=%+v", usage.FiveHour.WindowStats, usage.SevenDay.WindowStats)
	require.Equal(t, int64(7), usage.FiveHour.WindowStats.Requests)
	require.Equal(t, int64(481500), usage.FiveHour.WindowStats.Tokens)
	require.InDelta(t, 1.42, usage.FiveHour.WindowStats.Cost, 1e-9)
	require.InDelta(t, 1.42, usage.FiveHour.WindowStats.UserCost, 1e-9)
	require.Zero(t, usage.FiveHour.Utilization, "上游百分比是独立快照，不与本地费用相加")
	require.Equal(t, int64(7), usage.SevenDay.WindowStats.Requests)
}

// 任何额度重置状态都不截断本地近五小时，重读或重启也不靠内存累加。
func TestOpenAILocalFiveHourIgnoresQuotaResetAndRebuildsFromLedger(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, delta := range []time.Duration{-6 * time.Hour, -time.Hour, 0, time.Hour, 4 * time.Hour, 10 * time.Hour} {
		t.Run(delta.String(), func(t *testing.T) {
			repo := rollingFiveHourFixture(now)
			reset := now.Add(delta)
			account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			for i := 0; i < 3; i++ {
				svc := &AccountUsageService{usageLogRepo: repo}
				usage := &UsageInfo{FiveHour: &UsageProgress{Utilization: 27, ResetsAt: &reset}}
				svc.addOpenAIWindowStats(context.Background(), account, usage, now)
				require.Equal(t, int64(7), usage.FiveHour.WindowStats.Requests)
				require.Equal(t, 27.0, usage.FiveHour.Utilization)
				require.Equal(t, reset, *usage.FiveHour.ResetsAt)
			}
		})
	}
}

// 半开时间边界、账号隔离、A/U 金额口径与新请求递增都沿用账务事实。
func TestOpenAILocalFiveHourBoundariesAndNewUsage(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	base, multiplier := 0.25, 2.0
	repo := &rollingFiveHourUsageRepo{usageBatchLogRepoStub: &usageBatchLogRepoStub{}, rows: []UsageLog{
		{AccountID: 42, CreatedAt: now.Add(-5 * time.Hour), InputTokens: 100, TotalCost: 0.1, ActualCost: 0.3, AccountStatsCost: &base, AccountRateMultiplier: &multiplier},
		{AccountID: 42, CreatedAt: now.Add(-5*time.Hour - time.Nanosecond), InputTokens: 999},
		{AccountID: 42, CreatedAt: now, InputTokens: 999},
		{AccountID: 43, CreatedAt: now.Add(-time.Minute), InputTokens: 999},
	}}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	read := func() *UsageInfo {
		svc := &AccountUsageService{usageLogRepo: repo}
		info := &UsageInfo{}
		svc.addOpenAIWindowStats(context.Background(), account, info, now)
		return info
	}
	info := read()
	require.True(t, info.FiveHour.LocalOnly)
	require.Equal(t, int64(1), info.FiveHour.WindowStats.Requests)
	require.Equal(t, int64(100), info.FiveHour.WindowStats.Tokens)
	require.InDelta(t, 0.5, info.FiveHour.WindowStats.Cost, 1e-9)
	require.InDelta(t, 0.3, info.FiveHour.WindowStats.UserCost, 1e-9)
	repo.rows = append(repo.rows, UsageLog{AccountID: 42, CreatedAt: now.Add(-time.Second), CacheReadTokens: 50, TotalCost: 0.2, ActualCost: 0.6})
	info = read()
	require.Equal(t, int64(2), info.FiveHour.WindowStats.Requests)
	require.Equal(t, int64(150), info.FiveHour.WindowStats.Tokens)
	require.InDelta(t, 0.7, info.FiveHour.WindowStats.Cost, 1e-9)
	require.InDelta(t, 0.9, info.FiveHour.WindowStats.UserCost, 1e-9)
}
