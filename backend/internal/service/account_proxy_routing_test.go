package service

import (
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/pkg/proxyurl"
	"github.com/stretchr/testify/require"
)

func TestResolveAccountProxyURLRoutingContract(t *testing.T) {
	proxyID := int64(7)
	proxy := &Proxy{Protocol: "socks5h", Host: "127.0.0.1", Port: 17000}

	tests := []struct {
		name            string
		account         *Account
		wantURL         string
		wantConfigured  bool
		wantParseFailed bool
	}{
		{name: "nil account is direct", account: nil},
		{name: "unbound account is direct", account: &Account{}},
		{name: "loaded configured proxy is used", account: &Account{ProxyID: &proxyID, Proxy: proxy}, wantURL: proxy.URL(), wantConfigured: true},
		{name: "missing configured proxy fails closed", account: &Account{ProxyID: &proxyID}, wantURL: unavailableAccountProxyURL, wantConfigured: true, wantParseFailed: true},
		{name: "legacy loaded proxy remains proxied", account: &Account{Proxy: proxy}, wantURL: proxy.URL(), wantConfigured: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveAccountProxyURL(tt.account)
			require.Equal(t, tt.wantURL, got)
			require.Equal(t, tt.wantConfigured, accountHasConfiguredProxy(tt.account))
			_, _, err := proxyurl.Parse(got)
			if tt.wantParseFailed {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
