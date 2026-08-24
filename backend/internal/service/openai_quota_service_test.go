package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/model"
	infraerrors "github.com/TokenFlux/TokenRouter/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestOpenAIQuotaServiceQueryUsageUsesCodexHeaders(t *testing.T) {
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
		codexInviteResetJSONResponse(`{"user_id":"user-1","rate_limit_reset_credits":{"available_count":2}}`),
		codexInviteResetJSONResponse(`{"credits":[{"expires_at":"2026-07-03T04:05:06Z"},{"expiresAt":"2026-07-04T04:05:06Z"}]}`),
	}}
	svc := NewOpenAIQuotaService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, nil, nil)

	usage, err := svc.QueryUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "user-1", usage.UserID)
	require.NotNil(t, usage.RateLimitResetCredits)
	require.Equal(t, 2, usage.RateLimitResetCredits.AvailableCount)
	require.Equal(t, []OpenAIRateLimitResetCreditDetail{
		{ExpiresAt: "2026-07-03T04:05:06Z"},
		{ExpiresAt: "2026-07-04T04:05:06Z"},
	}, usage.RateLimitResetCredits.Credits)
	require.Greater(t, usage.FetchedAt, int64(0))

	require.Len(t, upstream.requests, 2)
	require.Equal(t, "/backend-api/wham/usage", upstream.requests[0].URL.Path)
	require.Equal(t, "true", upstream.requests[0].URL.Query().Get("supports_rewardless_invites"))
	require.Equal(t, "/backend-api/wham/rate-limit-reset-credits", upstream.requests[1].URL.Path)
	for _, req := range upstream.requests {
		require.Equal(t, "Bearer oauth-token", req.Header.Get("Authorization"))
		require.Equal(t, openaiQuotaCodexBeta, req.Header.Get("OpenAI-Beta"))
		require.Equal(t, "Codex Desktop", req.Header.Get("originator"))
		require.Equal(t, codexInviteResetDefaultUserAgent, req.Header.Get("User-Agent"))
		require.Equal(t, "chatgpt-acc", req.Header.Get("chatgpt-account-id"))
		require.Equal(t, "1", req.Header.Get("X-OpenAI-Attach-Auth"))
		require.Equal(t, "1", req.Header.Get("X-OpenAI-Attach-Integrity-State"))
		require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(req.Context()))
	}
}

func TestOpenAIQuotaServiceApplyHeadersPairsConfiguredCodexIdentity(t *testing.T) {
	withCodexCanonicalUA(t, DefaultOpenAICodexUserAgent)

	tests := []struct {
		name       string
		userAgent  string
		originator string
	}{
		{
			name:       "exec",
			userAgent:  "codex_exec/0.145.0 (Windows 10.0.26200; x86_64) dumb (codex_exec; 0.145.0)",
			originator: "codex_exec",
		},
		{
			name:       "tui",
			userAgent:  "codex-tui/0.145.0 (Windows 10.0.26200; x86_64) dumb (codex-tui; 0.145.0)",
			originator: "codex-tui",
		},
		{
			name:       "cli rs",
			userAgent:  "codex_cli_rs/0.145.0 (Windows 10.0.26200; x86_64) dumb (codex_cli_rs; 0.145.0)",
			originator: "codex_cli_rs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
			svc := &OpenAIQuotaService{}
			accountCtx := &openAIQuotaAccountContext{
				account: &Account{
					ID:       42,
					Platform: PlatformOpenAI,
					Type:     AccountTypeOAuth,
					Credentials: map[string]any{
						"chatgpt_account_id": "chatgpt-acc",
					},
				},
				token:     "oauth-token",
				userAgent: tt.userAgent,
			}

			_, err := svc.applyHeaders(req, accountCtx)
			require.NoError(t, err)
			require.Equal(t, tt.userAgent, req.Header.Get("User-Agent"))
			require.Equal(t, tt.originator, req.Header.Get("originator"))
			require.Empty(t, req.Header.Get("version"))
		})
	}
}

func TestOpenAIQuotaServiceResetCreditUsesAutomaticSelectionAndRedeemRequestID(t *testing.T) {
	account := &Account{
		ID:       9,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONResponse(`{"code":"reset","windows_reset":1}`),
	}}
	svc := NewOpenAIQuotaService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, nil, nil)

	result, err := svc.ResetCredit(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "reset", result.Code)
	require.Equal(t, 1, result.WindowsReset)

	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/backend-api/wham/rate-limit-reset-credits/consume", upstream.requests[0].URL.Path)
	require.Equal(t, "application/json", upstream.requests[0].Header.Get("Content-Type"))
	var payload map[string]string
	require.NoError(t, json.Unmarshal([]byte(upstream.bodies[0]), &payload))
	require.NotContains(t, payload, "credit_id")
	require.NotEmpty(t, payload["redeem_request_id"])
	require.Contains(t, payload["redeem_request_id"], "-")
}

func TestOpenAIQuotaServiceResetCreditReturnsUpstreamNoCreditResult(t *testing.T) {
	account := &Account{
		ID:       10,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONResponse(`{"code":"no_credit","windows_reset":0}`),
	}}
	svc := NewOpenAIQuotaService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, nil, nil)

	result, err := svc.ResetCredit(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "no_credit", result.Code)
	require.Equal(t, 0, result.WindowsReset)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/backend-api/wham/rate-limit-reset-credits/consume", upstream.requests[0].URL.Path)
}

func TestOpenAIQuotaServiceCacheResetCreditsSnapshot(t *testing.T) {
	t.Run("保存带到期明细的快照", func(t *testing.T) {
		repo := &stubQuotaAccountRepo{}
		svc := &OpenAIQuotaService{accountRepo: repo}
		credits := &OpenAIRateLimitResetCredits{
			AvailableCount: 1,
			Credits: []OpenAIRateLimitResetCreditDetail{
				{ExpiresAt: "2099-07-03T04:05:06Z"},
			},
		}

		require.NoError(t, svc.CacheResetCreditsSnapshot(context.Background(), 42, credits))
		require.Equal(t, credits, repo.extraUpdates[42][openaiQuotaResetCreditsKey])
	})

	t.Run("正数次数缺少到期明细时保留旧缓存", func(t *testing.T) {
		repo := &stubQuotaAccountRepo{}
		svc := &OpenAIQuotaService{accountRepo: repo}

		err := svc.CacheResetCreditsSnapshot(context.Background(), 42, &OpenAIRateLimitResetCredits{AvailableCount: 1})

		require.Error(t, err)
		require.Empty(t, repo.extraUpdates)
	})

	t.Run("零次数允许空明细", func(t *testing.T) {
		repo := &stubQuotaAccountRepo{}
		svc := &OpenAIQuotaService{accountRepo: repo}
		credits := &OpenAIRateLimitResetCredits{AvailableCount: 0}

		require.NoError(t, svc.CacheResetCreditsSnapshot(context.Background(), 42, credits))
		require.Equal(t, credits, repo.extraUpdates[42][openaiQuotaResetCreditsKey])
	})

	t.Run("仓储错误向调用方返回", func(t *testing.T) {
		repo := &stubQuotaAccountRepo{extraUpdateErr: errors.New("database unavailable")}
		svc := &OpenAIQuotaService{accountRepo: repo}
		credits := &OpenAIRateLimitResetCredits{
			AvailableCount: 1,
			Credits:        []OpenAIRateLimitResetCreditDetail{{ExpiresAt: "2099-07-03T04:05:06Z"}},
		}

		err := svc.CacheResetCreditsSnapshot(context.Background(), 42, credits)

		require.ErrorContains(t, err, "database unavailable")
	})
}

func TestOpenAIQuotaServiceQueryAndResetUseSameAccountEgressAndTLSRouterSettings(t *testing.T) {
	quotaProfileID := int64(20)
	proxyID := int64(77)
	account := &Account{
		ID:          44,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		ProxyID:     &proxyID,
		Concurrency: 6,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
		Extra: map[string]any{
			"enable_tls_fingerprint":     true,
			"tls_fingerprint_profile_id": int64(10),
			"tls_fingerprint_router_id":  int64(9),
		},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONResponse(`{"rate_limit_reset_credits":{"available_count":0}}`),
		codexInviteResetJSONResponse(`{"available_count":0,"credits":[]}`),
		codexInviteResetJSONResponse(`{"code":"reset","windows_reset":1}`),
	}}
	proxy := &Proxy{ID: proxyID, Protocol: "http", Host: "proxy.example.test", Port: 3128}
	routerReader := &openAIOAuthTLSRouterReaderStub{routers: map[int64]*model.TLSFingerprintRouter{
		9: {
			ID:                                      9,
			Enabled:                                 true,
			CodexInviteResetUserAgent:               " codex_cli_rs/0.145.0 (Windows 10.0.26200; x86_64) dumb (codex_cli_rs; 0.145.0) ",
			CodexInviteResetTLSFingerprintProfileID: &quotaProfileID,
			Rules: []model.TLSFingerprintRouterRule{{
				Enabled:                 true,
				Pattern:                 "codex_cli_rs/",
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
	svc := NewOpenAIQuotaService(codexInviteResetAdminServiceStub{account: account, proxy: proxy}, upstream, nil, profileService, routerReader)

	_, err := svc.QueryUsage(context.Background(), account.ID)
	require.NoError(t, err)
	_, err = svc.ResetCredit(context.Background(), account.ID)
	require.NoError(t, err)
	require.Len(t, upstream.requests, 3)
	require.Len(t, upstream.profiles, 3)
	for i := range upstream.requests {
		require.Equal(t, "codex_cli_rs/0.145.0 (Windows 10.0.26200; x86_64) dumb (codex_cli_rs; 0.145.0)", upstream.requests[i].Header.Get("User-Agent"))
		require.Equal(t, "codex_cli_rs", upstream.requests[i].Header.Get("originator"))
		require.Equal(t, "http://proxy.example.test:3128", upstream.proxyURLs[i])
		require.Equal(t, account.ID, upstream.accountIDs[i])
		require.Equal(t, account.Concurrency, upstream.concurrencies[i])
		require.NotNil(t, upstream.profiles[i])
		require.Equal(t, "router-token", upstream.profiles[i].Name)
	}
	require.Equal(t, 0, upstream.standardCalls)
	require.Equal(t, 3, upstream.tlsCalls)
	require.Equal(t, "/backend-api/wham/rate-limit-reset-credits/consume", upstream.requests[2].URL.Path)
}

func TestOpenAIQuotaServiceFallsBackToAccountTLSWithoutExplicitQuotaProfile(t *testing.T) {
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
					"access_token":       "oauth-token",
					"chatgpt_account_id": "chatgpt-acc",
				},
				Extra: map[string]any{
					"enable_tls_fingerprint":     true,
					"tls_fingerprint_profile_id": int64(10),
					"tls_fingerprint_router_id":  int64(9),
				},
			}
			upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
				codexInviteResetJSONResponse(`{"rate_limit_reset_credits":{"available_count":0}}`),
				codexInviteResetJSONResponse(`{"available_count":0,"credits":[]}`),
			}}
			routerReader := &openAIOAuthTLSRouterReaderStub{routers: map[int64]*model.TLSFingerprintRouter{
				9: {
					ID:                                      9,
					Enabled:                                 true,
					CodexInviteResetTLSFingerprintProfileID: tt.profileID,
					Rules: []model.TLSFingerprintRouterRule{{
						Enabled:                 true,
						Pattern:                 "codex_cli_rs/",
						TLSFingerprintProfileID: 11,
					}},
				},
			}}
			profileService := &TLSFingerprintProfileService{localCache: map[int64]*model.TLSFingerprintProfile{
				10: {ID: 10, Name: "inference-only"},
				11: {ID: 11, Name: "router-inference-only"},
			}}
			svc := NewOpenAIQuotaService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, profileService, routerReader)

			_, err := svc.QueryUsage(context.Background(), account.ID)
			require.NoError(t, err)
			require.Equal(t, 0, upstream.standardCalls)
			require.Equal(t, 2, upstream.tlsCalls)
			require.Len(t, upstream.profiles, 2)
			for _, profile := range upstream.profiles {
				require.NotNil(t, profile)
				require.Equal(t, "inference-only", profile.Name)
			}
		})
	}
}

func TestOpenAIQuotaServiceUsesStandardTLSWhenAccountTLSIsDisabled(t *testing.T) {
	account := &Account{
		ID:       45,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
		Extra: map[string]any{
			"tls_fingerprint_profile_id": int64(10),
			"tls_fingerprint_router_id":  int64(9),
		},
	}
	upstream := &codexInviteResetHTTPUpstreamStub{responses: []*http.Response{
		codexInviteResetJSONResponse(`{"rate_limit_reset_credits":{"available_count":0}}`),
		codexInviteResetJSONResponse(`{"available_count":0,"credits":[]}`),
	}}
	routerReader := &openAIOAuthTLSRouterReaderStub{routers: map[int64]*model.TLSFingerprintRouter{
		9: {ID: 9, Enabled: true},
	}}
	profileService := &TLSFingerprintProfileService{localCache: map[int64]*model.TLSFingerprintProfile{
		10: {ID: 10, Name: "inference-only"},
	}}
	svc := NewOpenAIQuotaService(codexInviteResetAdminServiceStub{account: account}, upstream, nil, profileService, routerReader)

	_, err := svc.QueryUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, 2, upstream.standardCalls)
	require.Equal(t, 0, upstream.tlsCalls)
}

func TestOpenAIQuotaServiceRejectsUnsupportedAccount(t *testing.T) {
	account := &Account{
		ID:       7,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "sk-test",
		},
	}
	svc := NewOpenAIQuotaService(codexInviteResetAdminServiceStub{account: account}, &codexInviteResetHTTPUpstreamStub{}, nil, nil, nil)

	_, err := svc.QueryUsage(context.Background(), account.ID)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "OPENAI_QUOTA_UNSUPPORTED_ACCOUNT", infraerrors.Reason(err))
	require.False(t, strings.Contains(err.Error(), "sk-test"))
}
