package service

import (
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 旧请求迟到的 trace 回调只能落入旧尝试，快照修改也不得污染后续日志。
func TestTTFTAttemptLateCallbackAndSnapshotIsolation(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	InitTTFTStageTiming(c, true)
	first := BeginTTFTUpstreamAttempt(c, 11)
	first("upstream_headers_received")
	first("stream_completed")
	second := BeginTTFTUpstreamAttempt(c, 22)
	first("first_upstream_byte")
	second("first_sse_event")
	require.NotContains(t, TTFTStageTimingSnapshot(c, time.Now()), "first_upstream_byte")
	attempts := TTFTAttemptTimingSnapshots(c)
	require.Len(t, attempts, 2)
	require.Equal(t, int64(11), attempts[0].AccountID)
	require.Equal(t, int64(22), attempts[1].AccountID)
	require.Contains(t, attempts[0].StagesMS, "first_upstream_byte")
	require.NotContains(t, attempts[1].StagesMS, "stream_completed")
	attempts[0].StagesMS["injected"] = 100
	require.NotContains(t, TTFTAttemptTimingSnapshots(c)[0].StagesMS, "injected")
}

// 诊断数量有界，多路回调和日志并行读取在 race 下保持独立。
func TestTTFTAttemptBoundedConcurrentSampling(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	InitTTFTStageTiming(c, true)
	var wg sync.WaitGroup
	for i := 0; i < maxTTFTDiagnosticAttempts+5; i++ {
		mark := BeginTTFTUpstreamAttempt(c, int64(i+1))
		wg.Add(1)
		go func() {
			defer wg.Done()
			mark("upstream_headers_received")
			mark("first_sse_event")
			mark("stream_completed")
			_ = TTFTAttemptTimingSnapshots(c)
		}()
	}
	wg.Wait()
	attempts := TTFTAttemptTimingSnapshots(c)
	require.Len(t, attempts, maxTTFTDiagnosticAttempts)
	require.Equal(t, 6, attempts[0].Attempt)
	require.Equal(t, maxTTFTDiagnosticAttempts+5, attempts[len(attempts)-1].Attempt)
}

func TestTTFTAttemptDisabledDoesNotAllocateState(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	BeginTTFTUpstreamAttempt(c, 1)("first_upstream_byte")
	beginTTFTStream(c, nil)("stream_completed")
	require.Empty(t, TTFTAttemptTimingSnapshots(c))
	require.Empty(t, TTFTStageTimingSnapshot(c, time.Now()))
}
