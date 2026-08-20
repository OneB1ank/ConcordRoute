package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type schedulerSnapshotContextCacheStub struct {
	SchedulerCache
}

func (s schedulerSnapshotContextCacheStub) GetSnapshot(ctx context.Context, bucket SchedulerBucket) ([]*Account, bool, error) {
	return nil, false, ctx.Err()
}

type schedulerSnapshotFallbackRepoStub struct {
	AccountRepository
	calls            int
	overdraftContext bool
}

func (r *schedulerSnapshotFallbackRepoStub) ListSchedulableByPlatform(ctx context.Context, platform string) ([]Account, error) {
	r.calls++
	return nil, nil
}

func (r *schedulerSnapshotFallbackRepoStub) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]Account, error) {
	r.calls++
	r.overdraftContext = CodexQuotaOverdraftSchedulingEnabled(ctx)
	return nil, nil
}

func TestSchedulerSnapshotService_OpenAIRebuildKeepsOverdraftCandidates(t *testing.T) {
	repo := &schedulerSnapshotFallbackRepoStub{}
	svc := &SchedulerSnapshotService{accountRepo: repo}

	_, err := svc.loadAccountsFromDB(context.Background(), SchedulerBucket{Platform: PlatformOpenAI}, false)
	require.NoError(t, err)
	require.True(t, repo.overdraftContext)
}

func TestSchedulerSnapshotService_NonOpenAIRebuildKeepsContextUnmarked(t *testing.T) {
	repo := &schedulerSnapshotFallbackRepoStub{}
	svc := &SchedulerSnapshotService{accountRepo: repo}

	_, err := svc.loadAccountsFromDB(context.Background(), SchedulerBucket{Platform: PlatformAnthropic}, false)
	require.NoError(t, err)
	require.False(t, repo.overdraftContext)
}

func TestSchedulerSnapshotService_ListSchedulableAccountsStopsWhenCacheContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := &schedulerSnapshotFallbackRepoStub{}
	svc := NewSchedulerSnapshotService(schedulerSnapshotContextCacheStub{}, nil, repo, nil, nil)

	accounts, useMixed, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, accounts)
	require.False(t, useMixed)
	require.Equal(t, 0, repo.calls)
}

func TestSchedulerSnapshotPlatformsIncludesQoder(t *testing.T) {
	require.Contains(t, schedulerSnapshotPlatforms(), PlatformQoder)
}

func TestSchedulerSnapshotServiceCanonicalBucketsIncludesQoder(t *testing.T) {
	buckets := schedulerCanonicalBuckets(0)

	require.Contains(t, buckets, SchedulerBucket{GroupID: 0, Platform: PlatformQoder, Mode: SchedulerModeSingle})
	require.Contains(t, buckets, SchedulerBucket{GroupID: 0, Platform: PlatformQoder, Mode: SchedulerModeForced})
	require.NotContains(t, buckets, SchedulerBucket{GroupID: 0, Platform: PlatformQoder, Mode: SchedulerModeMixed})
}
