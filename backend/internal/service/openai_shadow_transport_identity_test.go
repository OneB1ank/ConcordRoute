//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIShadowTransportIdentityUsesParentUAAndTLS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	parentID := int64(1901)
	parent := newTestOAuthAccount(parentID, map[string]any{
		"enable_tls_fingerprint":     true,
		"tls_fingerprint_profile_id": int64(10),
		"tls_fingerprint_router_id":  int64(9),
	})
	parent.Credentials = map[string]any{
		"access_token": "parent-token",
		"user_agent":   "codex-tui/0.144.1 (Mac OS X 15.6; arm64) Terminal.app",
	}
	shadow := newTestOAuthAccount(1902, map[string]any{
		CodexFingerprintSeedExtraKey: parent.GetExtraString(CodexFingerprintSeedExtraKey),
	})
	shadow.ParentAccountID = &parentID
	shadow.QuotaDimension = QuotaDimensionSpark
	shadow.Credentials = map[string]any{"model_mapping": map[string]any{}}

	router := &model.TLSFingerprintRouter{
		ID:      9,
		Name:    "parent-router",
		Enabled: true,
		Rules: []model.TLSFingerprintRouterRule{{
			Enabled:                 true,
			MatchType:               model.TLSRouterMatchExact,
			Pattern:                 "client-codex/1.0",
			TLSFingerprintProfileID: 20,
			UpstreamUserAgent:       "codex-tui/0.144.1 (Mac OS X 15.6; arm64) Terminal.app",
		}},
	}
	profileService := &TLSFingerprintProfileService{localCache: map[int64]*model.TLSFingerprintProfile{
		10: {ID: 10, Name: "parent-fixed"},
		20: {ID: 20, Name: "parent-routed"},
	}}
	gateway := &OpenAIGatewayService{
		accountRepo:         &stubOpenAIAccountRepo{accounts: []Account{*parent, *shadow}},
		tlsFPProfileService: profileService,
		tlsFPRouterService: &TLSFingerprintRouterService{localCache: map[int64]*cachedTLSFingerprintRouter{
			9: newCachedTLSFingerprintRouter(router),
		}},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "client-codex/1.0")

	match := gateway.matchTLSFingerprintRouter(c, shadow)
	require.True(t, match.Matched)
	require.NotNil(t, match.identityAccount)
	require.Equal(t, parent.ID, match.identityAccount.ID)

	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	req.Header.Set("User-Agent", "client-codex/1.0")
	req.Header.Set("Originator", "codex-tui")
	gateway.applyOpenAIUpstreamUserAgent(req.Context(), c, shadow, req, false, match)
	enforceCodexIdentityHeadersWithUA(req.Header, gateway.codexIdentityOverrideUA(shadow, match))
	require.Equal(t, "codex-tui/0.144.1 (Mac OS X 15.6; arm64) Terminal.app", req.Header.Get("User-Agent"))
	require.Equal(t, "codex-tui", req.Header.Get("Originator"))

	profile := gateway.resolveOpenAITLSProfile(shadow, match)
	require.NotNil(t, profile)
	require.Equal(t, "parent-routed", profile.Name)
}
