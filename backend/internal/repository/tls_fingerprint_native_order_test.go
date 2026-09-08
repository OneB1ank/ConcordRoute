package repository

import (
	"github.com/TokenFlux/TokenRouter/internal/pkg/tlsfingerprint"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/stretchr/testify/require"
)

// 验证真实 Transport 缓存，而不只比较独立 hash 函数的返回值。
func (s *HTTPUpstreamSuite) TestRustlsNativeOrderSplitsClientCache() {
	svc := s.newService()
	fixed := &tlsfingerprint.Profile{Name: "same-profile"}
	native := *fixed
	native.RustlsNativeOrder = true
	a, err := svc.getClientEntryWithTLS("", 1, 1, fixed, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(s.T(), err)
	b, err := svc.getClientEntryWithTLS("", 1, 1, &native, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(s.T(), err)
	bAgain, err := svc.getClientEntryWithTLS("", 1, 1, &native, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(s.T(), err)
	require.NotSame(s.T(), a, b)
	require.Same(s.T(), b, bAgain)
	disabled := native
	disabled.RustlsNativeOrder = false
	back, err := svc.getClientEntryWithTLS("", 1, 1, &disabled, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(s.T(), err)
	require.Same(s.T(), a, back, "关闭后回到旧模板池键，而不是永久改变固定排序客户端")
}
