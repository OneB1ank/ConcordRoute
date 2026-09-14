package openai_ws_v2

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// 无 ID 终态仍须返回当前轮次的时间，既不伪造响应 ID，也不沿用上一轮首字。
func TestRelayTTFTIDLessTerminalPreservesActiveTiming(t *testing.T) {
	for _, withContent := range []bool{false, true} {
		t.Run(fmt.Sprintf("content_%t", withContent), func(t *testing.T) {
			frames := []passthroughTestFrame{
				{msgType: coderws.MessageText, payload: []byte(`{"type":"response.created","response":{"id":"resp_noid"}}`)},
			}
			if withContent {
				frames = append(frames, passthroughTestFrame{msgType: coderws.MessageText,
					payload: []byte(`{"type":"response.output_text.delta","response_id":"resp_noid","delta":"visible"}`)})
			}
			frames = append(frames, passthroughTestFrame{msgType: coderws.MessageText,
				payload: []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":2,"output_tokens":1}}}`)})
			client := newPassthroughTestFrameConn(nil, false)
			upstream := newPassthroughTestFrameConn(frames, true)
			base := time.Unix(1000, 0)
			var clockMs atomic.Int64
			var turns []RelayTurnResult
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, exit := Relay(ctx, client, upstream, []byte(`{"type":"response.create","model":"test-model"}`), RelayOptions{
				Now: func() time.Time { return base.Add(time.Duration(clockMs.Load()) * time.Millisecond) },
				OnUpstreamEvent: func(eventType string, _ []byte) {
					switch eventType {
					case "response.output_text.delta":
						clockMs.Store(500)
					case "response.completed":
						clockMs.Store(800)
					}
				},
				OnTurnComplete: func(turn RelayTurnResult) { turns = append(turns, turn) },
			})
			require.Nil(t, exit)
			require.Len(t, turns, 1)
			require.Empty(t, turns[0].RequestID)
			require.Equal(t, 800*time.Millisecond, turns[0].Duration)
			require.Equal(t, 2, turns[0].Usage.InputTokens)
			if withContent {
				require.NotNil(t, turns[0].FirstTokenMs)
				require.Equal(t, 500, *turns[0].FirstTokenMs)
			} else {
				require.Nil(t, turns[0].FirstTokenMs)
			}
			require.Len(t, client.Writes(), len(frames))
			for i, frame := range frames {
				require.Equal(t, frame.payload, client.Writes()[i].payload)
			}
		})
	}
}

// 后续轮次的无 ID 内容事件使用活动轮次计时，不能被连接级已有首字样本遮蔽。
func TestRelayTTFTIDLessContentOnLaterTurn(t *testing.T) {
	for _, terminalOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal_only_%t", terminalOnly), func(t *testing.T) {
			state := &relayState{}
			base := time.Unix(1000, 0)
			observe := func(ms int, payload string) observedUpstreamEvent {
				return observeUpstreamMessage(state, []byte(payload), base,
					func() time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }, nil, nil)
			}
			state.beginPendingTurn()
			observe(0, `{"type":"response.created","response":{"id":"resp_first"}}`)
			observe(100, `{"type":"response.output_text.delta","response_id":"resp_first","delta":"first"}`)
			observe(200, `{"type":"response.completed","response":{"id":"resp_first"}}`)
			state.beginPendingTurn()
			observe(1000, `{"type":"response.created","response":{"id":"resp_second"}}`)
			terminal := `{"type":"response.completed"}`
			wantFirst := 400
			if terminalOnly {
				terminal = `{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"second"}]}]}}`
				wantFirst = 800
			} else {
				observe(1400, `{"type":"response.output_text.delta","delta":"second"}`)
			}
			result := observe(1800, terminal)
			require.Empty(t, result.responseID)
			require.Equal(t, 800*time.Millisecond, result.duration)
			require.NotNil(t, result.firstToken)
			require.Equal(t, wantFirst, *result.firstToken)
			require.Equal(t, 100, *state.firstTokenMs, "连接级首字仍属于首轮")
			require.False(t, state.hasUnfinishedTurn())
		})
	}
}

// 通过实际 Relay 入口喂入结构/空增量与正文，锁定 WS 和 HTTP 的首内容口径。
func TestRelayTTFTWaitsForVisibleContent(t *testing.T) {
	for _, withContent := range []bool{false, true} {
		name := "empty"
		if withContent {
			name = "content"
		}
		t.Run(name, func(t *testing.T) {
			frames := []passthroughTestFrame{
				{msgType: coderws.MessageText, payload: []byte(`{"type":"response.created","response":{"id":"resp_ttft"}}`)},
				{msgType: coderws.MessageText, payload: []byte(`{"type":"response.output_text.delta","response_id":"resp_ttft","delta":""}`)},
				{msgType: coderws.MessageText, payload: []byte(`{"type":"response.output_text.done","response_id":"resp_ttft","text":""}`)},
			}
			if withContent {
				frames = append(frames, passthroughTestFrame{msgType: coderws.MessageText,
					payload: []byte(`{"type":"response.output_text.delta","response_id":"resp_ttft","delta":"visible"}`)})
			}
			frames = append(frames, passthroughTestFrame{msgType: coderws.MessageText,
				payload: []byte(`{"type":"response.completed","response":{"id":"resp_ttft","usage":{"input_tokens":2,"output_tokens":1}}}`)})
			upstream := newPassthroughTestFrameConn(frames, true)
			client := newPassthroughTestFrameConn(nil, false)
			base := time.Unix(1000, 0)
			var clockMs atomic.Int64
			var turn RelayTurnResult
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, exit := Relay(ctx, client, upstream,
				[]byte(`{"type":"response.create","model":"test-model","input":[]}`),
				RelayOptions{
					Now: func() time.Time { return base.Add(time.Duration(clockMs.Load()) * time.Millisecond) },
					OnUpstreamEvent: func(eventType string, body []byte) {
						if strings.Contains(string(body), `"delta":"visible"`) {
							clockMs.Store(500)
						}
						if eventType == "response.completed" {
							clockMs.Store(800)
						}
					},
					OnTurnComplete: func(r RelayTurnResult) { turn = r },
				})
			require.Nil(t, exit)
			require.EqualValues(t, len(frames), result.UpstreamToClientFrames, "计时修正不应过滤或延迟任何帧")
			require.Equal(t, 2, result.Usage.InputTokens)
			if withContent {
				require.NotNil(t, result.FirstTokenMs)
				require.Equal(t, 500, *result.FirstTokenMs, "应等到正文，而不是 0ms 的空事件或 800ms 的结束事件")
				require.NotNil(t, turn.FirstTokenMs)
			} else {
				require.Nil(t, result.FirstTokenMs)
				require.Nil(t, turn.FirstTokenMs)
			}
		})
	}
}

// 工具发现采用对象参数；WS 必须与 HTTP 同口径，且不得改写任何转发帧。
func TestRelayTTFTToolSearchObjectArguments(t *testing.T) {
	const item = `{"type":"tool_search_call","status":"completed","execution":"client","arguments":{"query":"synthetic tool","limit":1}}`
	for _, mode := range []string{"item_done", "terminal_only", "empty"} {
		t.Run(mode, func(t *testing.T) {
			frames := []passthroughTestFrame{
				{msgType: coderws.MessageText, payload: []byte(`{"type":"response.created","response":{"id":"resp_tool"}}`)},
				{msgType: coderws.MessageText, payload: []byte(`{"type":"response.output_item.added","response_id":"resp_tool","item":{"type":"tool_search_call","status":"in_progress","arguments":{}}}`)},
			}
			output := "[]"
			if mode != "empty" {
				output = "[" + item + "]"
			}
			if mode == "item_done" {
				frames = append(frames, passthroughTestFrame{msgType: coderws.MessageText,
					payload: []byte(`{"type":"response.output_item.done","response_id":"resp_tool","item":` + item + `}`)})
			}
			frames = append(frames, passthroughTestFrame{msgType: coderws.MessageText,
				payload: []byte(`{"type":"response.completed","response":{"id":"resp_tool","output":` + output + `,"usage":{"input_tokens":350,"output_tokens":28}}}`)})
			upstream := newPassthroughTestFrameConn(frames, true)
			client := newPassthroughTestFrameConn(nil, false)
			base := time.Unix(1000, 0)
			var clockMs atomic.Int64
			var turn RelayTurnResult
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, exit := Relay(ctx, client, upstream,
				[]byte(`{"type":"response.create","model":"test-model","input":[]}`),
				RelayOptions{
					Now: func() time.Time { return base.Add(time.Duration(clockMs.Load()) * time.Millisecond) },
					OnUpstreamEvent: func(eventType string, _ []byte) {
						if eventType == "response.output_item.done" {
							clockMs.Store(500)
						}
						if eventType == "response.completed" {
							clockMs.Store(800)
						}
					},
					OnTurnComplete: func(r RelayTurnResult) { turn = r },
				})
			require.Nil(t, exit)
			require.EqualValues(t, len(frames), result.UpstreamToClientFrames)
			require.Equal(t, 350, result.Usage.InputTokens)
			require.Equal(t, 28, result.Usage.OutputTokens)
			written := client.Writes()
			require.Equal(t, frames, written, "统计修正不应改写或丢弃工具调用与用量")
			if mode == "empty" {
				require.Nil(t, result.FirstTokenMs)
				require.Nil(t, turn.FirstTokenMs)
			} else {
				want := 500
				if mode == "terminal_only" {
					want = 800
				}
				require.NotNil(t, result.FirstTokenMs)
				require.Equal(t, want, *result.FirstTokenMs)
				require.Equal(t, result.FirstTokenMs, turn.FirstTokenMs)
			}
		})
	}
}
