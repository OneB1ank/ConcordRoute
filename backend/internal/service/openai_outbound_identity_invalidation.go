package service

import "context"

// openAIOutboundAccountConfig 是账号出站身份相关配置的轻量快照。
// 凭据内容只保留规范化 UA，不保存 token 或代理 URL。
type openAIOutboundAccountConfig struct {
	Eligible     bool
	UserAgent    string
	RouterID     int64
	TLSProfileID int64
	TLSEnabled   bool
	ProxyID      int64
}

func openAIOutboundAccountConfigOf(account *Account) openAIOutboundAccountConfig {
	if account == nil || !account.IsOpenAIOAuth() || account.IsShadow() {
		return openAIOutboundAccountConfig{}
	}
	proxyID := int64(0)
	if account.ProxyID != nil {
		proxyID = *account.ProxyID
	}
	return openAIOutboundAccountConfig{
		Eligible:     true,
		UserAgent:    openAIBackgroundIdentityBaseUA(account),
		RouterID:     account.GetTLSFingerprintRouterID(),
		TLSProfileID: account.GetTLSFingerprintProfileID(),
		TLSEnabled:   account.IsTLSFingerprintEnabled(),
		ProxyID:      proxyID,
	}
}

func openAIOutboundIdentityExtraChanged(updates map[string]any) bool {
	for _, key := range []string{
		"enable_tls_fingerprint",
		"tls_fingerprint_profile_id",
		"tls_fingerprint_router_id",
	} {
		if _, ok := updates[key]; ok {
			return true
		}
	}
	return false
}

// invalidateOpenAIOutboundIdentityFamily evicts a logical account and any
// credential shadows that inherit its UA/TLS/proxy settings. Shadow requests
// use their own scheduling IDs but share the parent's transport identity.
func invalidateOpenAIOutboundIdentityFamily(ctx context.Context, repo AccountRepository, accountID int64) {
	invalidateOpenAIOutboundIdentity(accountID)
	if repo == nil || accountID <= 0 {
		return
	}
	shadows, err := repo.ListShadowsByParent(ctx, accountID)
	if err != nil {
		return
	}
	for _, shadow := range shadows {
		if shadow != nil {
			invalidateOpenAIOutboundIdentity(shadow.ID)
		}
	}
}
