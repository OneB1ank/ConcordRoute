//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/model"
	"github.com/stretchr/testify/require"
)

func TestOpenAIBackgroundIdentityReusesLatestNormalRequestSnapshot(t *testing.T) {
	account := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"tls_fingerprint_router_id": int64(7),
		},
	}
	match := TLSFingerprintRouterMatchResult{
		Matched:                 true,
		RouterID:                7,
		RuleName:                "macos-codex",
		TLSFingerprintProfileID: 99,
		UpstreamUserAgent:       "codex-cli/1.2.3 (Mac OS 15.6; arm64) Apple_Terminal",
	}
	gateway := &OpenAIGatewayService{}
	gateway.rememberOpenAIOutboundIdentity(account, match.UpstreamUserAgent, match)

	userAgent, resolved := gateway.resolveOpenAIBackgroundIdentity(account)

	require.Equal(t, match.UpstreamUserAgent, userAgent)
	require.NotNil(t, resolved.identityAccount)
	require.Equal(t, account.ID, resolved.identityAccount.ID)
	resolved.identityAccount = nil
	require.Equal(t, match, resolved)
}

func TestOpenAIBackgroundIdentityRejectsStaleOrDifferentRouterSnapshot(t *testing.T) {
	account := &Account{
		ID:       43,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"user_agent": "codex-tui/2.0.0 (Mac OS 15.6; arm64) Apple_Terminal",
		},
		Extra: map[string]any{"tls_fingerprint_router_id": int64(8)},
	}
	gateway := &OpenAIGatewayService{}
	gateway.openaiOutboundIdentities.Store(account.ID, openAIOutboundIdentitySnapshot{
		RouterID:            7,
		ConfiguredUserAgent: openAIBackgroundIdentityBaseUA(account),
		UserAgent:           "codex-cli/old (Linux; x86_64)",
		UpdatedAt:           time.Now().UTC(),
	})

	userAgent, match := gateway.resolveOpenAIBackgroundIdentity(account)

	require.Equal(t, resolveCodexOutboundIdentity(account.GetOpenAIUserAgent()).userAgent, userAgent)
	require.False(t, match.Matched)
	_, exists := gateway.openaiOutboundIdentities.Load(account.ID)
	require.False(t, exists)
}

func TestOpenAIBackgroundIdentityRejectsSnapshotAfterConfiguredUAChanges(t *testing.T) {
	account := &Account{
		ID:       44,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"user_agent": "codex-tui/2.0.0 (Mac OS 15.6; arm64) Apple_Terminal",
		},
	}
	gateway := &OpenAIGatewayService{}
	gateway.rememberOpenAIOutboundIdentity(account, resolveCodexOutboundIdentity(account.GetOpenAIUserAgent()).userAgent, TLSFingerprintRouterMatchResult{})
	account.Credentials["user_agent"] = "codex-tui/3.0.0 (Mac OS 15.7; arm64) Apple_Terminal"

	userAgent, _ := gateway.resolveOpenAIBackgroundIdentity(account)

	require.Equal(t, resolveCodexOutboundIdentity(account.GetOpenAIUserAgent()).userAgent, userAgent)
	_, exists := gateway.openaiOutboundIdentities.Load(account.ID)
	require.False(t, exists)
}

func TestOpenAIBackgroundIdentityDoesNotRematchUnknownForegroundUA(t *testing.T) {
	account := &Account{
		ID:       45,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"user_agent": "codex-tui/2.0.0 (Mac OS 15.6; arm64) Apple_Terminal",
		},
		Extra: map[string]any{"tls_fingerprint_router_id": int64(9)},
	}
	match := TLSFingerprintRouterMatchResult{
		Matched:                 true,
		RouterID:                9,
		RouterName:              "macos-router",
		RuleName:                "darwin-client",
		TLSFingerprintProfileID: 12,
		UpstreamUserAgent:       "codex-cli/1.2.3 (Mac OS 15.6; arm64) Apple_Terminal",
		UpstreamOriginator:      "codex_cli_rs",
	}
	router := &model.TLSFingerprintRouter{ID: 9, Name: "macos-router", Enabled: true}
	routerService := &TLSFingerprintRouterService{
		localCache: map[int64]*cachedTLSFingerprintRouter{
			9: {
				TLSFingerprintRouter: router,
				rules: []cachedTLSFingerprintRouterRule{{
					TLSFingerprintRouterRule: model.TLSFingerprintRouterRule{},
				}},
			},
		},
	}
	// Populate the embedded rule with the same outputs as the saved snapshot.
	routerService.localCache[9].rules[0].TLSFingerprintRouterRule = model.TLSFingerprintRouterRule{
		Name:                    "darwin-client",
		Enabled:                 true,
		TLSFingerprintProfileID: 12,
		UpstreamUserAgent:       match.UpstreamUserAgent,
		UpstreamOriginator:      match.UpstreamOriginator,
	}
	gateway := &OpenAIGatewayService{tlsFPRouterService: routerService}
	gateway.rememberOpenAIOutboundIdentity(account, match.UpstreamUserAgent, match)

	userAgent, resolved := gateway.resolveOpenAIBackgroundIdentity(account)
	require.Equal(t, match.UpstreamUserAgent, userAgent)
	resolved.identityAccount = nil
	require.Equal(t, match, resolved)
}
