package service

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestTTFTStageTimingRecordsFirstOccurrenceAndElapsedMilliseconds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	InitTTFTStageTiming(c, true)
	MarkTTFTStage(c, "forward_started")
	MarkTTFTStage(c, "forward_started")

	snapshot := TTFTStageTimingSnapshot(c, time.Now())
	require.Contains(t, snapshot, "request_received")
	require.Contains(t, snapshot, "forward_started")
	require.GreaterOrEqual(t, snapshot["forward_started"], snapshot["request_received"])
	require.Equal(t, []string{"forward_started", "request_received"}, TTFTStageTimingNames(c))
}

func TestTTFTStageTimingDisabledAndInvalidNamesAreNoOp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	InitTTFTStageTiming(c, false)
	MarkTTFTStage(c, "first\r\nheader")
	require.Empty(t, TTFTStageTimingSnapshot(c, time.Now()))

	InitTTFTStageTiming(c, true)
	MarkTTFTStage(c, "first\r\nheader")
	snapshot := TTFTStageTimingSnapshot(c, time.Now())
	require.Contains(t, snapshot, "firstheader")
	require.Contains(t, snapshot, "request_received")
}
