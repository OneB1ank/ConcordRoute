package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/model"
	"github.com/TokenFlux/TokenRouter/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type codexPATHTTPUpstreamRecorder struct {
	req                *http.Request
	proxyURL           string
	accountID          int64
	accountConcurrency int
	profile            *tlsfingerprint.Profile
	calledDo           bool
	calledDoWithTLS    bool
}

func (r *codexPATHTTPUpstreamRecorder) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	r.calledDo = true
	return r.record(req, proxyURL, accountID, accountConcurrency, nil)
}

func (r *codexPATHTTPUpstreamRecorder) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	r.calledDoWithTLS = true
	return r.record(req, proxyURL, accountID, accountConcurrency, profile)
}

func (r *codexPATHTTPUpstreamRecorder) record(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	r.req = req
	r.proxyURL = proxyURL
	r.accountID = accountID
	r.accountConcurrency = accountConcurrency
	r.profile = profile
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{
			"email":"user@example.com",
			"chatgpt_user_id":"user-123",
			"chatgpt_account_id":"acct-123",
			"chatgpt_plan_type":"plus",
			"chatgpt_account_is_fedramp":false
		}`)),
	}, nil
}

func TestOpenAIOAuthService_ValidateCodexPersonalAccessToken(t *testing.T) {
	const macUA = "codex-tui/0.200.1 (Mac OS X 15.6; arm64) Terminal.app (codex-tui; 0.200.1)"
	withCodexCanonicalUA(t, macUA)
	var gotAuthorization string
	var gotOriginator string
	var gotUserAgent string
	var gotVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("authorization")
		gotOriginator = r.Header.Get("originator")
		gotUserAgent = r.Header.Get("user-agent")
		gotVersion = r.Header.Get("version")
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"email":"user@example.com",
			"chatgpt_user_id":"user-123",
			"chatgpt_account_id":"acct-123",
			"chatgpt_plan_type":"plus",
			"chatgpt_account_is_fedramp":true
		}`))
	}))
	defer server.Close()

	originalURL := openAICodexPATWhoamiURL
	openAICodexPATWhoamiURL = server.URL
	defer func() { openAICodexPATWhoamiURL = originalURL }()

	svc := NewOpenAIOAuthService(nil, nil)
	defer svc.Stop()

	info, err := svc.ValidateCodexPersonalAccessToken(context.Background(), " at-test-token ", "")
	require.NoError(t, err)
	require.Equal(t, "Bearer at-test-token", gotAuthorization)
	require.Equal(t, "codex-tui", gotOriginator)
	require.Equal(t, macUA, gotUserAgent)
	require.Empty(t, gotVersion)
	require.Equal(t, OpenAIAuthModePersonalAccessToken, info.AuthMode)
	require.Equal(t, "user@example.com", info.Email)
	require.Equal(t, "user-123", info.ChatGPTUserID)
	require.Equal(t, "acct-123", info.ChatGPTAccountID)
	require.Equal(t, "plus", info.PlanType)
	require.True(t, info.ChatGPTAccountFedRAMP)
	require.Zero(t, info.ExpiresAt)
	require.Empty(t, info.RefreshToken)
}

func TestOpenAIOAuthService_ValidateCodexPersonalAccessTokenRequiresATPrefix(t *testing.T) {
	svc := NewOpenAIOAuthService(nil, nil)
	defer svc.Stop()

	_, err := svc.ValidateCodexPersonalAccessToken(context.Background(), "eyJ.jwt", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "at-")
}

func TestOpenAIOAuthService_ValidateCodexPersonalAccessTokenUsesTokenRouterIdentityTLSAndProxy(t *testing.T) {
	const macUA = "codex-tui/0.200.1 (Mac OS X 15.6; arm64) Terminal.app (codex-tui; 0.200.1)"
	withCodexCanonicalUA(t, macUA)
	profileID := int64(71)
	profile := &tlsfingerprint.Profile{Name: "macOS auth TLS", ALPNProtocols: []string{"h2", "http/1.1"}}
	upstream := &codexPATHTTPUpstreamRecorder{}
	svc := NewOpenAIOAuthService(nil, nil)
	svc.SetHTTPUpstream(upstream)
	svc.SetTokenTLSRouterDeps(nil, &openAIOAuthTLSRouterReaderStub{routers: map[int64]*model.TLSFingerprintRouter{
		9: {
			ID:                                       9,
			Enabled:                                  true,
			ChatGPTOAuthTokenUserAgent:               macUA,
			ChatGPTOAuthTokenTLSFingerprintProfileID: &profileID,
		},
	}}, &openAIOAuthTokenProfileResolverStub{profiles: map[int64]*tlsfingerprint.Profile{profileID: profile}})
	t.Cleanup(svc.Stop)
	account := &Account{
		ID:          88,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 4,
		Extra:       map[string]any{"tls_fingerprint_router_id": int64(9)},
	}

	_, err := svc.ValidateCodexPersonalAccessToken(t.Context(), "at-test-token", "http://127.0.0.1:18080", OpenAICodexPATValidationOptions{Account: account})
	require.NoError(t, err)
	require.Same(t, profile, upstream.profile)
	require.Equal(t, "http://127.0.0.1:18080", upstream.proxyURL)
	require.Equal(t, account.ID, upstream.accountID)
	require.Equal(t, account.Concurrency, upstream.accountConcurrency)
	require.False(t, upstream.calledDo)
	require.True(t, upstream.calledDoWithTLS)
	require.Equal(t, macUA, upstream.req.Header.Get("User-Agent"))
	require.Equal(t, "codex-tui", upstream.req.Header.Get("Originator"))
	require.Empty(t, upstream.req.Header.Get("Version"))
}

func TestOpenAIOAuthService_ValidateCodexPersonalAccessTokenWithoutExplicitAuthProfileUsesStandardTLS(t *testing.T) {
	const canonicalUA = "codex-tui/0.200.1 (Windows 10.0.26200; x86_64) dumb (codex-tui; 0.200.1)"
	withCodexCanonicalUA(t, canonicalUA)
	upstream := &codexPATHTTPUpstreamRecorder{}
	svc := NewOpenAIOAuthService(nil, nil)
	svc.SetHTTPUpstream(upstream)
	svc.SetTokenTLSRouterDeps(nil, &openAIOAuthTLSRouterReaderStub{routers: map[int64]*model.TLSFingerprintRouter{
		9: {ID: 9, Enabled: true},
	}}, &openAIOAuthTokenProfileResolverStub{})
	t.Cleanup(svc.Stop)
	account := &Account{
		ID:          89,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 2,
		Credentials: map[string]any{
			"user_agent": "codex_cli_rs/9.9.9 (Mac OS X 15.6; arm64)",
		},
		Extra: map[string]any{
			"enable_tls_fingerprint":     true,
			"tls_fingerprint_profile_id": int64(99),
			"tls_fingerprint_router_id":  int64(9),
		},
	}

	_, err := svc.ValidateCodexPersonalAccessToken(t.Context(), "at-test-token", "", OpenAICodexPATValidationOptions{Account: account})
	require.NoError(t, err)
	require.True(t, upstream.calledDo)
	require.False(t, upstream.calledDoWithTLS)
	require.Nil(t, upstream.profile)
	expectedUA, expectedOriginator := CodexAuthIdentityForUserAgent(canonicalUA)
	require.Equal(t, expectedUA, upstream.req.Header.Get("User-Agent"))
	require.Equal(t, expectedOriginator, upstream.req.Header.Get("Originator"))
}

func TestOpenAIOAuthService_BuildAccountCredentialsForPAT(t *testing.T) {
	svc := NewOpenAIOAuthService(nil, nil)
	defer svc.Stop()

	credentials := svc.BuildAccountCredentials(&OpenAITokenInfo{
		AccessToken:           "at-test-token",
		AuthMode:              OpenAIAuthModePersonalAccessToken,
		Email:                 "user@example.com",
		ChatGPTAccountID:      "acct-123",
		ChatGPTUserID:         "user-123",
		ChatGPTAccountFedRAMP: true,
		PlanType:              "plus",
	})

	require.Equal(t, "at-test-token", credentials["access_token"])
	require.Equal(t, OpenAIAuthModePersonalAccessToken, credentials["auth_mode"])
	require.Equal(t, "personal_access_token", credentials["openai_auth_mode"])
	require.Equal(t, "Bearer", credentials["token_type"])
	require.Equal(t, true, credentials["chatgpt_account_is_fedramp"])
	require.NotContains(t, credentials, "expires_at")
	require.NotContains(t, credentials, "refresh_token")
	require.NotContains(t, credentials, "id_token")
}

func TestNormalizeOpenAIPersonalAccessTokenCredentialsRemovesOAuthFields(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"auth_mode": "personal_access_token",
		},
	}
	credentials := map[string]any{
		"access_token":                "at-test-token",
		"refresh_token":               "stale-refresh-token",
		"id_token":                    "stale-id-token",
		"expires_at":                  "2026-01-01T00:00:00Z",
		"expires_in":                  3600,
		"client_id":                   "stale-client",
		"model_mapping":               map[string]any{"gpt-5": "gpt-5-codex"},
		"chatgpt_account_is_fedramp":  true,
		"subscription_expires_at":     "2026-12-31T00:00:00Z",
		"openai_usage_channel_fields": []any{"custom"},
	}

	got := NormalizeOpenAIPersonalAccessTokenCredentials(account, nil, credentials)

	require.Equal(t, "at-test-token", got["access_token"])
	require.Equal(t, OpenAIAuthModePersonalAccessToken, got["auth_mode"])
	require.Equal(t, "personal_access_token", got["openai_auth_mode"])
	require.Equal(t, "Bearer", got["token_type"])
	require.NotContains(t, got, "refresh_token")
	require.NotContains(t, got, "id_token")
	require.NotContains(t, got, "expires_at")
	require.NotContains(t, got, "expires_in")
	require.NotContains(t, got, "client_id")
	require.Equal(t, map[string]any{"gpt-5": "gpt-5-codex"}, got["model_mapping"])
	require.Equal(t, true, got["chatgpt_account_is_fedramp"])
	require.Equal(t, "2026-12-31T00:00:00Z", got["subscription_expires_at"])
	require.Equal(t, []any{"custom"}, got["openai_usage_channel_fields"])
}

func TestOpenAITokenProvider_PersonalAccessTokenDoesNotExpireLikeOAuth(t *testing.T) {
	expiresAt := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	account := &Account{
		ID:       4001,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "at-expired-metadata",
			"auth_mode":          OpenAIAuthModePersonalAccessToken,
			"openai_auth_mode":   "personal_access_token",
			"expires_at":         expiresAt,
			"chatgpt_account_id": "acct-123",
		},
	}

	provider := NewOpenAITokenProvider(nil, nil, nil)
	token, err := provider.GetAccessToken(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, "at-expired-metadata", token)
}

func TestSetOpenAIChatGPTAccountHeadersAddsFedRAMP(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id":         "acct-fed",
			"chatgpt_account_is_fedramp": true,
		},
	}
	headers := make(http.Header)

	setOpenAIChatGPTAccountHeaders(headers, account)

	require.Equal(t, "acct-fed", headers.Get("chatgpt-account-id"))
	require.Equal(t, "true", headers.Get("x-openai-fedramp"))
}
