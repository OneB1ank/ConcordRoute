package openai_ws_v2

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// 生命周期先于用量观测推进；二进制只结算轮次，文本缺少 ID 仍应回调用量。
func TestCoreSyncTerminalLifecycleBeforeObservation(t *testing.T) {
	for _, kind := range []coderws.MessageType{coderws.MessageText, coderws.MessageBinary} {
		for _, withID := range []bool{false, true} {
			t.Run(fmt.Sprintf("kind_%d/id_%t", kind, withID), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				idField := ""
				if withID {
					idField = `"id":"resp_terminal",`
				}
				terminal := []byte(`{"type":"response.completed","response":{` + idField +
					`"usage":{"input_tokens":7,"output_tokens":3}}}`)
				client := newPassthroughTestFrameConn(nil, false)
				upstream := newPassthroughTestFrameConn([]passthroughTestFrame{
					{msgType: kind, payload: terminal},
				}, true)
				var events []string
				result, exit := Relay(ctx, client, upstream, []byte(`{"type":"response.create"}`), RelayOptions{
					OnTurnSettled: func(coderws.MessageType) { events = append(events, "settled") },
					OnTurnComplete: func(turn RelayTurnResult) {
						events = append(events, "observed")
						require.Equal(t, 7, turn.Usage.InputTokens)
						if !withID {
							require.Empty(t, turn.RequestID)
						}
					},
				})
				require.Nil(t, exit)
				if kind == coderws.MessageText {
					require.Equal(t, []string{"settled", "observed"}, events)
				} else {
					require.Equal(t, []string{"settled"}, events)
					require.Zero(t, result.Usage.InputTokens)
					require.Empty(t, result.RequestID)
				}
				require.Equal(t, terminal, client.Writes()[0].payload)
			})
		}
	}
}

// 覆盖已预发首帧、二进制终止以及无 ID 终止，防止 pending 检查引入误报。
func TestCoreSyncPendingTurnSettlement(t *testing.T) {
	for _, tc := range []struct {
		name       string
		frames     []passthroughTestFrame
		firstSent  bool
		wantFailed bool
	}{
		{name: "already_sent_without_response", firstSent: true, wantFailed: true},
		{name: "binary_terminal_after_created", frames: []passthroughTestFrame{
			{msgType: coderws.MessageText, payload: []byte(`{"type":"response.created","response":{"id":"resp_a"}}`)},
			{msgType: coderws.MessageBinary, payload: []byte(`{"type":"response.completed","response":{"id":"resp_a"}}`)},
		}},
		{name: "idless_terminal_after_created", frames: []passthroughTestFrame{
			{msgType: coderws.MessageText, payload: []byte(`{"type":"response.created","response":{"id":"resp_a"}}`)},
			{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed"}`)},
		}},
		{name: "idless_terminal_before_created", frames: []passthroughTestFrame{
			{msgType: coderws.MessageText, payload: []byte(`{"type":"response.failed"}`)},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			client := newPassthroughTestFrameConn(nil, false)
			upstream := newPassthroughTestFrameConn(tc.frames, true)
			_, exit := Relay(ctx, client, upstream, []byte(`{"type":"response.create","model":"test"}`),
				RelayOptions{FirstMessageSent: tc.firstSent})
			if tc.wantFailed {
				require.NotNil(t, exit)
				require.ErrorContains(t, exit.Err, "before terminal event")
			} else {
				require.Nil(t, exit)
			}
			require.Len(t, client.Writes(), len(tc.frames))
		})
	}
}

// 写观测器用于把第二轮发送精确夹在第一轮下行写完成回调内，无需依赖调度延迟。
type coreSyncWriteObserverConn struct {
	FrameConn
	writes chan struct{}
}

func (c *coreSyncWriteObserverConn) WriteFrame(ctx context.Context, kind coderws.MessageType, data []byte) error {
	if err := c.FrameConn.WriteFrame(ctx, kind, data); err != nil {
		return err
	}
	c.writes <- struct{}{}
	return nil
}

// 第一轮迟到的写完成标记不应把已经重置的第二轮再次标成“已输出”。
func TestCoreSyncLateWriteCompletionDoesNotMarkNextTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := newPassthroughTestFrameConn(nil, false)
	base := newPassthroughTestFrameConn([]passthroughTestFrame{
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"resp_first"}}`)},
	}, false)
	upstream := &coreSyncWriteObserverConn{FrameConn: base, writes: make(chan struct{}, 4)}
	stop := errors.New("stop after second turn")
	var once sync.Once
	states := make(chan bool, 4)
	_, exit := Relay(ctx, client, upstream, []byte(`{"type":"response.create","model":"test"}`), RelayOptions{
		BeforeWriteClient: func(_ coderws.MessageType, data []byte, wrote bool) error {
			states <- wrote
			if string(data) == `{"type":"error"}` {
				return stop
			}
			return nil
		},
		AfterClientWrite: func(_ coderws.MessageType, _ []byte, err error) {
			once.Do(func() {
				<-upstream.writes
				client.readCh <- passthroughTestFrame{msgType: coderws.MessageText, payload: []byte(`{"type":"response.create","model":"test"}`)}
				select {
				case <-upstream.writes:
					base.readCh <- passthroughTestFrame{msgType: coderws.MessageText, payload: []byte(`{"type":"error"}`)}
				case <-ctx.Done():
				}
			})
		},
	})
	require.NotNil(t, exit)
	require.ErrorIs(t, exit.Err, stop)
	require.True(t, exit.WroteDownstream, "连接级诊断仍保留历史输出")
	require.False(t, <-states)
	require.False(t, <-states, "第一轮写完成不得覆盖第二轮状态")
}
