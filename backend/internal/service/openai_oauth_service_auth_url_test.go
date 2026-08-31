package service

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/model"
	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
	"github.com/TokenFlux/TokenRouter/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type openaiOAuthClientAuthURLStub struct{}

func (s *openaiOAuthClientAuthURLStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string, options ...OpenAIOAuthTokenRequestOptions) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientAuthURLStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string, options ...OpenAIOAuthTokenRequestOptions) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientAuthURLStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string, options ...OpenAIOAuthTokenRequestOptions) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func TestOpenAIOAuthService_GenerateAuthURL_OpenAIKeepsCodexFlow(t *testing.T) {
	svc := NewOpenAIOAuthService(nil, &openaiOAuthClientAuthURLStub{})
	defer svc.Stop()

	result, err := svc.GenerateAuthURL(context.Background(), nil, "", PlatformOpenAI, nil)
	require.NoError(t, err)
	require.NotEmpty(t, result.AuthURL)
	require.NotEmpty(t, result.SessionID)

	parsed, err := url.Parse(result.AuthURL)
	require.NoError(t, err)
	q := parsed.Query()
	require.Equal(t, openai.ClientID, q.Get("client_id"))
	require.Equal(t, "true", q.Get("codex_cli_simplified_flow"))

	session, ok := svc.sessionStore.Get(result.SessionID)
	require.True(t, ok)
	require.Equal(t, openai.ClientID, session.ClientID)
	require.Zero(t, session.TLSFingerprintRouterID)
}

func TestOpenAIOAuthService_GenerateAuthURL_BindsValidatedTLSRouter(t *testing.T) {
	profileID := int64(31)
	routerID := int64(17)
	svc := NewOpenAIOAuthService(nil, &openaiOAuthClientAuthURLStub{})
	defer svc.Stop()
	svc.SetTokenTLSRouterDeps(nil, &openAIOAuthTLSRouterReaderStub{routers: map[int64]*model.TLSFingerprintRouter{
		routerID: {
			ID:                                       routerID,
			Enabled:                                  true,
			ChatGPTOAuthTokenTLSFingerprintProfileID: &profileID,
		},
	}}, &openAIOAuthTokenProfileResolverStub{profiles: map[int64]*tlsfingerprint.Profile{
		profileID: {Name: "oauth-token-profile"},
	}})

	result, err := svc.GenerateAuthURL(context.Background(), nil, "", PlatformOpenAI, &routerID)
	require.NoError(t, err)

	session, ok := svc.sessionStore.Get(result.SessionID)
	require.True(t, ok)
	require.Equal(t, routerID, session.TLSFingerprintRouterID)
}

func TestOpenAIOAuthService_GenerateAuthURL_MissingDedicatedTokenProfileReturnsFallbackWarning(t *testing.T) {
	routerID := int64(17)
	svc := NewOpenAIOAuthService(nil, &openaiOAuthClientAuthURLStub{})
	defer svc.Stop()
	svc.SetTokenTLSRouterDeps(nil, &openAIOAuthTLSRouterReaderStub{routers: map[int64]*model.TLSFingerprintRouter{
		routerID: {ID: routerID, Enabled: true},
	}}, &openAIOAuthTokenProfileResolverStub{})

	result, err := svc.GenerateAuthURL(context.Background(), nil, "", PlatformOpenAI, &routerID)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, result.Warning, "standard Go TLS fallback")

	session, ok := svc.sessionStore.Get(result.SessionID)
	require.True(t, ok)
	require.Equal(t, routerID, session.TLSFingerprintRouterID)
}
