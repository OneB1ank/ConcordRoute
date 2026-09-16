package openai_ws_v2

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// 用可控时钟验证用户提出的 0.5s 空结构、8s 正文案例，原帧逐字节转发。
func TestRelaySemanticTTFTSeparatesVisibleContent(t *testing.T) {
	frames := []passthroughTestFrame{
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.created","response":{"id":"resp_semantic"}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.output_item.added","response_id":"resp_semantic","item":{"type":"reasoning","summary":[]}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.reasoning_summary_text.delta","response_id":"resp_semantic","delta":"summary"}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"resp_semantic","usage":{"input_tokens":2,"output_tokens":3}}}`)},
	}
	client := newPassthroughTestFrameConn(nil, false)
	upstream := newPassthroughTestFrameConn(frames, true)
	base := time.Unix(1000, 0)
	var clock atomic.Int64
	var turn RelayTurnResult
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, exit := Relay(ctx, client, upstream, []byte(`{"type":"response.create","model":"test"}`), RelayOptions{
		Now: func() time.Time { return base.Add(time.Duration(clock.Load()) * time.Millisecond) },
		OnUpstreamEvent: func(eventType string, _ []byte) {
			switch eventType {
			case "response.output_item.added":
				clock.Store(500)
			case "response.reasoning_summary_text.delta":
				clock.Store(8000)
			case "response.completed":
				clock.Store(9000)
			}
		},
		OnTurnComplete: func(value RelayTurnResult) { turn = value },
	})
	require.Nil(t, exit)
	require.NotNil(t, turn.SemanticFirstTokenMs)
	require.Equal(t, 500, *turn.SemanticFirstTokenMs)
	require.Equal(t, 8000, *turn.FirstTokenMs)
	require.Equal(t, 500, *result.SemanticFirstTokenMs)
	require.Equal(t, 8000, *result.FirstTokenMs)
	require.Equal(t, 9*time.Second, turn.Duration)
	require.Equal(t, 2, turn.Usage.InputTokens)
	require.Equal(t, 3, turn.Usage.OutputTokens)
	require.Len(t, client.Writes(), len(frames))
	for i, frame := range frames {
		require.Equal(t, frame.payload, client.Writes()[i].payload)
	}
}

// 后续轮次与无 ID 终态使用本轮语义样本，禁止复用连接首轮的 500ms。
func TestRelaySemanticTTFTTurnIsolation(t *testing.T) {
	state := &relayState{}
	base := time.Unix(1000, 0)
	observe := func(ms int, payload string) observedUpstreamEvent {
		return observeUpstreamMessage(state, []byte(payload), base,
			func() time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }, nil, nil)
	}
	state.beginPendingTurn()
	observe(0, `{"type":"response.created","response":{"id":"resp_first"}}`)
	observe(500, `{"type":"response.output_item.added","item":{"type":"reasoning","summary":[]}}`)
	observe(8000, `{"type":"response.output_text.delta","delta":"first"}`)
	first := observe(9000, `{"type":"response.completed","response":{"id":"resp_first"}}`)
	require.Equal(t, 500, *first.semanticFirstToken)
	state.beginPendingTurn()
	observe(10000, `{"type":"response.created","response":{"id":"resp_second"}}`)
	observe(10300, `{"type":"response.output_item.added","item":{"type":"reasoning","summary":[]}}`)
	observe(18000, `{"type":"response.output_text.delta","delta":"second"}`)
	second := observe(19000, `{"type":"response.completed"}`)
	require.Equal(t, 300, *second.semanticFirstToken)
	require.Equal(t, 8000, *second.firstToken)
	require.Equal(t, 500, *state.semanticFirstTokenMs)
	require.False(t, state.hasUnfinishedTurn())
	state.beginPendingTurn()
	observe(20000, `{"type":"response.created","response":{"id":"resp_third"}}`)
	third := observe(20900, `{"type":"response.completed"}`)
	require.Nil(t, third.semanticFirstToken, "无语义事件的后续轮次保持缺省")
	require.Nil(t, third.firstToken)
}

// 延迟 created 不移动既有逐轮起点；连接级统计和逐轮统计的历史区间不同。
func TestRelaySemanticTTFTDelayedCreatedPreservesTimingOrigin(t *testing.T) {
	state := &relayState{}
	base := time.Unix(1000, 0)
	observe := func(ms int, payload string) observedUpstreamEvent {
		return observeUpstreamMessage(state, []byte(payload), base,
			func() time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }, nil, nil)
	}
	state.beginPendingTurn()
	observe(5000, `{"type":"response.created","response":{"id":"resp_delayed"}}`)
	observe(5500, `{"type":"response.output_item.added","item":{"type":"reasoning","summary":[]}}`)
	observe(8000, `{"type":"response.output_text.delta","delta":"first"}`)
	first := observe(9000, `{"type":"response.completed","response":{"id":"resp_delayed"}}`)
	require.Equal(t, 500, *first.semanticFirstToken)
	require.Equal(t, 3000, *first.firstToken)
	require.Equal(t, 4*time.Second, first.duration)
	require.Equal(t, 5500, *state.semanticFirstTokenMs)
	require.Equal(t, 8000, *state.firstTokenMs)

	state.beginPendingTurn()
	observe(15000, `{"type":"response.created","response":{"id":"resp_delayed_next"}}`)
	observe(15700, `{"type":"response.output_item.added","item":{"type":"reasoning","summary":[]}}`)
	observe(18000, `{"type":"response.output_text.delta","delta":"second"}`)
	second := observe(19000, `{"type":"response.completed"}`)
	require.Equal(t, 700, *second.semanticFirstToken)
	require.Equal(t, 3000, *second.firstToken)
	require.Equal(t, 4*time.Second, second.duration)
	require.False(t, state.hasUnfinishedTurn())
}
