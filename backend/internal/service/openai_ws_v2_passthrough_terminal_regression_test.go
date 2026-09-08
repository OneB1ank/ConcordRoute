package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 完整适配链路逐轮校验请求、模型和用量快照，避免只放行下一轮却仍沿用上一轮请求。
func TestOpenAIWSPassthroughTerminalKeepsTurnSnapshots(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []coderws.MessageType{coderws.MessageText, coderws.MessageBinary} {
		for _, withID := range []bool{false, true} {
			t.Run(fmt.Sprintf("kind_%d/id_%t", kind, withID), func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(context.Canceled)
				upstream := newStagedPassthroughConn()
				svc := newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream)
				captures := make(chan OpenAIWSTurnCapture, 4)
				turnNumbers := make(chan int, 4)
				hooks := &OpenAIWSIngressHooks{
					BeforeRequest: func(turn int, payload []byte, _, _ string) ([]byte, error) {
						if turn > 1 {
							turnNumbers <- turn
						}
						return payload, nil
					},
					AfterTurn: func(capture OpenAIWSTurnCapture) { captures <- capture },
				}
				server, _ := startPassthroughLifecycleServer(t, ctx, svc, passthroughLifecycleAccount(), hooks)
				defer server.Close()
				client := dialPassthroughLifecycleClient(t, server)
				defer func() { _ = client.CloseNow() }()
				require.NotEmpty(t, requirePassthroughUpstreamWrite(t, upstream, time.Second))
				if kind == coderws.MessageText {
					upstream.Send(`{"type":"response.created","response":{"id":"resp_first"}}`)
					_, err := readPassthroughLifecycleFrame(t, client, time.Second)
					require.NoError(t, err)
					upstream.Send(`{"type":"response.output_text.delta","response_id":"resp_first","delta":"visible"}`)
					_, err = readPassthroughLifecycleFrame(t, client, time.Second)
					require.NoError(t, err)
				}
				idField := ""
				if withID {
					idField = `"id":"resp_first",`
				}
				firstTerminal := []byte(`{"type":"response.completed","response":{` + idField +
					`"model":"gpt-5.1","usage":{"input_tokens":11,"output_tokens":2}}}`)
				upstream.frames <- stagedPassthroughFrame{messageType: kind, payload: firstTerminal}
				first, err := readPassthroughLifecycleFrame(t, client, time.Second)
				require.NoError(t, err)
				require.JSONEq(t, string(firstTerminal), string(first))
				if kind == coderws.MessageText {
					select {
					case capture := <-captures:
						require.Equal(t, 1, capture.Turn)
						require.Equal(t, 11, capture.Result.Usage.InputTokens)
						require.NotNil(t, capture.Result.FirstTokenMs, "终态缺少 ID 仍保留本轮可见输出的计时")
						if !withID {
							require.Empty(t, capture.Result.RequestID, "缺失的上游 ID 保持空值")
						}
					case <-time.After(time.Second):
						t.Fatal("文本首轮用量未结算")
					}
				} else {
					select {
					case capture := <-captures:
						require.Equal(t, 1, capture.Turn)
						require.Nil(t, capture.Result, "只释放本轮资源，不伪造二进制帧用量")
					case <-time.After(time.Second):
						t.Fatal("二进制首轮未通知 AfterTurn，处理器占用的并发槽位未及时释放")
					}
				}

				writeCtx, cancelWrite := context.WithTimeout(ctx, time.Second)
				err = client.Write(writeCtx, coderws.MessageText,
					[]byte(`{"type":"response.create","model":"gpt-5.2","input":"second turn marker"}`))
				cancelWrite()
				require.NoError(t, err)
				next := requirePassthroughUpstreamWrite(t, upstream, time.Second)
				require.Equal(t, "second turn marker", gjson.GetBytes(next, "input").String())
				select {
				case turn := <-turnNumbers:
					require.Equal(t, 2, turn)
				case <-time.After(time.Second):
					t.Fatal("第二轮请求回调未触发")
				}
				upstream.Send(`{"type":"response.completed","response":{"id":"resp_second","model":"gpt-5.2","usage":{"input_tokens":23,"output_tokens":3}}}`)
				second, err := readPassthroughLifecycleFrame(t, client, time.Second)
				require.NoError(t, err)
				require.Equal(t, "gpt-5.2", gjson.GetBytes(second, "response.model").String())
				select {
				case capture := <-captures:
					require.Equal(t, 2, capture.Turn)
					require.Equal(t, "gpt-5.2", capture.OriginalModel)
					require.Equal(t, "second turn marker", gjson.GetBytes(capture.RequestBody, "input").String())
					require.Equal(t, 23, capture.Result.Usage.InputTokens)
					require.Equal(t, "resp_second", capture.Result.RequestID)
				case <-time.After(time.Second):
					t.Fatal("第二轮用量未结算")
				}
			})
		}
	}
}

// 所有已接受的终态都解除首输出/活跃读取超时，不遗留上一轮的定时器。
func TestOpenAIWSPassthroughTerminalDisarmsDeadline(t *testing.T) {
	for _, kind := range []coderws.MessageType{coderws.MessageText, coderws.MessageBinary} {
		for _, event := range []string{"response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled"} {
			t.Run(fmt.Sprintf("kind_%d/%s", kind, event), func(t *testing.T) {
				conn := &openAIWSPassthroughFirstOutputFrameConn{
					resolveDeadline: func([]byte) openAIWSPassthroughFirstOutputDeadline {
						return openAIWSPassthroughFirstOutputDeadline{timeout: time.Second}
					},
				}
				conn.armDeadline([]byte(`{"type":"response.create"}`))
				require.True(t, conn.deadlineState().armed)
				conn.observeUpstreamActivity(kind, []byte(`{"type":"`+event+`"}`))
				require.False(t, conn.deadlineState().armed)
			})
		}
	}
}
