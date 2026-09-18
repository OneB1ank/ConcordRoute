package service

import (
	"bytes"
	"context"
	"fmt"
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

// 使用合成值核对状态码与长度互不混淆，且摘要不持有凭据。
func TestUpstreamResponseObservation(t *testing.T) {
	for _, code := range []int{200, 201, 429, 502} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			header := http.Header{"X-Codex-Turn-State": {" \tabc\t "}}
			before := header.Clone()
			resp := &http.Response{StatusCode: code, Header: header, Body: observationUnreadBody{}}
			got := observeUpstreamResponse(resp)
			require.Equal(t, code, got.StatusCode)
			require.Equal(t, 3, got.CodexTurnStateBytes)
			require.Equal(t, before, header)
			header.Set("X-Codex-Turn-State", "changed")
			require.Equal(t, 3, got.CodexTurnStateBytes)
		})
	}
	require.Zero(t, observeUpstreamResponse(nil))
	for _, code := range []int{0, 99, 600} {
		require.Zero(t, observeUpstreamResponse(&http.Response{StatusCode: code}))
	}
	for _, state := range []string{
		"",
		" \t",
		strings.Repeat("x", 292),
		strings.Repeat("x", 312),
		strings.Repeat("x", 332),
		strings.Repeat("x", 356),
		"测试",
	} {
		got := observeUpstreamResponse(&http.Response{StatusCode: 200, Header: http.Header{"X-Codex-Turn-State": {state}}})
		require.Equal(t, 200, got.StatusCode)
		require.Equal(t, len(strings.TrimSpace(state)), got.CodexTurnStateBytes)
	}
	// 不假定博客所称 current_turn_state 是 Codex 的回合状态头别名。
	got := observeUpstreamResponse(&http.Response{StatusCode: 200, Header: http.Header{"Current_turn_state": {"opaque"}}})
	require.Zero(t, got.CodexTurnStateBytes)
}

type observationUnreadBody struct{}

func (observationUnreadBody) Read([]byte) (int, error) { panic("观测不应读取正文") }
func (observationUnreadBody) Close() error             { panic("观测不应关闭正文") }

// 验证实际 Forward 接线，覆盖普通/透传、流式/非流式及 compact。
func TestUpstreamResponseObservationForward(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
			for _, stream := range []bool{false, true} {
				if stream && strings.HasSuffix(path, "/compact") {
					continue
				}
				for _, code := range []int{200, 201, 206, 299} {
					t.Run(fmt.Sprintf("raw=%v/path=%s/stream=%v/status=%d", passthrough, path, stream, code), func(t *testing.T) {
						body := []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%v,"instructions":"test","input":[{"role":"user","content":"test"}],"prompt_cache_key":"cache-unchanged"}`, stream))
						c, _ := gin.CreateTestContext(httptest.NewRecorder())
						c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
						c.Request.Header.Set("Content-Type", "application/json")
						c.Request.Header.Set("session_id", "session-unchanged")
						c.Request.Header.Set("x-codex-turn-state", "client-state")
						responseBody := `{"id":"resp_observe","object":"response","status":"completed","output":[],"usage":{"input_tokens":8,"output_tokens":2}}`
						contentType := "application/json"
						// OAuth 透传的普通 Responses 即使下游非流式，上游仍返回 SSE。
						if stream || (passthrough && !strings.HasSuffix(path, "/compact")) {
							contentType = "text/event-stream"
							responseBody = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
								"data: {\"type\":\"response.completed\",\"response\":" + responseBody + "}\n\n"
						}
						upstream := &httpUpstreamRecorder{resp: &http.Response{
							StatusCode: code,
							Header:     http.Header{"Content-Type": {contentType}, "X-Codex-Turn-State": {"upstream-state"}},
							Body:       io.NopCloser(strings.NewReader(responseBody)),
						}}
						svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
						account := &Account{ID: 123, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
							Credentials: map[string]any{"access_token": "synthetic", "chatgpt_account_id": "synthetic"},
							Extra:       map[string]any{"openai_passthrough": passthrough}, Status: StatusActive, Schedulable: true}
						result, err := svc.Forward(context.Background(), c, account, body)
						require.NoError(t, err)
						require.Equal(t, UpstreamResponseObservation{StatusCode: code, CodexTurnStateBytes: 14}, result.UpstreamResponse)
						require.Len(t, upstream.requests, 1, "观测不触发探测或续取")
						require.Equal(t, "client-state", upstream.lastReq.Header.Get("x-codex-turn-state"))
						require.Contains(t, string(upstream.lastBody), `"prompt_cache_key":"cache-unchanged"`)
						require.Equal(t, 8, result.Usage.InputTokens)
						require.Equal(t, 2, result.Usage.OutputTokens)
					})
				}
			}
		}
	}
}

// 最终结果拥有自己的摘要，不从 Gin、全局或旧握手中继承失败尝试信息。
func TestUpstreamResponseObservationIsolationAndUsage(t *testing.T) {
	for _, observation := range []UpstreamResponseObservation{
		{},
		{StatusCode: 200},
		{StatusCode: 200, CodexTurnStateBytes: 292},
		{StatusCode: 200, CodexTurnStateBytes: 312},
		{StatusCode: 200, CodexTurnStateBytes: 332},
		{StatusCode: 200, CodexTurnStateBytes: 356},
	} {
		usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
		svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, &openAIUserGroupRateRepoStub{})
		result := &OpenAIForwardResult{RequestID: "req-observation", Model: "gpt-5.1",
			Usage: OpenAIUsage{InputTokens: 8, OutputTokens: 2}, Duration: time.Second,
			FirstTokenMs: valuePtr(120), UpstreamResponse: observation}
		err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
			Result: result, APIKey: &APIKey{ID: 1002, Group: &Group{RateMultiplier: 1}},
			User: &User{ID: 2002}, Account: &Account{ID: 3002},
		})
		require.NoError(t, err)
		log := usageRepo.lastLog
		require.NotNil(t, log)
		require.Equal(t, 120, *log.FirstTokenMs)
		require.Equal(t, 2, log.OutputTokens)
		if observation.StatusCode == 0 {
			require.Nil(t, log.UpstreamStatusCode)
			require.Nil(t, log.CodexTurnStateBytes)
		} else {
			require.Equal(t, observation.StatusCode, *log.UpstreamStatusCode)
			require.Equal(t, observation.CodexTurnStateBytes, *log.CodexTurnStateBytes)
		}
	}
	// 旧失败或 WS 握手即使带有状态，也不构造新的逐请求 HTTP 观测。
	old := observeUpstreamResponse(&http.Response{StatusCode: 502, Header: http.Header{"X-Codex-Turn-State": {"old"}}})
	final := observeUpstreamResponse(&http.Response{StatusCode: 200})
	require.Equal(t, 502, old.StatusCode)
	require.Equal(t, UpstreamResponseObservation{StatusCode: 200}, final)
	ws := &OpenAIForwardResult{OpenAIWSMode: true, ResponseHeaders: http.Header{"X-Codex-Turn-State": {"old-handshake"}}}
	var log UsageLog
	ws.UpstreamResponse.applyUsageObservation(&log)
	require.Nil(t, log.UpstreamStatusCode)
	require.Nil(t, log.CodexTurnStateBytes)
}

var observationBenchmarkSink UpstreamResponseObservation

// 固定头名读取与整数快照必须零分配，避免默认观测增加每笔 GC 压力。
func TestUpstreamResponseObservationZeroAllocation(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Header: http.Header{"X-Codex-Turn-State": {"synthetic"}}}
	allocations := testing.AllocsPerRun(1000, func() {
		observationBenchmarkSink = observeUpstreamResponse(resp)
	})
	require.Zero(t, allocations)
}

// 异步落库持有独立摘要，不引用可变的原转发结果，更不延长正文生命周期。
func TestUpstreamResponseObservationUsageSnapshot(t *testing.T) {
	observation := UpstreamResponseObservation{StatusCode: 200, CodexTurnStateBytes: 292}
	var log UsageLog
	observation.applyUsageObservation(&log)
	observation.StatusCode, observation.CodexTurnStateBytes = 200, 0
	require.Equal(t, 200, *log.UpstreamStatusCode)
	require.Equal(t, 292, *log.CodexTurnStateBytes)
}

// 通过真实的请求重试循环验证最终响应摘要，而非只比较独立辅助函数返回值。
func TestUpstreamResponseObservationRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.1","stream":false,"instructions":"test","input":[{"type":"reasoning","encrypted_content":"synthetic-old-reasoning"},{"role":"user","content":"test"}]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}, "X-Codex-Turn-State": {"old-state"}},
			Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"invalid reasoning"}}`))},
		{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"resp_retry","object":"response","status":"completed","output":[],"usage":{"input_tokens":8,"output_tokens":2}}`))},
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 123, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "synthetic", "base_url": "https://api.example.com"},
		Status:      StatusActive, Schedulable: true}
	got, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Len(t, upstream.requests, 2)
	require.Equal(t, UpstreamResponseObservation{StatusCode: 200}, got.UpstreamResponse)
	require.NotContains(t, upstream.lastReq.Header.Get("X-Codex-Turn-State"), "old-state")
}

func BenchmarkUpstreamResponseObservation(b *testing.B) {
	for _, size := range []int{
		0,
		292,
		312,
		332,
		356,
		4096,
		65536,
	} {
		b.Run(fmt.Sprintf("state_bytes_%d", size), func(b *testing.B) {
			resp := &http.Response{StatusCode: 200, Header: http.Header{"X-Codex-Turn-State": {strings.Repeat("x", size)}}}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				observationBenchmarkSink = observeUpstreamResponse(resp)
			}
		})
	}
}

var usageObservationBenchmarkSink UsageLog

// 单独衡量 worker 的隔离快照成本，避免把读取头的零分配误称为整条落库链路零分配。
func BenchmarkUpstreamUsageObservation(b *testing.B) {
	for _, code := range []int{0, 200} {
		b.Run(fmt.Sprint(code), func(b *testing.B) {
			observation := UpstreamResponseObservation{StatusCode: code, CodexTurnStateBytes: 292}
			b.ReportAllocs()
			for b.Loop() {
				observation.applyUsageObservation(&usageObservationBenchmarkSink)
			}
		})
	}
}
