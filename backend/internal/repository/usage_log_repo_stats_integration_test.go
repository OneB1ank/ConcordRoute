//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/usagestats"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/stretchr/testify/require"
)

// 真实数据库覆盖管理页被动读取：周重置后的账务小于滚动 5h 并不表示漏算。
func TestUsageLog_OpenAIWindowCostsAfterWeeklyReset(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	now := time.Now().UTC().Truncate(time.Second)
	weekStart := now.Add(-time.Hour)
	account := mustCreateAccount(t, client, &service.Account{
		Name: "weekly-reset-costs", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Extra: map[string]any{
			"codex_5h_used_percent": 0.0, "codex_5h_reset_at": now.Format(time.RFC3339),
			"codex_7d_used_percent": 1.0, "codex_7d_reset_at": weekStart.Add(7 * 24 * time.Hour).Format(time.RFC3339),
			"codex_usage_updated_at": now.Format(time.RFC3339),
		},
	})
	other := mustCreateAccount(t, client, &service.Account{Name: "weekly-reset-other"})
	var keys []*service.APIKey
	for i := 0; i < 2; i++ {
		user := mustCreateUser(t, client, &service.User{Email: fmt.Sprintf("weekly-reset-%d@test.com", i)})
		keys = append(keys, mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: fmt.Sprintf("sk-weekly-reset-%d", i), Name: "k"}))
	}
	for i := 0; i < 19; i++ {
		at, tokens, cost, accountID := now.Add(-time.Duration(i+1)*time.Minute), 140000, 0.19, account.ID
		switch i {
		case 13:
			tokens = 180000
		case 14:
			at, tokens, cost = now.Add(-2*time.Hour), 100000, 1.16
		case 15:
			at, tokens, cost = now.Add(-3*time.Hour), 100000, 1.17
		case 16:
			at, cost = now.Add(-6*time.Hour), 99
		case 17:
			at, cost = now.Add(time.Hour), 99
		case 18:
			accountID, cost = other.ID, 99
		}
		session := fmt.Sprintf("weekly-reset-session-%d", i%3)
		key := keys[i%2]
		_, err := repo.Create(ctx, &service.UsageLog{
			UserID: key.UserID, APIKeyID: key.ID, AccountID: accountID,
			RequestID: fmt.Sprintf("weekly-reset-%d", i), Model: "test-model",
			SessionID: &session, CreatedAt: at, InputTokens: tokens,
			TotalCost: cost, ActualCost: cost,
		})
		require.NoError(t, err)
	}
	svc := service.NewAccountUsageService(NewAccountRepository(client, nil, nil), repo,
		nil, nil, nil, nil, nil, nil, service.NewUsageCache(), nil, nil, nil, nil)
	for i := 0; i < 2; i++ {
		usage, err := svc.GetPassiveUsage(ctx, account.ID)
		require.NoError(t, err)
		batch, failures, err := svc.GetUsageBatch(ctx, []int64{account.ID, account.ID}, false)
		require.NoError(t, err)
		require.Empty(t, failures)
		require.Equal(t, usage.FiveHour.WindowStats, batch[account.ID].FiveHour.WindowStats)
		require.Equal(t, usage.SevenDay.WindowStats, batch[account.ID].SevenDay.WindowStats)
		require.Equal(t, int64(16), usage.FiveHour.WindowStats.Requests)
		require.Equal(t, int64(2200000), usage.FiveHour.WindowStats.Tokens)
		require.InDelta(t, 4.99, usage.FiveHour.WindowStats.Cost, 1e-9)
		require.InDelta(t, 4.99, usage.FiveHour.WindowStats.UserCost, 1e-9)
		require.Equal(t, int64(14), usage.SevenDay.WindowStats.Requests)
		require.Equal(t, int64(2000000), usage.SevenDay.WindowStats.Tokens)
		require.InDelta(t, 2.66, usage.SevenDay.WindowStats.Cost, 1e-9)
		require.InDelta(t, 2.66, usage.SevenDay.WindowStats.UserCost, 1e-9)
		t.Logf("postgres passive/batch: 5h=%d A/U=%.2f/%.2f; 7d=%d A/U=%.2f/%.2f",
			usage.FiveHour.WindowStats.Requests, usage.FiveHour.WindowStats.Cost, usage.FiveHour.WindowStats.UserCost,
			usage.SevenDay.WindowStats.Requests, usage.SevenDay.WindowStats.Cost, usage.SevenDay.WindowStats.UserCost)
	}
	// 半开边界和费用倍率使用同一真实 SQL；标准、账号和用户口径互不替代。
	_, err := tx.ExecContext(ctx, `UPDATE usage_logs SET account_stats_cost = total_cost / 2,
		account_rate_multiplier = 2, actual_cost = total_cost * 3 WHERE account_id = $1`, account.ID)
	require.NoError(t, err)
	stats, err := repo.GetAccountWindowStatsRange(ctx, account.ID, weekStart, now)
	require.NoError(t, err)
	require.InDelta(t, 2.66, stats.Cost, 1e-9)
	require.InDelta(t, 2.66, stats.StandardCost, 1e-9)
	require.InDelta(t, 7.98, stats.UserCost, 1e-9)
	boundary := now.Add(-14 * time.Minute)
	stats, err = repo.GetAccountWindowStatsRange(ctx, account.ID, boundary, boundary.Add(time.Microsecond))
	require.NoError(t, err)
	require.Equal(t, int64(1), stats.Requests)
	stats, err = repo.GetAccountWindowStatsRange(ctx, account.ID, boundary.Add(-time.Microsecond), boundary)
	require.NoError(t, err)
	require.Zero(t, stats.Requests)
}

func TestUsageLog_GetStatsWithFilters_AggregatesAndEndpoints(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)

	user := mustCreateUser(t, client, &service.User{Email: "stats@test.com"})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-stats-1", Name: "k"})
	account := mustCreateAccount(t, client, &service.Account{Name: "acc-stats"})

	now := time.Now().UTC()
	inboundEndpoint := "/v1/messages"
	upstreamEndpoint := "/v1/responses"
	for i := 0; i < 3; i++ {
		_, err := repo.Create(ctx, &service.UsageLog{
			UserID: user.ID, APIKeyID: apiKey.ID, AccountID: account.ID,
			Model: "claude-3", InputTokens: 2, OutputTokens: 3,
			CacheCreationTokens: 4, CacheReadTokens: 5,
			TotalCost: 0.5, ActualCost: 0.4, CreatedAt: now,
			InboundEndpoint: &inboundEndpoint, UpstreamEndpoint: &upstreamEndpoint,
		})
		require.NoError(t, err)
	}

	start := now.Add(-1 * time.Hour)
	end := now.Add(1 * time.Hour)
	// 按本测试创建的 user 维度过滤:集成库为共享实例,其它用 testEntClient 的兄弟测试会留下
	// 已提交的 usage_log 行(含零 token 的失败请求),不限定 user 会把它们计入 TotalRequests。
	stats, err := repo.GetStatsWithFilters(ctx, usagestats.UsageLogFilters{UserID: user.ID, StartTime: &start, EndTime: &end})
	require.NoError(t, err)
	require.Equal(t, int64(3), stats.TotalRequests)
	require.Equal(t, int64(6), stats.TotalInputTokens)
	require.Equal(t, int64(9), stats.TotalOutputTokens)
	require.Equal(t, int64(27), stats.TotalCacheTokens)
	require.Equal(t, int64(12), stats.TotalCacheCreationTokens)
	require.Equal(t, int64(15), stats.TotalCacheReadTokens)
	require.InDelta(t, 1.2, stats.TotalActualCost, 1e-9)
	require.NotEmpty(t, stats.Endpoints)
	require.NotEmpty(t, stats.UpstreamEndpoints)
	require.NotEmpty(t, stats.EndpointPaths)
}

// TestUsageLog_GetModelStats_MergesCompositePrefix 验证复合前缀不会拆分内部模型统计。
func TestUsageLog_GetModelStats_MergesCompositePrefix(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)

	user := mustCreateUser(t, client, &service.User{Email: "model-stats-composite@test.com"})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-model-stats-composite", Name: "k"})
	account := mustCreateAccount(t, client, &service.Account{Name: "acc-model-stats-composite"})
	now := time.Now().UTC()

	for _, requestedModel := range []string{"gpt-5.6-sol", "GPT/gpt-5.6-sol"} {
		_, err := repo.Create(ctx, &service.UsageLog{
			UserID: user.ID, APIKeyID: apiKey.ID, AccountID: account.ID,
			Model: "gpt-5.6-sol", RequestedModel: requestedModel,
			InputTokens: 10, OutputTokens: 5, TotalCost: 0.1, ActualCost: 0.1,
			CreatedAt: now,
		})
		require.NoError(t, err)
	}

	stats, err := repo.GetModelStatsWithFilters(
		ctx,
		now.Add(-time.Hour),
		now.Add(time.Hour),
		user.ID,
		0,
		0,
		0,
		nil,
		nil,
		nil,
	)
	require.NoError(t, err)
	require.Len(t, stats, 1)
	require.Equal(t, "gpt-5.6-sol", stats[0].Model)
	require.Equal(t, int64(2), stats[0].Requests)
	require.Equal(t, int64(30), stats[0].TotalTokens)
}
