package service

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/pkg/ctxkey"
	"github.com/TokenFlux/TokenRouter/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// transcriptionForm 只构造合成上传，不访问上游或真实录音。
func transcriptionForm(t *testing.T, fields map[string]string, files int) ([]byte, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for k, v := range fields {
		require.NoError(t, w.WriteField(k, v))
	}
	for i := 0; i < files; i++ {
		p, err := w.CreateFormFile("file", "sample.wav")
		require.NoError(t, err)
		_, err = p.Write([]byte("synthetic audio"))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return b.Bytes(), w.FormDataContentType()
}

func TestAudioTranscriptionMultipartContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fields  map[string]string
		files   int
		wantErr bool
	}{
		{"valid", map[string]string{"model": "gpt-transcribe", "language": "zh"}, 1, false},
		{"missing file", map[string]string{"model": "gpt-transcribe"}, 0, true},
		{"duplicate file", map[string]string{"model": "gpt-transcribe"}, 2, true},
		{"missing model", nil, 1, true},
		{"streaming", map[string]string{"model": "gpt-transcribe", "stream": "true"}, 1, true},
		{"invalid stream", map[string]string{"model": "gpt-transcribe", "stream": "maybe"}, 1, true},
		{"field overflow", map[string]string{"model": string(bytes.Repeat([]byte("x"), 65537))}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, contentType := transcriptionForm(t, tc.fields, tc.files)
			got, err := ParseOpenAIAudioTranscriptionRequest(contentType, body, false)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "gpt-transcribe", got.Model)
			require.Equal(t, "zh", got.Language)
		})
	}
}

func TestAudioTranscriptionDesktopDefaultAndCapability(t *testing.T) {
	body, contentType := transcriptionForm(t, nil, 1)
	p, err := ParseOpenAIAudioTranscriptionRequest(contentType, body, true)
	require.NoError(t, err)
	require.Equal(t, "gpt-transcribe", p.Model)
	for _, kind := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		a := &Account{Platform: PlatformOpenAI, Type: kind}
		require.True(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAudioTranscriptions))
		a.Platform = PlatformAnthropic
		require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAudioTranscriptions))
	}
}

func TestAudioTranscriptionForwardOAuthIdentityAndAudio(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body, ct := transcriptionForm(t, map[string]string{"model": "gpt-transcribe", "language": "zh"}, 1)
	p, err := ParseOpenAIAudioTranscriptionRequest(ct, body, false)
	require.NoError(t, err)
	p.DurationSeconds = 3.5
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/audio/transcriptions?collector_token=do-not-forward", bytes.NewReader(body))
	c.Request.Header.Set("Authorization", "Bearer client-key")
	c.Request.Header.Set("X-Oai-Attestation", "do-not-replay")
	c.Request.Header.Set("Session_ID", "client-session")
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200,
		Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"text":"测试转录"}`))}}
	s := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, tlsFPProfileService: &TLSFingerprintProfileService{}}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "upstream-token", "chatgpt_account_id": "account-42"},
		Proxy:       &Proxy{Protocol: "socks5", Host: "proxy.invalid", Port: 1080},
		Extra:       map[string]any{"enable_tls_fingerprint": true}}
	const ua = "codex-tui/0.153.3 (Windows 10.0.19045; x86_64) WindowsTerminal (codex-tui; 0.153.3)"
	result, err := s.ForwardAudioTranscription(context.Background(), c, account, p, p.Model,
		TLSFingerprintRouterMatchResult{Matched: true, UpstreamUserAgent: ua, UpstreamOriginator: "codex-tui"})
	require.NoError(t, err)
	require.JSONEq(t, `{"text":"测试转录"}`, recorder.Body.String())
	require.Equal(t, "https://chatgpt.com/backend-api/transcribe", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer upstream-token", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "account-42", upstream.lastReq.Header.Get("Chatgpt-Account-Id"))
	require.Equal(t, ua, upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, "0.153.3", upstream.lastReq.Header.Get("Version"))
	require.Equal(t, "codex-tui", upstream.lastReq.Header.Get("Originator"))
	require.Equal(t, "socks5://proxy.invalid:1080", upstream.lastProxyURL)
	require.NotNil(t, upstream.lastTLSProfile)
	require.Empty(t, upstream.lastReq.Header.Get("Session_ID"))
	require.Empty(t, upstream.lastReq.Header.Get("X-Oai-Attestation"))
	req := httptest.NewRequest("POST", "/", bytes.NewReader(upstream.lastBody))
	req.Header.Set("Content-Type", upstream.lastReq.Header.Get("Content-Type"))
	require.NoError(t, req.ParseMultipartForm(1<<20))
	require.Equal(t, "zh", req.FormValue("language"))
	require.Empty(t, req.FormValue("model"))
	require.Empty(t, result.UpstreamModel)
	require.Equal(t, "stt", result.AudioUsage.Mode)
	require.InDelta(t, 3.5/3600, result.AudioUsage.DurationOrUnits, 1e-12)
	require.Empty(t, account.Extra["codex_fingerprint_bindings"], "转录不生成推理绑定")
}

func TestAudioTranscriptionAPIKeyMappingAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		response string
		success  bool
	}{
		{"valid", 200, `{"text":"ok"}`, true},
		{"silence", 200, `{"text":""}`, true},
		{"HTML fallback", 200, `<html>SPA</html>`, false},
		{"non-string", 200, `{"text":7}`, false},
		{"auth error", 401, `{"secret":"never return"}`, false},
		{"forbidden", 403, `{"secret":"never return"}`, false},
		{"rate limit", 429, `{"secret":"never return"}`, false},
		{"server error", 502, `{"secret":"never return"}`, false},
		{"redirect", 302, `{"text":"not followed"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, ct := transcriptionForm(t, map[string]string{"model": "client-alias"}, 1)
			p, err := ParseOpenAIAudioTranscriptionRequest(ct, body, false)
			require.NoError(t, err)
			p.DurationSeconds = 2
			c, w := func() (*gin.Context, *httptest.ResponseRecorder) {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/v1/audio/transcriptions", bytes.NewReader(body))
				return c, w
			}()
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.response))}}
			s := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			a := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "upstream-key"}}
			result, err := s.ForwardAudioTranscription(context.Background(), c, a, p, "gpt-transcribe", TLSFingerprintRouterMatchResult{})
			require.Len(t, upstream.requests, 1, "不自动跨账号重新上传")
			if !tc.success {
				require.Error(t, err)
				require.Nil(t, result)
				require.Empty(t, w.Body.String())
				return
			}
			require.NoError(t, err)
			require.Equal(t, "https://api.openai.com/v1/audio/transcriptions", upstream.lastReq.URL.String())
			require.Equal(t, "Bearer upstream-key", upstream.lastReq.Header.Get("Authorization"))
			require.Equal(t, "gpt-transcribe", result.UpstreamModel)
			req := httptest.NewRequest("POST", "/", bytes.NewReader(upstream.lastBody))
			req.Header.Set("Content-Type", upstream.lastReq.Header.Get("Content-Type"))
			require.NoError(t, req.ParseMultipartForm(1<<20))
			require.Equal(t, "gpt-transcribe", req.FormValue("model"))
			require.JSONEq(t, tc.response, w.Body.String())
		})
	}
}

func TestAudioTranscriptionCancelledBeforeUpload(t *testing.T) {
	s := &OpenAIGatewayService{httpUpstream: &httpUpstreamRecorder{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	_, err := s.ForwardAudioTranscription(ctx, c, &Account{}, &OpenAIAudioTranscriptionRequest{DurationSeconds: 1}, "", TLSFingerprintRouterMatchResult{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestAudioTranscriptionSTTBilling(t *testing.T) {
	price := 3.6
	s := &OpenAIGatewayService{billingService: &BillingService{}}
	result := &OpenAIForwardResult{Model: "gpt-transcribe", AudioUsage: &AudioUsage{Mode: "stt", DurationOrUnits: 2.5 / 3600}}
	cost, err := s.calculateOpenAIRecordUsageCost(context.Background(), result,
		&APIKey{Group: &Group{ID: 1, AudioSTTPricePerHour: &price}}, []string{"gpt-transcribe"},
		1, 1, 1, 2, UsageTokens{}, "")
	require.NoError(t, err)
	require.InDelta(t, 0.0025, cost.TotalCost, 1e-10)
	require.InDelta(t, 0.005, cost.ActualCost, 1e-10)
}

func TestAudioTranscriptionGroupSnapshotAndDuplicate(t *testing.T) {
	for _, audio := range []bool{false, true} {
		for _, live := range []bool{false, true} {
			g := &Group{ID: 1, Platform: PlatformOpenAI, AllowLive: live, AllowAudioTranscription: audio}
			roundtrip := groupFromAuthSnapshot(authGroupSnapshotFromGroup(g))
			require.Equal(t, audio, roundtrip.AllowAudioTranscription)
			require.Equal(t, live, roundtrip.AllowLive)
			clone := cloneGroupForDuplicate(g, "audio-test")
			require.Equal(t, audio, clone.AllowAudioTranscription)
			require.Equal(t, live, clone.AllowLive)
		}
	}
}

func TestAudioTranscriptionDefaultModelNotGuessedFromAudio(t *testing.T) {
	body, ct := transcriptionForm(t, nil, 1)
	body = bytes.ReplaceAll(body, []byte("synthetic audio"), []byte(`name="model"`))
	p, err := ParseOpenAIAudioTranscriptionRequest(ct, body, true)
	require.NoError(t, err)
	b, outCT, err := p.upstreamBody(false, p.Model)
	require.NoError(t, err)
	req := httptest.NewRequest("POST", "/", bytes.NewReader(b))
	req.Header.Set("Content-Type", outCT)
	require.NoError(t, req.ParseMultipartForm(1<<20))
	require.Equal(t, "gpt-transcribe", req.FormValue("model"))
	empty, emptyCT := transcriptionForm(t, map[string]string{"model": ""}, 1)
	_, err = ParseOpenAIAudioTranscriptionRequest(emptyCT, empty, true)
	require.Error(t, err, "显式空模型不得被默认值遮蔽")
}

// blockingAudioUpstream 在上传阶段等待取消，用于检查真实转发请求携带的预算。
type blockingAudioUpstream struct{ httpUpstreamRecorder }

func (u *blockingAudioUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestAudioTranscriptionInflightCancellation(t *testing.T) {
	body, ct := transcriptionForm(t, map[string]string{"model": "gpt-transcribe"}, 1)
	p, err := ParseOpenAIAudioTranscriptionRequest(ct, body, false)
	require.NoError(t, err)
	p.DurationSeconds = 1
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(body))
	s := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: &blockingAudioUpstream{}}
	a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "synthetic-key"}}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result, err := s.ForwardAudioTranscription(ctx, c, a, p, p.Model, TLSFingerprintRouterMatchResult{})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, result)
	require.False(t, c.Writer.Written())
}

func TestAudioTranscriptionBillingIDOverridesReusedClientID(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "reused-client-id")
	require.Equal(t, "openai_audio:sample-1", resolveUsageBillingRequestID(ctx, "openai_audio:sample-1"))
	require.Equal(t, "openai_audio:sample-2", resolveUsageBillingRequestID(ctx, "openai_audio:sample-2"))
}

func TestAudioTranscriptionUsageLogMatchesSTTRate(t *testing.T) {
	repo := &openAIRecordUsageLogRepoStub{inserted: true}
	s := newOpenAIRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	s.usageBillingNow = func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) }
	groupID, price := int64(1), 3.6
	err := s.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{RequestID: "openai_audio:measured", Model: "gpt-transcribe",
			AudioUsage: &AudioUsage{Mode: "stt", DurationOrUnits: 2.5 / 3600}},
		APIKey: &APIKey{ID: 2, GroupID: &groupID, Group: &Group{ID: groupID, Platform: PlatformOpenAI,
			RateMultiplier: 2, AudioSTTPricePerHour: &price, PeakRateEnabled: true, PeakStart: "11:59", PeakEnd: "12:01", PeakRateMultiplier: 3}},
		User: &User{ID: 3}, Account: &Account{ID: 4, Platform: PlatformOpenAI},
	})
	require.NoError(t, err)
	require.NotNil(t, repo.lastLog)
	require.InDelta(t, 0.0025, repo.lastLog.TotalCost, 1e-10)
	require.InDelta(t, 0.005, repo.lastLog.ActualCost, 1e-10)
	require.Equal(t, 2.0, repo.lastLog.RateMultiplier, "按时长费用不应显示 token 高峰倍率")
	// 同时核对送入事务层的倍率，避免只修显示而实际结算仍叠加高峰。
	cmd := requireOpenAIRecordUsageBillingRepoStub(t, s).lastCmd
	require.NotNil(t, cmd)
	require.InDelta(t, 0.005, cmd.BillableAmountUSD, 1e-10)
	require.Equal(t, 2.0, cmd.BalanceRateMultiplier)
	require.Equal(t, 2.0, cmd.SubscriptionRateMultiplier)
}
