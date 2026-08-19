//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type blockingChannelMonitorV2Repo struct {
	*channelMonitorV2RepoStub
	entered chan struct{}
	exited  chan struct{}
}

type recordingChannelMonitorV2Repo struct {
	*channelMonitorV2RepoStub
	watermark ChannelMonitorV2AggregationWatermark
	ranges    [][2]time.Time
}

func (r *recordingChannelMonitorV2Repo) GetAggregationWatermark(context.Context) (*ChannelMonitorV2AggregationWatermark, error) {
	wm := r.watermark
	return &wm, nil
}

func (r *recordingChannelMonitorV2Repo) RecomputeRange(_ context.Context, start, end time.Time) error {
	r.ranges = append(r.ranges, [2]time.Time{start, end})
	return nil
}

func (r *blockingChannelMonitorV2Repo) GetConfig(context.Context) (*ChannelMonitorV2Config, error) {
	return &ChannelMonitorV2Config{Enabled: true, RefreshIntervalSeconds: 300}, nil
}

func (r *blockingChannelMonitorV2Repo) RecomputeRange(ctx context.Context, _, _ time.Time) error {
	close(r.entered)
	<-ctx.Done()
	close(r.exited)
	return ctx.Err()
}

func TestChannelMonitorV2MaxChunkForDepth(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	// Within last day → tightest ceiling (2h).
	require.Equal(t, channelMonitorV2MaxChunkNear1d, channelMonitorV2MaxChunkForDepth(now, now.Add(-2*time.Hour)))
	// Between 1d and 7d → 4h.
	require.Equal(t, channelMonitorV2MaxChunkNear7d, channelMonitorV2MaxChunkForDepth(now, now.Add(-2*24*time.Hour)))
	// Older than 7d → 6h (never 24h default).
	require.Equal(t, channelMonitorV2MaxChunkFar, channelMonitorV2MaxChunkForDepth(now, now.Add(-10*24*time.Hour)))
	require.Less(t, channelMonitorV2MaxChunkFar, 24*time.Hour)
	require.Equal(t, time.Hour, channelMonitorV2BackfillChunkInit)
	require.Equal(t, 15*time.Minute, channelMonitorV2MinBackfillChunk)
}

func TestChannelMonitorV2AggregatorAdaptiveChunk(t *testing.T) {
	s := NewChannelMonitorV2Aggregator(nil, nil, nil)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cursor := now.Add(-3 * time.Hour)

	// Failure shrinks chunk and sets backoff floor.
	s.backfillChunk = 2 * time.Hour
	s.recordBackfillFailure(now, cursor)
	require.Equal(t, time.Hour, s.backfillChunk)
	require.Equal(t, time.Minute, s.nextWaitFloor)
	require.Equal(t, 1, s.backfillFailures)

	// Repeated failure halves again and raises floor.
	s.recordBackfillFailure(now, cursor)
	require.Equal(t, 30*time.Minute, s.backfillChunk)
	require.Equal(t, 2*time.Minute, s.nextWaitFloor)

	// Fast success grows within depth ceiling and clears backoff.
	s.recordBackfillSuccess(cursor.Add(-30*time.Minute), 5*time.Second, now)
	require.Equal(t, 0, s.backfillFailures)
	require.Equal(t, time.Duration(0), s.nextWaitFloor)
	require.Greater(t, s.backfillChunk, 30*time.Minute)
	require.LessOrEqual(t, s.backfillChunk, channelMonitorV2MaxChunkForDepth(now, cursor.Add(-30*time.Minute)))
}

func TestChannelMonitorV2BackfillStartRespectsChunkCeiling(t *testing.T) {
	end := time.Date(2026, 8, 8, 23, 45, 0, 0, time.UTC)
	cutoff := end.Add(-90 * 24 * time.Hour)

	start := channelMonitorV2BackfillStart(end, cutoff, channelMonitorV2MaxChunkFar)

	require.Equal(t, channelMonitorV2MaxChunkFar, end.Sub(start))
	require.Equal(t, 17, start.Hour(), "非整日结束点不应被扩成整日扫描")

	nearCutoff := cutoff.Add(2 * time.Hour)
	require.Equal(t, cutoff, channelMonitorV2BackfillStart(nearCutoff, cutoff, channelMonitorV2MaxChunkFar))
}

func TestChannelMonitorV2ForwardCatchupRange(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	start, end, ok := channelMonitorV2ForwardCatchupRange(now.Add(-5*time.Hour), now)
	require.True(t, ok)
	require.Equal(t, now.Add(-5*time.Hour), start)
	require.Equal(t, channelMonitorV2ForwardCatchupChunk, end.Sub(start))

	cutoff := now.Add(-channelMonitorV2RetentionMax)
	start, end, ok = channelMonitorV2ForwardCatchupRange(now.Add(-120*24*time.Hour), now)
	require.True(t, ok)
	require.Equal(t, cutoff, start)
	require.Equal(t, channelMonitorV2ForwardCatchupChunk, end.Sub(start))

	_, _, ok = channelMonitorV2ForwardCatchupRange(now.Add(-5*time.Minute), now)
	require.False(t, ok, "最近重叠窗口内的数据由常规刷新处理")
}

func TestChannelMonitorV2AggregatorFillsForwardGapBeforeRecentOverlap(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	repo := &recordingChannelMonitorV2Repo{
		channelMonitorV2RepoStub: &channelMonitorV2RepoStub{},
		watermark: ChannelMonitorV2AggregationWatermark{
			DataThrough:      now.Add(-5 * time.Hour),
			BackfillCursor:   now.Add(-channelMonitorV2RetentionMax),
			LastSuccessfulAt: now.Add(-5 * time.Hour),
			HasData:          true,
		},
	}
	aggregator := NewChannelMonitorV2Aggregator(repo, nil, nil)
	aggregator.ctx, aggregator.cancel = context.WithCancel(context.Background())
	defer aggregator.cancel()

	aggregator.runOnce()

	require.Len(t, repo.ranges, 1)
	require.Equal(t, now.Add(-5*time.Hour), repo.ranges[0][0])
	require.Equal(t, channelMonitorV2ForwardCatchupChunk, repo.ranges[0][1].Sub(repo.ranges[0][0]))
}

func TestChannelMonitorV2AggregatorStopWaitsForLoopExit(t *testing.T) {
	repo := &blockingChannelMonitorV2Repo{
		channelMonitorV2RepoStub: &channelMonitorV2RepoStub{},
		entered:                  make(chan struct{}),
		exited:                   make(chan struct{}),
	}
	aggregator := NewChannelMonitorV2Aggregator(repo, nil, channelMonitorRuntimeStub{rt: ChannelMonitorRuntime{
		Enabled: true,
		Mode:    ChannelMonitorModeV2,
	}})
	aggregator.Start()

	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("聚合任务未进入执行阶段")
	}

	stopped := make(chan struct{})
	go func() {
		aggregator.Stop()
		close(stopped)
	}()

	select {
	case <-repo.exited:
	case <-time.After(time.Second):
		t.Fatal("停止时未取消正在运行的聚合任务")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("停止操作未等待聚合循环退出")
	}
}
