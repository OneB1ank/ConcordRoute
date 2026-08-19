package service

import "strings"

// unavailableAccountProxyURL 是 fail-closed 哨兵。代理已被账号显式选择、但运行时
// 代理对象缺失或配置为空时，统一 HTTP/WS 客户端会在解析该 URL 时失败，
// 从而阻止请求静默回退到服务器直连出口。
const unavailableAccountProxyURL = "required-proxy://unavailable.invalid"

// resolveAccountProxyURL 返回账号唯一允许使用的出口。
// 未配置代理时返回空字符串；已配置但代理对象缺失时返回无效哨兵。
func resolveAccountProxyURL(account *Account) string {
	if account == nil {
		return ""
	}
	if account.Proxy != nil {
		if proxyURL := strings.TrimSpace(account.Proxy.URL()); proxyURL != "" {
			return proxyURL
		}
		return unavailableAccountProxyURL
	}
	if account.ProxyID != nil {
		return unavailableAccountProxyURL
	}
	return ""
}

func accountHasConfiguredProxy(account *Account) bool {
	return account != nil && (account.ProxyID != nil || account.Proxy != nil)
}
