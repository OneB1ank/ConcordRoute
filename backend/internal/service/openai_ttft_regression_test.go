package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 分阶段上游只在门槛时间之后输出正文，验证真实转发入口不把元数据计为首 token。
func TestOpenAITTFTCompatibilityStreams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"raw_chat", "responses_chat", "responses_messages"} {
		for _, content := range []bool{false, true} {
			name := route + "/empty"
			if content {
				name = route + "/content"
			}
			t.Run(name, func(t *testing.T) {
				svc := &OpenAIGatewayService{cfg: &config.Config{}}
				reader, writer := io.Pipe()
				defer func() { _ = reader.Close() }()
				finished := make(chan struct{})
				start := time.Now()
				var visibleAt time.Time
				go func() {
					defer close(finished)
					defer func() { _ = writer.Close() }()
					send := func(s string) { _, _ = io.WriteString(writer, "data: "+s+"\n\n") }
					if route == "raw_chat" {
						send(`{"id":"chat_test","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`)
						send(`{"choices":[{"index":0,"delta":{},"finish_reason":null}]}`)
					} else {
						send(`{"type":"response.created","response":{"id":"resp_test","model":"test-model","status":"in_progress"}}`)
						send(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":""}`)
					}
					time.Sleep(80 * time.Millisecond)
					visibleAt = time.Now()
					if route == "raw_chat" {
						if content {
							send(`{"choices":[{"index":0,"delta":{"content":"visible-test"}}]}`)
						}
						send(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
						send(`{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
						send(`[DONE]`)
					} else {
						if content {
							send(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"visible-test"}`)
						}
						send(`{"type":"response.completed","response":{"id":"resp_test","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}}`)
					}
				}()
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}
				account := &Account{ID: 1, Platform: PlatformOpenAI}
				var result *OpenAIForwardResult
				var err error
				switch route {
				case "raw_chat":
					result, err = svc.streamRawChatCompletions(c, resp, account, "test-model", "test-model", "test-model", nil, nil, start, 0)
				case "responses_chat":
					result, err = svc.handleChatStreamingResponse(resp, c, account, "test-model", "test-model", "test-model", start, 0)
				default:
					result, err = svc.handleAnthropicStreamingResponse(resp, c, account, "test-model", "test-model", "test-model", start)
				}
				<-finished
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 2, result.Usage.InputTokens)
				require.Equal(t, 1, result.Usage.OutputTokens)
				if content {
					require.Contains(t, rec.Body.String(), "visible-test")
					require.NotNil(t, result.FirstTokenMs)
					require.GreaterOrEqual(t, *result.FirstTokenMs, int(visibleAt.Sub(start).Milliseconds()),
						"元数据先到达时，首 token 应等待真实内容")
					require.LessOrEqual(t, *result.FirstTokenMs, int(result.Duration.Milliseconds()))
				} else {
					require.Nil(t, result.FirstTokenMs, "纯结构/usage/终止事件应保持无首 token 样本")
				}
			})
		}
	}
}

// 热路径调用真实策略入口，查询次数与模拟的查库耗时都是可观测回归信号。
func TestOpenAIFastPolicyHotPathAvoidsRepeatedReads(t *testing.T) {
	repo := &ttftFastPolicyRepo{value: `{"rules":[]}`}
	svc := &OpenAIGatewayService{settingService: &SettingService{settingRepo: repo}}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for range 8 {
		action, _ := svc.evaluateOpenAIFastPolicy(context.Background(), account, "test-model", "priority")
		require.Equal(t, BetaPolicyActionPass, action)
	}
	require.EqualValues(t, 1, repo.reads.Load(), "同一缓存期的八次策略评估只应读取一次配置")
}

func TestOpenAIChatTTFTContentClassification(t *testing.T) {
	for _, tc := range []struct {
		data string
		want bool
	}{
		{`{"choices":[{"delta":{"role":"assistant","content":null}}]}`, false},
		{`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, false},
		{`{"choices":[],"usage":{"completion_tokens":2}}`, false},
		{`{"choices":[{"delta":{"tool_calls":[{"id":"call_test","function":{"name":"test"}}]}}]}`, false},
		{`{"choices":[{"delta":{"content":" "}}]}`, true},
		{`{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`, true},
		{`{"choices":[{"delta":{"reasoning":"thinking"}}]}`, true},
		{`{"choices":[{"delta":{"refusal":"refused"}}]}`, true},
		{`{"choices":[{"delta":{"audio":{"transcript":"test"}}}]}`, true},
		{`{"choices":[{"delta":{"function_call":{"arguments":"{}"}}}]}`, true},
		{`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}`, true},
		{`invalid json`, false},
	} {
		require.Equal(t, tc.want, openAIChatStreamHasVisibleOutput(tc.data), tc.data)
	}
}

// 用 Flush 信号验证真实流转发，不以最终拼接后的响应体冒充“已经及时发送”。
type ttftFlushObserver struct {
	gin.ResponseWriter
	sawContent bool
	flushed    chan struct{}
	once       sync.Once
}

func (w *ttftFlushObserver) Write(p []byte) (int, error) {
	w.sawContent = w.sawContent || strings.Contains(string(p), "visible-test")
	return w.ResponseWriter.Write(p)
}

func (w *ttftFlushObserver) WriteString(s string) (int, error) {
	w.sawContent = w.sawContent || strings.Contains(s, "visible-test")
	return w.ResponseWriter.WriteString(s)
}

func (w *ttftFlushObserver) Flush() {
	w.ResponseWriter.Flush()
	if w.sawContent {
		w.once.Do(func() { close(w.flushed) })
	}
}

func TestOpenAITTFTStreamsFlushBeforeCompletion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"raw_chat", "responses_chat", "responses_messages", "responses_native", "responses_passthrough"} {
		t.Run(route, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer func() { _ = reader.Close() }()
			release := make(chan struct{})
			var releaseOnce sync.Once
			finish := func() { releaseOnce.Do(func() { close(release) }) }
			defer finish()
			upstreamDone := make(chan struct{})
			go func() {
				defer close(upstreamDone)
				defer func() { _ = writer.Close() }()
				send := func(s string) { _, _ = io.WriteString(writer, "data: "+s+"\n\n") }
				if route == "raw_chat" {
					send(`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`)
					send(`{"choices":[{"index":0,"delta":{"content":"visible-test"}}]}`)
				} else {
					send(`{"type":"response.created","response":{"id":"resp_test","status":"in_progress"}}`)
					send(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"visible-test"}`)
				}
				<-release
				if route == "raw_chat" {
					send(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
					send(`[DONE]`)
				} else {
					send(`{"type":"response.completed","response":{"id":"resp_test","output":[],"usage":{"input_tokens":2,"output_tokens":1}}}`)
				}
			}()
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIFirstOutputTimeoutSeconds: 1}}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			observer := &ttftFlushObserver{ResponseWriter: c.Writer, flushed: make(chan struct{})}
			c.Writer = observer
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}
			account := &Account{ID: 1, Platform: PlatformOpenAI}
			errs := make(chan error, 1)
			go func() {
				start := time.Now()
				var err error
				switch route {
				case "raw_chat":
					_, err = svc.streamRawChatCompletions(c, resp, account, "test-model", "test-model", "test-model", nil, nil, start, 0)
				case "responses_chat":
					_, err = svc.handleChatStreamingResponse(resp, c, account, "test-model", "test-model", "test-model", start, 0)
				case "responses_messages":
					_, err = svc.handleAnthropicStreamingResponse(resp, c, account, "test-model", "test-model", "test-model", start)
				case "responses_native":
					_, err = svc.handleStreamingResponse(context.Background(), resp, c, account, start, "test-model", "test-model")
				default:
					_, err = svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, start, "test-model", "test-model")
				}
				errs <- err
			}()
			select {
			case <-observer.flushed:
				// 上游尚未获准发送 completed；这里收到信号才证明内容没有等到结束才释放。
			case <-time.After(3 * time.Second):
				finish()
				t.Fatal("上游结束前未观察到内容 Flush")
			}
			finish()
			require.NoError(t, <-errs)
			<-upstreamDone
			require.Contains(t, rec.Body.String(), "visible-test")
		})
	}
}
