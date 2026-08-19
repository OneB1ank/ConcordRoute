package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardResponsesInputTokensCustomRelayUsesLocalEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"gpt-5.4","instructions":"Be concise.","input":"hello world","metadata":{"trace":"kept-local"}}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", bytes.NewReader(body))

	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID: 159, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "relay-key", "base_url": "https://relay.example/v1"},
	}

	err := svc.ForwardResponsesInputTokens(t.Context(), c, account, body)

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "response.input_tokens", gjson.Get(recorder.Body.String(), "object").String())
	require.Positive(t, gjson.Get(recorder.Body.String(), "input_tokens").Int())
	require.Nil(t, upstream.lastReq, "兼容中转应直接使用本地估算")
}

func TestForwardResponsesInputTokensGrokOAuthUsesLocalEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"grok-4.1","input":"hello world"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/responses/input_tokens", bytes.NewReader(body))

	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{ID: 160, Platform: PlatformGrok, Type: AccountTypeOAuth}

	err := svc.ForwardResponsesInputTokens(t.Context(), c, account, body)

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Positive(t, gjson.Get(recorder.Body.String(), "input_tokens").Int())
	require.Nil(t, upstream.lastReq)
}

func TestForwardResponsesInputTokensOAuthUsesCodexTLSIdentityAndProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const macUA = "codex-tui/0.200.1 (Mac OS X 15.6; arm64) Terminal.app (codex-tui; 0.200.1)"
	withCodexCanonicalUA(t, macUA)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"gpt-5.4","input":"hello","metadata":{"client_field":"preserved"}}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses/input_tokens", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", "native-input-tokens-client/1.0")

	profileID := int64(181)
	profileService := &TLSFingerprintProfileService{localCache: map[int64]*model.TLSFingerprintProfile{
		profileID: {ID: profileID, Name: "native input tokens macOS TLS", ALPNProtocols: []string{"h2", "http/1.1"}},
	}}
	routerService := newTLSFingerprintRouterTestService(&model.TLSFingerprintRouter{
		ID: 118, Enabled: true,
		Rules: []model.TLSFingerprintRouterRule{{
			Enabled: true, Pattern: "native-input-tokens-client/",
			TLSFingerprintProfileID: profileID, UpstreamUserAgent: macUA,
		}},
	})
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"object":"response.input_tokens","input_tokens":11}`)),
	}}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{}, httpUpstream: upstream,
		tlsFPProfileService: profileService, tlsFPRouterService: routerService,
	}
	proxyID := int64(182)
	account := &Account{
		ID: 203, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 2,
		ProxyID: &proxyID, Proxy: &Proxy{ID: proxyID, Protocol: "http", Host: "proxy.example", Port: 8080},
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra: map[string]any{
			"enable_tls_fingerprint": true, "tls_fingerprint_router_id": int64(118),
		},
	}

	err := svc.ForwardResponsesInputTokens(context.Background(), c, account, body)

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"object":"response.input_tokens","input_tokens":11}`, recorder.Body.String())
	require.Equal(t, "http://proxy.example:8080", upstream.lastProxyURL)
	require.NotNil(t, upstream.lastTLSProfile)
	require.Equal(t, "native input tokens macOS TLS", upstream.lastTLSProfile.Name)
	require.Equal(t, macUA, upstream.lastReq.Header.Get("User-Agent"))
	_, expectedOriginator := CodexAuthIdentityForUserAgent(macUA)
	require.Equal(t, expectedOriginator, upstream.lastReq.Header.Get("Originator"))
	require.Equal(t, CodexCanonicalClientVersion(), upstream.lastReq.Header.Get("Version"))
	require.Equal(t, "preserved", gjson.GetBytes(upstream.lastBody, "metadata.client_field").String())

	backgroundUA, backgroundMatch := svc.resolveOpenAIBackgroundIdentity(account)
	require.Equal(t, macUA, backgroundUA)
	require.True(t, backgroundMatch.Matched)
	require.Equal(t, profileID, backgroundMatch.TLSFingerprintProfileID)
}

func TestForwardResponsesInputTokensUpstream404FallsBackWithoutAccountPenalty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"gpt-5.4","instructions":"Be concise.","input":"hello world"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", bytes.NewReader(body))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"Invalid URL (POST /v1/responses/input_tokens)"}}`)),
	}}
	repo := &countTokensRuntimeStateRepo{}
	svc := &OpenAIGatewayService{
		cfg:              &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream:     upstream,
		rateLimitService: &RateLimitService{accountRepo: repo, cfg: &config.Config{}},
	}
	account := &Account{
		ID: 171, Name: "official-openai", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "official-key", "base_url": "https://api.openai.com/v1"},
	}

	err := svc.ForwardResponsesInputTokens(t.Context(), c, account, body)

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "response.input_tokens", gjson.Get(recorder.Body.String(), "object").String())
	require.Positive(t, gjson.Get(recorder.Body.String(), "input_tokens").Int())
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "https://api.openai.com/v1/responses/input_tokens", upstream.lastReq.URL.String())
	require.Zero(t, repo.tempUnschedCalls)
	require.Zero(t, repo.setErrorCalls)
}

func TestForwardResponsesInputTokensTransportErrorUsesExistingFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"gpt-5.4","input":"hello world"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", bytes.NewReader(body))

	upstream := &httpUpstreamRecorder{err: errors.New("temporary TLS handshake failure")}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream: upstream,
	}
	account := &Account{
		ID: 172, Name: "official-openai", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "official-key", "base_url": "https://api.openai.com/v1"},
	}

	err := svc.ForwardResponsesInputTokens(t.Context(), c, account, body)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.Zero(t, recorder.Body.Len(), "切号前不得提前写出客户端错误响应")
}
