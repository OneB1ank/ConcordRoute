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

func TestTTFTStageTimingSeparatesQueueFromUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	InitTTFTStageTiming(c, true)

	MarkTTFTStage(c, "auth_complete")
	MarkTTFTStage(c, "user_slot_wait_started")
	time.Sleep(5 * time.Millisecond)
	MarkTTFTStage(c, "user_slot_acquired")
	MarkTTFTStage(c, "account_selection_started")
	time.Sleep(5 * time.Millisecond)
	MarkTTFTStage(c, "account_selected")
	MarkTTFTStage(c, "account_slot_wait_started")
	time.Sleep(5 * time.Millisecond)
	MarkTTFTStage(c, "account_slot_acquired")
	MarkTTFTStage(c, "routing_complete")
	time.Sleep(5 * time.Millisecond)
	MarkTTFTStage(c, "upstream_do_started")
	time.Sleep(5 * time.Millisecond)
	MarkTTFTStage(c, "first_upstream_byte")
	MarkTTFTStage(c, "first_downstream_flush")

	snapshot := TTFTStageTimingSnapshot(c, time.Now())
	require.Less(t, snapshot["user_slot_wait_started"], snapshot["user_slot_acquired"])
	require.Less(t, snapshot["account_selected"], snapshot["account_slot_acquired"])
	require.Less(t, snapshot["routing_complete"], snapshot["first_upstream_byte"])
	require.GreaterOrEqual(t, snapshot["first_upstream_byte"], int64(20))
}
