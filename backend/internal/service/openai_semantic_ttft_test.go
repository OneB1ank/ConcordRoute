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

// 真实流处理入口应同时保留早到的语义事件和晚到的正文，转发内容及诊断不互相覆盖。
func TestOpenAISemanticTTFTHTTP(t *testing.T) {
	for _, raw := range []bool{false, true} {
		name := "native"
		if raw {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			InitTTFTStageTiming(c, true)
			reader, writer := io.Pipe()
			defer func() { _ = reader.Close() }()
			created := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_semantic\"}}\n\n"
			empty := "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"summary\":[]}}\n\n"
			content := "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"summary\"}\n\n"
			// 提供完整终态，避免触发原有的缺失 output 重建逻辑，才能逐字节比较转发结果。
			terminal := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_semantic\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3},\"output\":[{\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"summary\"}]}]}}\n\n"
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() { _ = writer.Close() }()
				_, _ = io.WriteString(writer, created+empty)
				time.Sleep(160 * time.Millisecond)
				_, _ = io.WriteString(writer, content+terminal)
			}()
			started := time.Now()
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			var semantic, visible *int
			var usage *OpenAIUsage
			if raw {
				result, err := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, started, "test", "test")
				require.NoError(t, err)
				semantic, visible, usage = result.semanticFirstTokenMs, result.firstTokenMs, result.usage
			} else {
				result, err := svc.handleStreamingResponse(context.Background(), resp, c, account, started, "test", "test")
				require.NoError(t, err)
				semantic, visible, usage = result.semanticFirstTokenMs, result.firstTokenMs, result.usage
			}
			<-done
			require.NotNil(t, semantic)
			require.NotNil(t, visible)
			require.GreaterOrEqual(t, *visible-*semantic, 120, "首内容仍晚于首语义结构")
			require.Equal(t, 2, usage.InputTokens)
			require.Equal(t, 3, usage.OutputTokens)
			require.Equal(t, created+empty+content+terminal, recorder.Body.String(), "只新增计时，不改写流")
			stages := TTFTStageTimingSnapshot(c, time.Now())
			require.GreaterOrEqual(t, stages["first_content_received"]-int64(*semantic), int64(120))
		})
	}
}

// 通过公开 Forward 验证两个 HTTP 分支都把新增样本带回，而不是仅在流处理函数内生效。
func TestOpenAISemanticTTFTForward(t *testing.T) {
	for _, raw := range []bool{false, true} {
		name := "native"
		if raw {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("User-Agent", "codex_cli_rs/0.153.3")
			sse := "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"summary\":[]}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_semantic_forward\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(sse)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{
				ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"},
				Extra:       map[string]any{"openai_passthrough": raw},
			}
			result, err := svc.Forward(context.Background(), c, account,
				[]byte(`{"model":"gpt-5.1","stream":true,"input":[{"role":"user","content":"test"}]}`))
			require.NoError(t, err)
			require.NotNil(t, result.SemanticFirstTokenMs)
			require.Nil(t, result.FirstTokenMs, "空推理结构不应成为调度用的真实首内容")
			require.NotNil(t, result.FirstResponseMs)
			require.LessOrEqual(t, *result.FirstResponseMs, *result.SemanticFirstTokenMs)
			require.Equal(t, result.FirstResponseMs, result.usageFirstTokenMs(account))
			require.Equal(t, 1, result.Usage.OutputTokens)
			require.NotNil(t, upstream.lastReq)
		})
	}
}

// WS 使用 HTTP 回源时也必须将语义样本从桥接函数带回，不依赖 WS 中继实现。
func TestOpenAISemanticTTFTHTTPBridge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	sse := "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"summary\":[]}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_semantic_bridge\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(sse)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	payload := []byte(`{"type":"response.create","model":"gpt-5.1","stream":true,"input":"test"}`)
	var frames [][]byte
	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "test-token",
		payload, len(payload), "gpt-5.1", "gpt-5.1", "", "", "", "", 1,
		func(frame []byte) error {
			frames = append(frames, append([]byte(nil), frame...))
			return nil
		})
	require.NoError(t, err)
	require.NotNil(t, result.SemanticFirstTokenMs)
	require.Nil(t, result.FirstTokenMs)
	require.NotNil(t, result.FirstResponseMs)
	require.LessOrEqual(t, *result.FirstResponseMs, *result.SemanticFirstTokenMs)
	require.Equal(t, result.FirstResponseMs, result.usageFirstTokenMs(account))
	require.Len(t, frames, 2)
}

// 最终入库选择语义首字，但不得修改调度仍在读取的 ForwardResult 首内容样本。
func TestOpenAISemanticTTFTUsageOnly(t *testing.T) {
	for _, tc := range []struct {
		name, platform string
		stream, ws     bool
		semantic       *int
		want           int
	}{
		{"responses", PlatformOpenAI, true, false, valuePtr(500), 500},
		{"websocket", PlatformOpenAI, false, true, valuePtr(500), 500},
		{"zero_is_observed", PlatformOpenAI, true, false, valuePtr(0), 0},
		{"legacy_content", PlatformOpenAI, true, false, nil, 8000},
		{"other_platform", PlatformGrok, true, false, valuePtr(500), 8000},
		{"non_streaming", PlatformOpenAI, false, false, valuePtr(500), 8000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &openAIRecordUsageLogRepoStub{inserted: true}
			svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(repo,
				&openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}},
				&openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
			result := &OpenAIForwardResult{
				RequestID: "resp_semantic_usage", Model: "gpt-5.1", Duration: 9 * time.Second,
				Stream: tc.stream, OpenAIWSMode: tc.ws,
				FirstTokenMs: valuePtr(8000), SemanticFirstTokenMs: tc.semantic,
			}
			require.NoError(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
				Result: result,
				APIKey: &APIKey{ID: 1000, Group: &Group{RateMultiplier: 1}},
				User:   &User{ID: 2000}, Account: &Account{ID: 3000, Platform: tc.platform, Type: AccountTypeAPIKey},
				APIKeyService: &openAIRecordUsageAPIKeyQuotaStub{},
			}))
			require.NotNil(t, repo.lastLog)
			require.Equal(t, tc.want, *repo.lastLog.FirstTokenMs)
			require.Equal(t, 8000, *result.FirstTokenMs)
			require.Equal(t, 9000, *repo.lastLog.DurationMs)
			require.Zero(t, repo.lastLog.TotalCost)
		})
	}
}

// 纯状态/用量流不应为了显示而捏造首语义样本，EOF 同样保持缺省。
func TestOpenAISemanticTTFTStatusOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, raw := range []bool{false, true} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		svc := &OpenAIGatewayService{cfg: &config.Config{}}
		body := "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
		account := &Account{ID: 1, Platform: PlatformOpenAI}
		if raw {
			result, err := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "test", "test")
			require.NoError(t, err)
			require.NotNil(t, result.firstResponseMs)
			require.Nil(t, result.semanticFirstTokenMs)
			require.Nil(t, result.firstTokenMs)
		} else {
			result, err := svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "test", "test")
			require.NoError(t, err)
			require.NotNil(t, result.firstResponseMs)
			require.Nil(t, result.semanticFirstTokenMs)
			require.Nil(t, result.firstTokenMs)
		}
	}
}
