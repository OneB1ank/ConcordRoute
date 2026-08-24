package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/model"
	infraerrors "github.com/TokenFlux/TokenRouter/internal/pkg/errors"
	"github.com/TokenFlux/TokenRouter/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type codexInviteResetAdminServiceStub struct {
	AdminService
	account *Account
	proxy   *Proxy
}

func (s codexInviteResetAdminServiceStub) GetAccount(ctx context.Context, id int64) (*Account, error) {
	return s.account, nil
}

func (s codexInviteResetAdminServiceStub) GetProxy(ctx context.Context, id int64) (*Proxy, error) {
	return s.proxy, nil
}

type codexInviteResetHTTPUpstreamStub struct {
	responses     []*http.Response
	requests      []*http.Request
	bodies        []string
	proxyURLs     []string
	accountIDs    []int64
	concurrencies []int
	profiles      []*tlsfingerprint.Profile
	standardCalls int
	tlsCalls      int
}

func (s *codexInviteResetHTTPUpstreamStub) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	s.standardCalls++
	return s.record(req, proxyURL, accountID, accountConcurrency, nil)
}

func (s *codexInviteResetHTTPUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	s.tlsCalls++
	return s.record(req, proxyURL, accountID, accountConcurrency, profile)
}

func (s *codexInviteResetHTTPUpstreamStub) record(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		payload, _ := io.ReadAll(req.Body)
		body = string(payload)
		req.Body = io.NopCloser(strings.NewReader(body))
	}
	s.requests = append(s.requests, req)
	s.bodies = append(s.bodies, body)
	s.proxyURLs = append(s.proxyURLs, proxyURL)
	s.accountIDs = append(s.accountIDs, accountID)
	s.concurrencies = append(s.concurrencies, accountConcurrency)
	s.profiles = append(s.profiles, profile)
	if len(s.responses) == 0 {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}
	resp := s.responses[0]
	s.responses = s.responses[1:]
	return resp, nil
}

func TestCodexInviteResetServiceGetStatusAggregatesDesktopEndpoints(t *testing.T) {
	account := &Account{
		ID:          42,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 3,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONResponse(`{"requires_explicit_confirmation":true,"should_show":false,"has_rewards":true,"grant_action":"rate_limit_reset_credit","grant_amount":3}`),
		codexInviteResetJSONResponse(`{"rules":[{"text":"friend must send first Codex message"}]}`),
		codexInviteResetJSONResponse(`{"rate_limit_reset_credits":{"available_count":2}}`),
		codexInviteResetJSONResponse(`{"available_count":2,"credits":[{"id":"credit-1","status":"available","title":"Reset","reset_type":"primary","granted_at":"2026-07-01T04:05:06Z"},{"id":"credit-2","status":"available"}]}`),
	}}
	svc := NewCodexInviteResetService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, nil, nil)

	status, err := svc.GetStatus(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, codexInviteResetReferralKey, status.ReferralKey)
	require.Equal(t, 2, status.AvailableCount)
	require.True(t, status.RequiresConsent)
	require.NotNil(t, status.ShouldShow)
	require.False(t, *status.ShouldShow)
	require.Equal(t, "rate_limit_reset_credit", status.GrantAction)
	require.NotNil(t, status.GrantAmount)
	require.Equal(t, 3, *status.GrantAmount)
	require.NotNil(t, status.HasRewards)
	require.True(t, *status.HasRewards)
	require.Equal(t, "rate_limit_reset", status.GrantType)
	require.Len(t, status.Credits, 2)
	require.Equal(t, "primary", status.Credits[0].ResetType)
	require.Equal(t, "2026-07-01T04:05:06Z", status.Credits[0].GrantedAt)
	require.Equal(t, "friend must send first Codex message", status.EligibilityRules[0])

	require.Len(t, upstream.requests, 4)
	require.Equal(t, "/backend-api/referrals/invite/eligibility", upstream.requests[0].URL.Path)
	require.Equal(t, codexInviteResetReferralKey, upstream.requests[0].URL.Query().Get("referral_key"))
	require.Equal(t, "true", upstream.requests[0].URL.Query().Get("supports_rewardless_invites"))
	require.Equal(t, "/backend-api/wham/referrals/eligibility_rules", upstream.requests[1].URL.Path)
	require.Equal(t, "/backend-api/wham/usage", upstream.requests[2].URL.Path)
	require.Equal(t, "true", upstream.requests[2].URL.Query().Get("supports_rewardless_invites"))
	require.Equal(t, "/backend-api/wham/rate-limit-reset-credits", upstream.requests[3].URL.Path)
	require.Equal(t, "Bearer oauth-token", upstream.requests[0].Header.Get("Authorization"))
	require.Equal(t, "Codex Desktop", upstream.requests[0].Header.Get("originator"))
	require.Equal(t, codexInviteResetDefaultUserAgent, upstream.requests[0].Header.Get("User-Agent"))
	require.Equal(t, "1", upstream.requests[0].Header.Get("X-OpenAI-Attach-Auth"))
	require.Equal(t, "1", upstream.requests[0].Header.Get("X-OpenAI-Attach-Integrity-State"))
	require.Equal(t, "none", upstream.requests[0].Header.Get("sec-fetch-site"))
	require.Equal(t, "no-cors", upstream.requests[0].Header.Get("sec-fetch-mode"))
	require.Equal(t, "empty", upstream.requests[0].Header.Get("sec-fetch-dest"))
	require.Equal(t, "u=4, i", upstream.requests[0].Header.Get("priority"))
	require.Equal(t, "chatgpt-acc", upstream.requests[0].Header.Get("chatgpt-account-id"))
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(upstream.requests[0].Context()))
}

func TestCodexInviteResetServiceGetStatusKeepsUsageCreditsWhenEligibilityReturns422AndDetailsFail(t *testing.T) {
	account := &Account{
		ID:       50,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "oauth-token",
		},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONStatusResponse(http.StatusUnprocessableEntity, `{"detail":[{"type":"missing","loc":["query","legacy_field"],"msg":"Field required"}]}`),
		codexInviteResetJSONResponse(`{"rules":[]}`),
		codexInviteResetJSONResponse(`{"rate_limit_reset_credits":{"available_count":1}}`),
		codexInviteResetJSONStatusResponse(http.StatusUnauthorized, `{"detail":"expired"}`),
	}}
	svc := NewCodexInviteResetService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, nil, nil)

	status, err := svc.GetStatus(context.Background(), account.ID)
	require.NoError(t, err)
	require.False(t, status.InviteAvailable)
	require.Equal(t, codexInviteResetUnavailable, status.InviteUnavailableReason)
	require.Equal(t, codexInviteResetUnavailableMessage, status.InviteUnavailableMessage)
	require.Equal(t, 1, status.AvailableCount)
	require.Empty(t, status.Credits)
	require.NotContains(t, status.InviteUnavailableMessage, "Field required")
	require.Len(t, upstream.requests, 4)
	require.Equal(t, "/backend-api/wham/usage", upstream.requests[2].URL.Path)
	require.Equal(t, "/backend-api/wham/rate-limit-reset-credits", upstream.requests[3].URL.Path)
}

func TestNormalizeCodexInviteResetGrantType(t *testing.T) {
	hasRewards := true
	hasNoRewards := false
	require.Equal(t, "none", normalizeCodexInviteResetGrantType(&hasNoRewards, "workspace_credits"))
	require.Equal(t, "rate_limit_reset", normalizeCodexInviteResetGrantType(&hasRewards, "rate_limit_reset_credit"))
	require.Equal(t, "workspace_credits", normalizeCodexInviteResetGrantType(&hasRewards, "workspace_credits"))
	require.Equal(t, "unknown", normalizeCodexInviteResetGrantType(&hasRewards, "future_reward"))
	require.Equal(t, "unknown", normalizeCodexInviteResetGrantType(nil, ""))
}

func TestCodexInviteResetServiceSendInviteNormalizesEmails(t *testing.T) {
	account := &Account{
		ID:          7,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONResponse(`{"invites":[{"email":"a@example.com"}],"message":"ok"}`),
	}}
	svc := NewCodexInviteResetService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, nil, nil)

	result, err := svc.SendInvite(context.Background(), account.ID, []string{"a@example.com, b@example.com", "A@example.com"})
	require.NoError(t, err)
	require.Equal(t, "ok", result.Message)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/backend-api/wham/referrals/invite", upstream.requests[0].URL.Path)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(upstream.bodies[0]), &payload))
	require.Equal(t, codexInviteResetReferralKey, payload["referral_key"])
	require.Equal(t, []any{"a@example.com", "b@example.com"}, payload["emails"])
}

func TestCodexInviteResetServiceSendInviteMapsUnavailableInvite(t *testing.T) {
	account := &Account{
		ID:          8,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONStatusResponse(http.StatusForbidden, `{"detail":"该推荐码对应的推荐邀请不可用"}`),
	}}
	svc := NewCodexInviteResetService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, nil, nil)

	result, err := svc.SendInvite(context.Background(), account.ID, []string{"a@example.com"})
	require.Nil(t, result)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, infraerrors.Code(err))
	require.Equal(t, codexInviteResetUnavailable, infraerrors.Reason(err))
	require.Equal(t, codexInviteResetUnavailableMessage, infraerrors.Message(err))
	require.Equal(t, "该推荐码对应的推荐邀请不可用", infraerrors.FromError(err).Metadata["upstream_detail"])
}

func TestCodexInviteResetServiceConsumeSendsRedeemRequestID(t *testing.T) {
	account := &Account{
		ID:          9,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONResponse(`{"code":"reset","available_count":0,"windows_reset":2}`),
	}}
	svc := NewCodexInviteResetService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, nil, nil)

	result, err := svc.Consume(context.Background(), account.ID, "credit-1")
	require.NoError(t, err)
	require.Equal(t, "reset", result.Code)
	require.Equal(t, "credit-1", result.CreditID)
	require.NotEmpty(t, result.RedeemRequestID)
	require.Equal(t, 2, result.WindowsReset)
	require.NotNil(t, result.AvailableCount)
	require.Equal(t, 0, *result.AvailableCount)

	var payload map[string]string
	require.NoError(t, json.Unmarshal([]byte(upstream.bodies[0]), &payload))
	require.Equal(t, "credit-1", payload["credit_id"])
	require.Equal(t, result.RedeemRequestID, payload["redeem_request_id"])
}

func TestCodexInviteResetServiceConsumeAllowsAutomaticCreditSelection(t *testing.T) {
	account := &Account{
		ID:          10,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONResponse(`{"code":"reset","windows_reset":1}`),
	}}
	svc := NewCodexInviteResetService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, nil, nil)

	result, err := svc.Consume(context.Background(), account.ID, "")
	require.NoError(t, err)
	require.Empty(t, result.CreditID)
	require.Equal(t, 1, result.WindowsReset)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(upstream.bodies[0]), &payload))
	require.NotEmpty(t, payload["redeem_request_id"])
	require.NotContains(t, payload, "credit_id")
}

func TestCodexInviteResetServiceUsesTLSRouterInviteResetUserAgent(t *testing.T) {
	account := &Account{
		ID:          43,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra: map[string]any{
			"tls_fingerprint_router_id": int64(9),
		},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONResponse(`{"requires_explicit_confirmation":true}`),
		codexInviteResetJSONResponse(`{"rules":[]}`),
		codexInviteResetJSONResponse(`{"available_count":0,"credits":[]}`),
	}}
	routerReader := &openAIOAuthTLSRouterReaderStub{routers: map[int64]*model.TLSFingerprintRouter{
		9: {
			ID:                        9,
			Enabled:                   true,
			CodexInviteResetUserAgent: " codex-tui/0.150.0 (Windows 10.0.26200; x86_64) dumb (codex-tui; 0.150.0) ",
		},
	}}
	svc := NewCodexInviteResetService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, nil, routerReader)

	_, err := svc.GetStatus(context.Background(), account.ID)
	require.NoError(t, err)
	require.Len(t, upstream.requests, 3)
	require.Equal(t, "codex-tui/0.150.0 (Windows 10.0.26200; x86_64) dumb (codex-tui; 0.150.0)", upstream.requests[0].Header.Get("User-Agent"))
	require.Equal(t, "codex-tui", upstream.requests[0].Header.Get("originator"))
}

func TestCodexInviteResetServiceDoesNotReuseTokenUserAgent(t *testing.T) {
	account := &Account{
		ID:          45,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra: map[string]any{
			"tls_fingerprint_router_id": int64(9),
		},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONResponse(`{"requires_explicit_confirmation":true}`),
		codexInviteResetJSONResponse(`{"rules":[]}`),
		codexInviteResetJSONResponse(`{"available_count":0,"credits":[]}`),
	}}
	routerReader := &openAIOAuthTLSRouterReaderStub{routers: map[int64]*model.TLSFingerprintRouter{
		9: {
			ID:                         9,
			Enabled:                    true,
			ChatGPTOAuthTokenUserAgent: "codex-tui/0.135.0 (Windows 10.0.26200; x86_64)",
		},
	}}
	svc := NewCodexInviteResetService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, nil, routerReader)

	_, err := svc.GetStatus(context.Background(), account.ID)
	require.NoError(t, err)
	require.Len(t, upstream.requests, 3)
	require.Equal(t, codexInviteResetDefaultUserAgent, upstream.requests[0].Header.Get("User-Agent"))
}

func TestCodexInviteResetServiceUsesTLSRouterInviteResetTLSProfile(t *testing.T) {
	inviteResetProfileID := int64(20)
	account := &Account{
		ID:          44,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra: map[string]any{
			"enable_tls_fingerprint":     true,
			"tls_fingerprint_profile_id": int64(10),
			"tls_fingerprint_router_id":  int64(9),
		},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONResponse(`{"requires_explicit_confirmation":true}`),
		codexInviteResetJSONResponse(`{"rules":[]}`),
		codexInviteResetJSONResponse(`{"available_count":0,"credits":[]}`),
	}}
	routerReader := &openAIOAuthTLSRouterReaderStub{routers: map[int64]*model.TLSFingerprintRouter{
		9: {
			ID:                                      9,
			Enabled:                                 true,
			CodexInviteResetTLSFingerprintProfileID: &inviteResetProfileID,
			Rules: []model.TLSFingerprintRouterRule{{
				Enabled:                 true,
				Pattern:                 "codex-cli/",
				TLSFingerprintProfileID: 21,
			}},
		},
	}}
	profileService := &TLSFingerprintProfileService{
		localCache: map[int64]*model.TLSFingerprintProfile{
			10: {ID: 10, Name: "account-fixed"},
			20: {ID: 20, Name: "router-token"},
			21: {ID: 21, Name: "router-rule"},
		},
	}
	svc := NewCodexInviteResetService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, profileService, routerReader)

	_, err := svc.GetStatus(context.Background(), account.ID)
	require.NoError(t, err)
	require.Len(t, upstream.profiles, 3)
	require.NotNil(t, upstream.profiles[0])
	require.Equal(t, "router-token", upstream.profiles[0].Name)
	require.Equal(t, 0, upstream.standardCalls)
	require.Equal(t, 3, upstream.tlsCalls)
}

func TestCodexInviteResetServiceFallsBackToAccountTLSWithoutExplicitInviteResetProfile(t *testing.T) {
	missingProfileID := int64(99)
	tests := []struct {
		name      string
		profileID *int64
	}{
		{name: "not configured"},
		{name: "configured profile missing", profileID: &missingProfileID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				ID:       44,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token": "oauth-token",
				},
				Extra: map[string]any{
					"enable_tls_fingerprint":     true,
					"tls_fingerprint_profile_id": int64(10),
					"tls_fingerprint_router_id":  int64(9),
				},
			}
			upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
				codexInviteResetJSONResponse(`{"requires_explicit_confirmation":true}`),
				codexInviteResetJSONResponse(`{"rules":[]}`),
				codexInviteResetJSONResponse(`{"available_count":0,"credits":[]}`),
			}}
			routerReader := &openAIOAuthTLSRouterReaderStub{routers: map[int64]*model.TLSFingerprintRouter{
				9: {
					ID:                                      9,
					Enabled:                                 true,
					CodexInviteResetTLSFingerprintProfileID: tt.profileID,
				},
			}}
			profileService := &TLSFingerprintProfileService{localCache: map[int64]*model.TLSFingerprintProfile{
				10: {ID: 10, Name: "inference-only"},
			}}
			svc := NewCodexInviteResetService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, profileService, routerReader)

			_, err := svc.GetStatus(context.Background(), account.ID)
			require.NoError(t, err)
			require.Equal(t, 0, upstream.standardCalls)
			require.Equal(t, 3, upstream.tlsCalls)
			require.Len(t, upstream.profiles, 3)
			for _, profile := range upstream.profiles {
				require.NotNil(t, profile)
				require.Equal(t, "inference-only", profile.Name)
			}
		})
	}
}

func TestCodexInviteResetServiceIgnoresInferenceRuleAndUsesAccountTLS(t *testing.T) {
	account := &Account{
		ID:          46,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token", "user_agent": "codex_cli_rs/0.145.0 (Windows 10.0.26200; x86_64) dumb (codex_cli_rs; 0.145.0)"},
		Extra: map[string]any{
			"enable_tls_fingerprint":     true,
			"tls_fingerprint_profile_id": int64(10),
			"tls_fingerprint_router_id":  int64(9),
		},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONResponse(`{"requires_explicit_confirmation":true}`),
		codexInviteResetJSONResponse(`{"rules":[]}`),
		codexInviteResetJSONResponse(`{"available_count":0,"credits":[]}`),
	}}
	routerReader := &openAIOAuthTLSRouterReaderStub{routers: map[int64]*model.TLSFingerprintRouter{
		9: {
			ID:      9,
			Enabled: true,
			Rules: []model.TLSFingerprintRouterRule{{
				Enabled:                 true,
				Pattern:                 "codex_cli_rs/",
				TLSFingerprintProfileID: 21,
			}},
		},
	}}
	profileService := &TLSFingerprintProfileService{localCache: map[int64]*model.TLSFingerprintProfile{
		10: {ID: 10, Name: "account-fixed"},
		21: {ID: 21, Name: "router-rule"},
	}}
	svc := NewCodexInviteResetService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, profileService, routerReader)

	_, err := svc.GetStatus(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, 3, upstream.tlsCalls)
	require.Equal(t, 0, upstream.standardCalls)
	for _, profile := range upstream.profiles {
		require.NotNil(t, profile)
		require.Equal(t, "account-fixed", profile.Name)
	}
}

func TestCodexInviteResetServiceUsesStandardTLSWhenAccountTLSIsDisabled(t *testing.T) {
	account := &Account{
		ID:          47,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra: map[string]any{
			"tls_fingerprint_profile_id": int64(10),
			"tls_fingerprint_router_id":  int64(9),
		},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONResponse(`{"requires_explicit_confirmation":true}`),
		codexInviteResetJSONResponse(`{"rules":[]}`),
		codexInviteResetJSONResponse(`{"available_count":0,"credits":[]}`),
	}}
	routerReader := &openAIOAuthTLSRouterReaderStub{routers: map[int64]*model.TLSFingerprintRouter{
		9: {ID: 9, Enabled: true},
	}}
	profileService := &TLSFingerprintProfileService{localCache: map[int64]*model.TLSFingerprintProfile{
		10: {ID: 10, Name: "account-fixed"},
	}}
	svc := NewCodexInviteResetService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, profileService, routerReader)

	_, err := svc.GetStatus(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, 3, upstream.standardCalls)
	require.Equal(t, 0, upstream.tlsCalls)
}

func TestNormalizeCodexInviteEmailsRejectsInvalidAndTooMany(t *testing.T) {
	_, err := normalizeCodexInviteEmails([]string{"bad-email"})
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))

	_, err = normalizeCodexInviteEmails([]string{"a@x.com,b@x.com,c@x.com,d@x.com,e@x.com,f@x.com"})
	require.Error(t, err)
	require.Equal(t, "CODEX_INVITE_RESET_EMAIL_LIMIT", infraerrors.Reason(err))
}

func codexInviteResetJSONResponse(body string) *http.Response {
	return codexInviteResetJSONStatusResponse(http.StatusOK, body)
}

func codexInviteResetJSONStatusResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
