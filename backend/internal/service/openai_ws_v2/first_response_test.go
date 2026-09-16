package openai_ws_v2

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// 延迟创建通知按请求发出前计时，不从创建通知自己开始而得到虚假的 0ms。
func TestRelayFirstResponseTurnTiming(t *testing.T) {
	base := time.Unix(1000, 0)
	var timing relayFirstResponse
	timing.begin(base)
	require.Nil(t, observeFirstResponse(&timing, nil, base))
	first := observeFirstResponse(&timing, []byte(`{"type":"response.created","response":{"id":"resp_1"}}`), base.Add(5*time.Second))
	require.Equal(t, 5000, *first)
	require.Equal(t, 5000, *observeFirstResponse(&timing, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`), base.Add(9*time.Second)))
	timing.begin(base.Add(10 * time.Second))
	require.Equal(t, 300, *observeFirstResponse(&timing, []byte(`{"type":"response.created","response":{"id":"resp_2"}}`), base.Add(10300*time.Millisecond)))
	require.Equal(t, 300, *observeFirstResponse(&timing, []byte(`{"type":"response.completed"}`), base.Add(12*time.Second)))
	require.Equal(t, 5000, *timing.snapshot(), "连接样本不污染后续轮次")
	require.Empty(t, timing.byID)
	require.Empty(t, timing.pending)
	require.Nil(t, timing.active)
	require.Nil(t, observeFirstResponse(&timing, []byte(`{"type":"notification"}`), base.Add(13*time.Second)), "闲置通知不制造请求")
}

// 先收到无 ID 的数据，随后补 ID，仍使用第一块数据；带 ID 的交错响应各自隔离。
// 无 ID 帧沿用活动响应，不猜测归属；生产入口仍拒绝重叠 response.create。
func TestRelayFirstResponseMissingIDAndOverlap(t *testing.T) {
	base := time.Unix(1000, 0)
	var timing relayFirstResponse
	timing.begin(base)
	require.Equal(t, 500, *observeFirstResponse(&timing, []byte(`{"type":"notice"}`), base.Add(500*time.Millisecond)))
	require.Equal(t, 500, *observeFirstResponse(&timing, []byte(`{"type":"response.created","response":{"id":"resp_1"}}`), base.Add(time.Second)))
	timing.begin(base.Add(2 * time.Second))
	require.Equal(t, 500, *observeFirstResponse(&timing, []byte(`{"type":"notice"}`), base.Add(2500*time.Millisecond)), "无 ID 帧不抢占新请求的 pending 样本")
	require.Equal(t, 1000, *observeFirstResponse(&timing, []byte(`{"type":"response.created","response":{"id":"resp_2"}}`), base.Add(3*time.Second)))
	require.Equal(t, 500, *observeFirstResponse(&timing, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`), base.Add(4*time.Second)))
	require.Equal(t, 1000, *observeFirstResponse(&timing, []byte(`{"type":"response.completed"}`), base.Add(5*time.Second)), "旧响应结束不丢失新活动轮次")
	timing.begin(base.Add(6 * time.Second))
	require.Equal(t, 1000, *observeFirstResponse(&timing, []byte(`{"type":"response.completed"}`), base.Add(7*time.Second)))
	require.Empty(t, timing.byID)
}

type firstResponseTimedConn struct {
	*passthroughTestFrameConn
	clock *atomic.Int64
	times []int64
	index int
}

func (c *firstResponseTimedConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	kind, payload, err := c.passthroughTestFrameConn.ReadFrame(ctx)
	if err == nil && c.index < len(c.times) {
		c.clock.Store(c.times[c.index])
		c.index++
	}
	return kind, payload, err
}

// 通过真正的 Relay 入口验证完整帧逐字节转发，以及首块和原首内容分离。
func TestRelayFirstResponseWiring(t *testing.T) {
	base := time.Unix(1000, 0)
	var clock atomic.Int64
	frames := []passthroughTestFrame{
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.created","response":{"id":"resp_first"}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.output_text.delta","response_id":"resp_first","delta":"hello"}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"resp_first","usage":{"input_tokens":2,"output_tokens":3}}}`)},
	}
	client := newPassthroughTestFrameConn(nil, false)
	upstream := &firstResponseTimedConn{passthroughTestFrameConn: newPassthroughTestFrameConn(frames, true),
		clock: &clock, times: []int64{5000, 8000, 9000}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var turn RelayTurnResult
	result, exit := Relay(ctx, client, upstream, []byte(`{"type":"response.create","model":"test"}`), RelayOptions{
		FirstMessageSent: true, FirstResponseStartAt: base.Add(-time.Second),
		Now:            func() time.Time { return base.Add(time.Duration(clock.Load()) * time.Millisecond) },
		OnTurnComplete: func(value RelayTurnResult) { turn = value },
	})
	require.Nil(t, exit)
	require.Equal(t, 6000, *turn.FirstResponseMs, "包含中继开始前的转发等待")
	require.Equal(t, 6000, *result.FirstResponseMs)
	require.Equal(t, 10*time.Second, turn.ResponseDuration)
	require.Equal(t, 3000, *turn.FirstTokenMs, "旧逐轮首内容起点保持不变")
	require.Equal(t, 8000, *result.FirstTokenMs)
	require.Equal(t, 3, turn.Usage.OutputTokens)
	require.Len(t, client.Writes(), len(frames))
	for i, frame := range frames {
		require.Equal(t, frame.payload, client.Writes()[i].payload)
	}
}

// 测试载体复用生产 envelope，不另建关联字段规则。
func observeFirstResponse(timing *relayFirstResponse, payload []byte, now time.Time) *int {
	if len(payload) == 0 {
		return nil
	}
	kind, id := relayEventEnvelope(payload)
	return timing.observe(kind, id, now)
}

func TestRelayFirstResponseErrorBeforeFailed(t *testing.T) {
	base := time.Unix(1000, 0)
	state := &relayState{}
	state.firstResponse.begin(base)
	state.beginPendingTurn()
	observeUpstreamMessage(state, []byte(`{"type":"error","error":{"message":"test"}}`), base, func() time.Time { return base.Add(time.Second) }, nil, nil)
	got := observeUpstreamMessage(state, []byte(`{"type":"response.failed","response":{"id":"resp_error"}}`), base, func() time.Time { return base.Add(2 * time.Second) }, nil, nil)
	require.NotNil(t, got.firstResponse)
	require.Equal(t, 1000, *got.firstResponse)
	require.Equal(t, 2*time.Second, got.responseDuration)
	require.Empty(t, state.firstResponse.byID)
	require.Nil(t, state.firstResponse.active)
}
