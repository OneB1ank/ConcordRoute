package service

import (
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/model"
	"github.com/stretchr/testify/require"
)

func TestTLSFingerprintProfileService_ResolveTLSProfileOpenAI(t *testing.T) {
	svc := &TLSFingerprintProfileService{}

	openAIOAuth := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"enable_tls_fingerprint": true},
	}
	require.NotNil(t, svc.ResolveTLSProfile(openAIOAuth), "OpenAI OAuth 开启后应返回内置默认 profile")

	openAIAPIKey := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"enable_tls_fingerprint": true},
	}
	require.Nil(t, svc.ResolveTLSProfile(openAIAPIKey), "OpenAI API Key 不应启用 TLS 指纹伪装")
}

func TestChangedTLSFingerprintProfileIDsOnlyReportsHandshakeChanges(t *testing.T) {
	base := &model.TLSFingerprintProfile{
		ID:                3,
		Name:              "macos",
		EnableGREASE:      true,
		CipherSuites:      []uint16{4865, 4866},
		SupportedVersions: []uint16{0x0304, 0x0303},
	}
	updatedTimestampOnly := *base
	updatedTimestampOnly.UpdatedAt = updatedTimestampOnly.UpdatedAt.Add(time.Hour)
	require.Empty(t, changedTLSFingerprintProfileIDs(
		map[int64]*model.TLSFingerprintProfile{3: base},
		map[int64]*model.TLSFingerprintProfile{3: &updatedTimestampOnly},
	))

	changedProfile := updatedTimestampOnly
	changedProfile.CipherSuites = []uint16{4865, 4867}
	changed := changedTLSFingerprintProfileIDs(
		map[int64]*model.TLSFingerprintProfile{3: base},
		map[int64]*model.TLSFingerprintProfile{3: &changedProfile},
	)
	_, ok := changed[3]
	require.True(t, ok)

	removed := changedTLSFingerprintProfileIDs(
		map[int64]*model.TLSFingerprintProfile{3: base},
		map[int64]*model.TLSFingerprintProfile{},
	)
	_, ok = removed[3]
	require.True(t, ok)
}

func TestTLSFingerprintProfileService_ResolveTLSProfileQoderCosy(t *testing.T) {
	svc := &TLSFingerprintProfileService{}

	qoderCosy := &Account{
		Platform: PlatformQoder,
		Type:     AccountTypeCosy,
		Extra:    map[string]any{"enable_tls_fingerprint": true},
	}
	require.NotNil(t, svc.ResolveTLSProfile(qoderCosy), "Qoder COSY 开启后应返回内置默认 profile")

	qoderOtherType := &Account{
		Platform: PlatformQoder,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"enable_tls_fingerprint": true},
	}
	require.Nil(t, svc.ResolveTLSProfile(qoderOtherType), "非 COSY Qoder 账号不应启用 TLS 指纹伪装")
}

func TestOpenAIGatewayService_ResolveTLSProfileRouterFallback(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"enable_tls_fingerprint":     true,
			"tls_fingerprint_profile_id": int64(10),
		},
	}
	profileSvc := &TLSFingerprintProfileService{
		localCache: map[int64]*model.TLSFingerprintProfile{
			10: {ID: 10, Name: "fixed"},
			20: {ID: 20, Name: "router"},
		},
	}
	svc := &OpenAIGatewayService{tlsFPProfileService: profileSvc}

	// 路由器命中优先使用规则目标模板。
	routerProfile := svc.resolveOpenAITLSProfile(account, TLSFingerprintRouterMatchResult{
		Matched:                 true,
		TLSFingerprintProfileID: 20,
	})
	require.NotNil(t, routerProfile)
	require.Equal(t, "router", routerProfile.Name)

	// 规则目标模板不可用时安全回退账号固定模板。
	fallbackProfile := svc.resolveOpenAITLSProfile(account, TLSFingerprintRouterMatchResult{
		Matched:                 true,
		TLSFingerprintProfileID: 404,
	})
	require.NotNil(t, fallbackProfile)
	require.Equal(t, "fixed", fallbackProfile.Name)
}
