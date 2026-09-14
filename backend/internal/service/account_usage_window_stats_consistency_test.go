package service

import (
	"context"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/usagestats"
)

type accountWindowStatsRangeProbe struct {
	*usageBatchLogRepoStub
	calls []struct {
		start time.Time
		end   time.Time
	}
	statsFn func(start, end time.Time) *usagestats.AccountStats
}

func (r *accountWindowStatsRangeProbe) GetAccountWindowStatsRange(_ context.Context, _ int64, start, end time.Time) (*usagestats.AccountStats, error) {
	r.calls = append(r.calls, struct {
		start time.Time
		end   time.Time
	}{start: start, end: end})
	if r.statsFn != nil {
		return r.statsFn(start, end), nil
	}
	return &usagestats.AccountStats{Requests: 1, Tokens: 10, Cost: 1, UserCost: 1}, nil
}

func TestAccountUsageService_OpenAIWindowStatsUseIndependentBoundedSnapshots(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	fiveReset := now.Add(-time.Minute)
	sevenReset := now.Add(7*24*time.Hour - time.Minute)
	repo := &accountWindowStatsRangeProbe{usageBatchLogRepoStub: &usageBatchLogRepoStub{}}
	svc := &AccountUsageService{usageLogRepo: repo}
	account := &Account{
		ID:       8123,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"codex_usage_updated_at": now.Format(time.RFC3339),
			"codex_5h_used_percent":  0.0,
			"codex_5h_reset_at":      fiveReset.Format(time.RFC3339),
			"codex_7d_used_percent":  2.0,
			"codex_7d_reset_at":      sevenReset.Format(time.RFC3339),
		},
	}

	usage, err := svc.getOpenAIUsage(context.Background(), account, false)
	if err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if len(repo.calls) != 2 {
		t.Fatalf("window stats calls = %d, want 2", len(repo.calls))
	}
	if !repo.calls[0].end.Equal(repo.calls[1].end) {
		t.Fatalf("window stats ends differ: %v vs %v", repo.calls[0].end, repo.calls[1].end)
	}
	if usage.FiveHour == nil || usage.SevenDay == nil {
		t.Fatalf("expected both usage windows, got %#v", usage)
	}
}

func TestCodexWindowStatsStartsDoesNotExpandWeeklyWindowAfterReset(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	fiveHourStart, sevenDayStart := codexWindowStatsStarts(
		nil,
		&UsageProgress{ResetsAt: func() *time.Time {
			t := now.Add(-time.Hour)
			return &t
		}()},
		now,
	)
	if !fiveHourStart.Equal(now.Add(-5 * time.Hour)) {
		t.Fatalf("5h start = %v, want %v", fiveHourStart, now.Add(-5*time.Hour))
	}
	wantWeeklyStart := now.Add(-time.Hour)
	if !sevenDayStart.Equal(wantWeeklyStart) {
		t.Fatalf("weekly start = %v, want its reset-derived boundary %v", sevenDayStart, wantWeeklyStart)
	}
	if !sevenDayStart.After(fiveHourStart) {
		t.Fatalf("test setup must represent a weekly reset after rolling 5h start: weekly=%v five=%v", sevenDayStart, fiveHourStart)
	}
}

func TestAccountUsageService_WeeklyOnlyKeepsRecentFiveHourStatsLocalOnly(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	weeklyReset := now.Add(-time.Hour)
	repo := &accountWindowStatsRangeProbe{usageBatchLogRepoStub: &usageBatchLogRepoStub{}}
	// 返回不同值，避免调用方意外把两个 SQL 时间范围混为一体。
	repo.statsFn = func(start, end time.Time) *usagestats.AccountStats {
		if !start.Equal(weeklyReset) {
			return &usagestats.AccountStats{Requests: 3, Tokens: 336800, Cost: 0.32, UserCost: 0.32}
		}
		return &usagestats.AccountStats{Requests: 93, Tokens: 12900000, Cost: 13.67, UserCost: 13.67}
	}

	svc := &AccountUsageService{usageLogRepo: repo}
	account := &Account{
		ID:       8124,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"codex_usage_updated_at": now.Format(time.RFC3339),
			// 此账号只有周窗口（主窗口）。
			"codex_5h_used_percent": 88.0,
			"codex_5h_reset_at":     now.Add(4 * time.Hour).Format(time.RFC3339),
			"codex_5h_available":    false,
			"codex_7d_used_percent": 3.0,
			"codex_7d_reset_at":     weeklyReset.Format(time.RFC3339),
			"codex_7d_available":    true,
		},
	}

	usage := svc.getPassiveOpenAIUsage(context.Background(), account)
	if usage.FiveHour == nil || !usage.FiveHour.LocalOnly {
		t.Fatalf("expected local-only recent 5h stats, got %#v", usage.FiveHour)
	}
	if usage.FiveHour.ResetsAt != nil || usage.FiveHour.Utilization != 0 {
		t.Fatalf("local-only 5h row must not expose quota progress: %#v", usage.FiveHour)
	}
	if usage.FiveHour.WindowStats == nil || usage.FiveHour.WindowStats.Requests != 3 || usage.FiveHour.WindowStats.Tokens != 336800 {
		t.Fatalf("unexpected local 5h stats: %#v", usage.FiveHour.WindowStats)
	}
	if usage.SevenDay == nil || usage.SevenDay.LocalOnly {
		t.Fatalf("expected real weekly quota window, got %#v", usage.SevenDay)
	}
	if usage.SevenDay.WindowStats == nil || usage.SevenDay.WindowStats.Requests != 93 {
		t.Fatalf("unexpected weekly stats: %#v", usage.SevenDay.WindowStats)
	}
	if len(repo.calls) != 2 {
		t.Fatalf("window stats calls = %d, want 2", len(repo.calls))
	}
	if repo.calls[0].start.After(now.Add(-5*time.Hour + 2*time.Second)) {
		t.Fatalf("5h local stats start %v is too recent", repo.calls[0].start)
	}
	if repo.calls[1].start.Before(weeklyReset.Add(-time.Second)) {
		t.Fatalf("weekly stats start %v should respect weekly reset %v", repo.calls[1].start, weeklyReset)
	}
}

func TestAccountUsageService_EmptyLocalWindowDoesNotCreateSyntheticQuota(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	repo := &accountWindowStatsRangeProbe{usageBatchLogRepoStub: &usageBatchLogRepoStub{}}
	repo.statsFn = func(_, _ time.Time) *usagestats.AccountStats { return &usagestats.AccountStats{} }
	svc := &AccountUsageService{usageLogRepo: repo}
	account := &Account{ID: 8125, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		"codex_usage_updated_at": now.Format(time.RFC3339),
		"codex_7d_used_percent":  0.0,
		"codex_7d_reset_at":      now.Add(6 * 24 * time.Hour).Format(time.RFC3339),
	}}

	usage := svc.getPassiveOpenAIUsage(context.Background(), account)
	if usage.FiveHour != nil {
		t.Fatalf("empty local 5h window should not fabricate a quota row: %#v", usage.FiveHour)
	}
}

func TestCodexQuotaWindowAvailable_ExplicitMarkerWinsOverLegacyFields(t *testing.T) {
	t.Parallel()

	legacy := map[string]any{
		"codex_5h_used_percent": 88.0,
		"codex_5h_reset_at":     "2099-01-01T00:00:00Z",
		"codex_5h_available":    false,
	}
	if codexQuotaWindowAvailable(legacy, "5h") {
		t.Fatal("explicit unavailable marker must suppress legacy 5h fields")
	}
	if !codexQuotaWindowAvailable(map[string]any{
		"codex_5h_used_percent": 12.0,
	}, "5h") {
		t.Fatal("legacy snapshot with a 5h field should remain readable")
	}
}
