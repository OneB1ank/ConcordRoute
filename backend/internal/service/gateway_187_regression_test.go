package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 通过实际流处理入口复现重试后的阶段时间混用，保护修复后的行为。
func TestGateway187RegressionStagesAcrossFailedAndSuccessfulAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, raw := range []bool{false, true} {
		name := "native"
		if raw {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			InitTTFTStageTiming(c, true)
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			run := func(body string) error {
				r := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
				if raw {
					_, err := svc.handleStreamingResponsePassthrough(context.Background(), r, c, a, time.Now(), "test", "test")
					return err
				}
				_, err := svc.handleStreamingResponse(context.Background(), r, c, a, time.Now(), "test", "test")
				return err
			}
			err := run("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"overloaded\"}}}\n\n")
			if err == nil {
				t.Fatal("第一次尝试必须失败")
			}
			time.Sleep(35 * time.Millisecond)
			err = run("data: {\"type\":\"response.output_text.delta\",\"delta\":\"visible\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			if err != nil {
				t.Fatal(err)
			}
			stages := TTFTStageTimingSnapshot(c, time.Now())
			t.Logf("阶段毫秒：%v", stages)
			if stages["stream_completed"] < stages["first_visible_output"] {
				t.Errorf("最终流完成=%d 早于首内容=%d，混用了失败和成功尝试", stages["stream_completed"], stages["first_visible_output"])
			}
			attempts := TTFTAttemptTimingSnapshots(c)
			if len(attempts) != 2 {
				t.Fatalf("应分别记录失败和成功两次尝试，实际 %d", len(attempts))
			}
			if _, leaked := attempts[0].StagesMS["first_visible_output"]; leaked {
				t.Error("成功尝试的首内容进入了失败尝试")
			}
		})
	}
}

// 单次请求的取消应覆盖证明通道互斥等待，不仅覆盖拿到锁后的 RPC。
func TestGateway187RegressionBridgeQueueHonorsCancellation(t *testing.T) {
	svc := &OpenAIGatewayService{codexAppServerBridges: newCodexAppServerBridgeRegistry()}
	b := &codexAppServerBridge{apiKeyID: 42, connectionID: "review", sessionID: "session", threadID: "thread", closed: make(chan struct{})}
	b.negotiated.Store(true)
	b.initialized.Store(true)
	b.boundAccountID.Store(99)
	if err := svc.codexAppServerBridges.add(b); err != nil {
		t.Fatal(err)
	}
	defer svc.codexAppServerBridges.remove(b)
	b.initLocks()
	if err := b.roundMu.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.bindCodexAppServerAttestationContextForAPIKey(ctx, 42, &Account{ID: 99, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, "session", "thread")
	}()
	select {
	case <-done:
		b.roundMu.Unlock()
	case <-time.After(120 * time.Millisecond):
		t.Error("取消请求在证明互斥锁前继续等待，20ms 截止已超过 100ms")
		b.roundMu.Unlock()
		<-done
	}
}

type regression187DelayedFlush struct {
	gin.ResponseWriter
	textSeen bool
	delayed  bool
	delay    time.Duration
}

func (w *regression187DelayedFlush) Write(p []byte) (int, error) {
	w.textSeen = w.textSeen || strings.Contains(string(p), `"delta":"visible"`) ||
		strings.Contains(string(p), `"content":"visible"`)
	return w.ResponseWriter.Write(p)
}
func (w *regression187DelayedFlush) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}
func (w *regression187DelayedFlush) Flush() {
	if w.textSeen && !w.delayed {
		w.delayed = true
		time.Sleep(w.delay)
	}
	w.ResponseWriter.Flush()
}

// 模拟慢下游 Flush，检查日志是否仍能区分上游内容到达与下游写出。
func TestGateway187RegressionVisibleStageSeparatesDownstreamDelay(t *testing.T) {
	for _, raw := range []bool{false, true} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		c.Writer = &regression187DelayedFlush{ResponseWriter: c.Writer, delay: 120 * time.Millisecond}
		InitTTFTStageTiming(c, true)
		body := "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"summary\":[]}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"visible\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
		resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
		svc := &OpenAIGatewayService{cfg: &config.Config{}}
		a := &Account{ID: 1, Platform: PlatformOpenAI}
		var sample *int
		if raw {
			r, err := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, a, time.Now(), "test", "test")
			if err != nil {
				t.Fatal(err)
			}
			sample = r.firstTokenMs
		} else {
			r, err := svc.handleStreamingResponse(context.Background(), resp, c, a, time.Now(), "test", "test")
			if err != nil {
				t.Fatal(err)
			}
			sample = r.firstTokenMs
		}
		stages := TTFTStageTimingSnapshot(c, time.Now())
		t.Logf("passthrough=%v, TTFT=%v, stages=%v", raw, sample, stages)
		received, present := stages["first_content_received"]
		flushed, flushedPresent := stages["first_content_flush_completed"]
		if !present || !flushedPresent || flushed-received < 100 {
			t.Errorf("passthrough=%v: 未分离上游内容到达与 120ms 下游阻塞", raw)
		}
		if sample == nil || *sample < 100 {
			t.Error("持久化 TTFT 仍应包含网关写出等待")
		}
	}
}

// 最后一个事件只有 EOF 而没有空行时，已有正文应保留首字样本。
func TestGateway187RegressionPassthroughEOFVisibleContentKeepsTTFT(t *testing.T) {
	body := `data: {"type":"response.completed","response":{"id":"resp_eof","output":[{"type":"message","content":[{"type":"output_text","text":"visible"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`
	result, recorder, writer, err := runPassthroughFlushTest(t, io.NopCloser(strings.NewReader(body)), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.Body.String(), "visible") {
		t.Fatal("正文未发送")
	}
	t.Logf("flushes=%v, first_token_nil=%v", writer.flushBodyLengths, result.firstTokenMs == nil)
	if result.firstTokenMs == nil {
		t.Error("正文已在 EOF flush，但返回值中的 FirstTokenMs 仍为空")
	}
}

// 非 OpenAI 账号的通用版本头不应受 Codex 专用保护范围影响。
func TestGateway187RegressionNonOpenAIHeaderOverrideCompatibility(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformGrok} {
		t.Run(platform, func(t *testing.T) {
			account := &Account{
				ID: 1, Platform: platform, Type: AccountTypeAPIKey,
				Credentials: map[string]any{
					credKeyHeaderOverrideEnabled: true,
					credKeyHeaderOverrides: map[string]any{
						"Version":               "service-v2",
						"X-Compatibility-Probe": "present",
					},
				},
			}
			headers := make(http.Header)
			account.ApplyHeaderOverrides(headers)
			t.Logf("非 OpenAI 出站配置：%v", headers)
			if getHeaderRaw(headers, "version") != "service-v2" {
				t.Error("非 OpenAI 的通用 Version 覆写被 Codex 保护全局过滤")
			}
			if err := NormalizeHeaderOverrideCredentialsForPlatform(account.Credentials, account.Platform); err != nil {
				t.Errorf("非 OpenAI 的 Version 配置保存被拒绝：%v", err)
			}
		})
	}
}

// 原始 Chat 使用同一阶段口径，慢下游也不应被算成首内容到达前的等待。
func TestGateway187RegressionRawChatSeparatesDownstreamDelay(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Writer = &regression187DelayedFlush{ResponseWriter: c.Writer, delay: 120 * time.Millisecond}
	InitTTFTStageTiming(c, true)
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"visible\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	result, err := svc.streamRawChatCompletions(c, resp, &Account{ID: 1, Platform: PlatformOpenAI},
		"test", "test", "test", nil, nil, time.Now(), 1)
	require.NoError(t, err)
	require.Equal(t, body, recorder.Body.String())
	require.Equal(t, 3, result.Usage.InputTokens)
	require.NotNil(t, result.FirstTokenMs)
	require.GreaterOrEqual(t, *result.FirstTokenMs, 100)
	stages := TTFTStageTimingSnapshot(c, time.Now())
	require.Contains(t, stages, "first_content_received")
	require.Contains(t, stages, "first_content_flush_completed")
	require.GreaterOrEqual(t, stages["first_content_flush_completed"]-stages["first_content_received"], int64(100))
}

// EOF 仅含用量时维持 nil；正文后的读取错误则保留样本、正文及原错误。
func TestGateway187RegressionEOFFinalizationEdges(t *testing.T) {
	usageOnly := `data: {"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":0},"output":[]}}`
	result, _, writer, err := runPassthroughFlushTest(t, io.NopCloser(strings.NewReader(usageOnly)), -1)
	require.NoError(t, err)
	require.Nil(t, result.firstTokenMs)
	require.Equal(t, 5, result.usage.InputTokens)
	require.Len(t, writer.flushBodyLengths, 1)

	body := `data: {"type":"response.output_text.delta","delta":"visible"}`
	result, recorder, writer, err := runPassthroughFlushTest(t, &passthroughFlushTestErrorBody{
		payload: []byte(body), err: io.ErrUnexpectedEOF,
	}, -1)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.NotNil(t, result.firstTokenMs)
	require.Equal(t, body+"\n", recorder.Body.String())
	require.Len(t, writer.flushBodyLengths, 1)
}
