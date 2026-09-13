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
}

func (r *accountWindowStatsRangeProbe) GetAccountWindowStatsRange(_ context.Context, _ int64, start, end time.Time) (*usagestats.AccountStats, error) {
	r.calls = append(r.calls, struct {
		start time.Time
		end   time.Time
	}{start: start, end: end})
	return &usagestats.AccountStats{Requests: 1, Tokens: 10, Cost: 1, UserCost: 1}, nil
}

func TestAccountUsageService_OpenAIWindowStatsUseOneBoundedSnapshot(t *testing.T) {
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
	if repo.calls[1].start.After(repo.calls[0].start) {
		t.Fatalf("seven-day start %v is after five-hour start %v", repo.calls[1].start, repo.calls[0].start)
	}
	if usage.FiveHour == nil || usage.SevenDay == nil {
		t.Fatalf("expected both usage windows, got %#v", usage)
	}
}
