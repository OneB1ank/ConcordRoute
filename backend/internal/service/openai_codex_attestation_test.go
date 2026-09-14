package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/platform/liveattestation"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCodexAttestationContextOnlyAppliesToNegotiatedOAuthSession(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	key := liveattestation.SessionKey{AccountID: 42, ConnectionID: "conn-1", SessionID: "sess-1"}
	_, err := store.ObserveInitialize(key, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	require.NoError(t, store.AcceptGenerateResponse(key, []byte(`{"jsonrpc":"2.0","id":1,"result":{"headerValue":"v1.client-token"}}`)))

	requestContext := WithCodexAttestationRequestContext(context.Background(), CodexAttestationRequestContext{
		Key: key, Envelope: `{"v":1,"s":0,"t":"v1.client-token"}`,
	})
	oauthAccount := &Account{ID: 42, Type: AccountTypeOAuth}
	headers := make(http.Header)
	svc.applyCodexClientAttestation(requestContext, oauthAccount, headers)
	require.Equal(t, `{"v":1,"s":0,"t":"v1.client-token"}`, headers.Get(liveAttestationHeader))

	apiKeyAccount := &Account{ID: 42, Type: AccountTypeAPIKey}
	headers = make(http.Header)
	svc.applyCodexClientAttestation(requestContext, apiKeyAccount, headers)
	require.Empty(t, headers.Get(liveAttestationHeader))
}

func TestCodexAttestationRequiresChatGPTAuthMode(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	key := liveattestation.SessionKey{AccountID: 42, ConnectionID: "conn-auth-mode"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	require.NoError(t, store.AcceptGenerateResponse(key, []byte(`{"id":1,"result":{"headerValue":"v1.client-token"}}`)))

	ctx := WithCodexAttestationRequestContext(context.Background(), CodexAttestationRequestContext{
		Key: key, Envelope: `{"v":1,"s":0,"t":"v1.client-token"}`,
	})
	cases := []struct {
		name        string
		credentials map[string]any
	}{
		{name: "personal access token", credentials: map[string]any{"auth_mode": OpenAIAuthModePersonalAccessToken}},
		{name: "agent identity", credentials: map[string]any{"auth_mode": OpenAIAuthModeAgentIdentity}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := make(http.Header)
			svc.applyCodexClientAttestation(ctx, &Account{
				ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: tc.credentials,
			}, headers)
			require.Empty(t, headers.Get(liveAttestationHeader))
		})
	}
}

func TestCodexAttestationContextRejectsCrossAccountAndUntrustedHeader(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	key := liveattestation.SessionKey{AccountID: 42, ConnectionID: "conn-1"}
	_, err := store.ObserveInitialize(key, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	require.NoError(t, store.AcceptGenerateResponse(key, []byte(`{"jsonrpc":"2.0","id":1,"result":{"headerValue":"v1.client-token"}}`)))

	requestContext := WithCodexAttestationRequestContext(context.Background(), CodexAttestationRequestContext{
		Key: liveattestation.SessionKey{AccountID: 99, ConnectionID: "conn-1"},
	})
	headers := http.Header{"X-Oai-Attestation": []string{`{"v":1,"s":0,"t":"v1.untrusted"}`}}
	svc.applyCodexClientAttestation(requestContext, &Account{ID: 42, Type: AccountTypeOAuth}, headers)
	require.Empty(t, headers.Get(liveAttestationHeader))
}

func TestCodexAttestationContextDoesNotFallBackToInboundHeaderWhenEmpty(t *testing.T) {
	svc := &OpenAIGatewayService{}
	ctx := WithCodexAttestationRequestContext(context.Background(), CodexAttestationRequestContext{
		Key: liveattestation.SessionKey{AccountID: 42, ConnectionID: "bound-empty"},
	})
	headers := make(http.Header)
	svc.applyCodexClientAttestation(ctx, &Account{ID: 42, Type: AccountTypeOAuth}, headers,
		codexClientAttestationCandidate{
			Envelope:   `{"v":1,"s":0,"t":"v1.inbound"}`,
			UserAgent:  "Codex Desktop/0.153.4 (Windows 10.0.26200; x86_64)",
			Originator: "Codex Desktop",
		})
	require.Empty(t, headers.Get(liveAttestationHeader), "an app-server-bound request must not use a direct fallback proof")
}

func TestWindowsUAAndTLSIdentityDoNotSynthesizeAttestation(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	headers := make(http.Header)
	headers.Set("User-Agent", "Codex Desktop/0.153.4 (Windows 10.0.26200; x86_64)")
	account := &Account{ID: 42, Type: AccountTypeOAuth}

	// Windows UA/TLS 只描述客户端与连接特征；没有 app-server context 时不签发证明头。
	svc.applyCodexClientAttestation(context.Background(), account, headers)
	require.Empty(t, headers.Get(liveAttestationHeader))
}

func TestCodexAttestationDoesNotOverrideOutboundUAOrOriginator(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	key := liveattestation.SessionKey{AccountID: 42, ConnectionID: "conn-identity"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	require.NoError(t, store.AcceptGenerateResponse(key, []byte(`{"id":1,"result":{"headerValue":"v1.identity-token"}}`)))

	ctx := WithCodexAttestationRequestContext(context.Background(), CodexAttestationRequestContext{
		Key: key, Envelope: `{"v":1,"s":0,"t":"v1.identity-token"}`,
	})
	headers := make(http.Header)
	ua := "codex-tui/0.153.4 (Windows 10.0.26200; x86_64) xterm-256color (codex-tui; 0.153.4)"
	headerOriginator := "codex-tui"
	headers.Set("User-Agent", ua)
	headers.Set("originator", headerOriginator)
	svc.applyCodexClientAttestation(ctx, &Account{ID: 42, Type: AccountTypeOAuth}, headers)

	require.Equal(t, ua, headers.Get("User-Agent"))
	require.Equal(t, headerOriginator, headers.Get("originator"))
	require.Equal(t, `{"v":1,"s":0,"t":"v1.identity-token"}`, headers.Get(liveAttestationHeader))
}

func TestCodexAttestationClientEnvelopeRequiresTrustedIdentityWithoutTransport(t *testing.T) {
	validEnvelope := `{"v":1,"s":0,"t":"v1.client-token"}`
	cases := []struct {
		name       string
		envelope   string
		userAgent  string
		originator string
		wantHeader bool
	}{
		{
			name:       "windows codex tui identity",
			userAgent:  "codex-tui/0.153.4 (Windows 10.0.26200; x86_64) xterm-256color (codex-tui; 0.153.4)",
			originator: "codex-tui",
			wantHeader: true,
		},
		{
			name:       "spoofed originator rejected",
			userAgent:  "codex-tui_evil/0.153.4 (Windows 10.0.26200; x86_64)",
			originator: "codex-tui_evil",
		},
		{
			name:       "crlf user agent rejected",
			userAgent:  "codex-tui/0.153.4\r\nX-Injected: 1",
			originator: "codex-tui",
		},
		{
			name:       "crlf envelope rejected",
			envelope:   validEnvelope + "\r\nX-Injected: 1",
			userAgent:  "codex-tui/0.153.4 (Windows 10.0.26200; x86_64)",
			originator: "codex-tui",
			// Keep the control bytes in the raw header value. The validator
			// must reject them before any surrounding whitespace normalization.
			wantHeader: false,
		},
		{
			name:       "middle prefix spoof rejected",
			userAgent:  "evil codex-tui/0.153.4 (Windows 10.0.26200; x86_64)",
			originator: "codex-tui",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &OpenAIGatewayService{}
			headers := make(http.Header)
			envelope := validEnvelope
			if tc.envelope != "" {
				envelope = tc.envelope
			}
			svc.applyCodexClientAttestation(context.Background(), &Account{ID: 42, Type: AccountTypeOAuth}, headers,
				codexClientAttestationCandidate{Envelope: envelope, UserAgent: tc.userAgent, Originator: tc.originator})
			if tc.wantHeader {
				require.Equal(t, validEnvelope, headers.Get(liveAttestationHeader))
			} else {
				require.Empty(t, headers.Get(liveAttestationHeader))
			}
		})
	}
}

func TestCodexAttestationClientTokenRemainsOpaqueWhenRelayed(t *testing.T) {
	svc := &OpenAIGatewayService{}
	headers := make(http.Header)
	svc.applyCodexClientAttestation(context.Background(), &Account{ID: 42, Type: AccountTypeOAuth}, headers,
		codexClientAttestationCandidate{
			Envelope:   ` {"v": 1, "s": 0, "t": " opaque-token "} `,
			UserAgent:  "Codex Desktop/0.153.4 (Windows 10.0.26200; x86_64)",
			Originator: "Codex Desktop",
		})
	require.Equal(t, `{"v":1,"s":0,"t":" opaque-token "}`, headers.Get(liveAttestationHeader))
}
func TestWithCodexAttestationRequestContextAcceptsNilParentContext(t *testing.T) {
	value := CodexAttestationRequestContext{Key: liveattestation.SessionKey{AccountID: 42, ConnectionID: "conn-nil"}}
	ctx := WithCodexAttestationRequestContext(nil, value)
	got, ok := codexAttestationContextFrom(ctx)
	require.True(t, ok)
	require.Equal(t, value.Key, got.Key)
}

func TestBuildUpstreamRequestUsesOnlyNegotiatedClientAttestation(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	key := liveattestation.SessionKey{AccountID: 42, ConnectionID: "conn-1"}
	_, err := store.ObserveInitialize(key, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	require.NoError(t, store.AcceptGenerateResponse(key, []byte(`{"jsonrpc":"2.0","id":1,"result":{"headerValue":"v1.client-token"}}`)))

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-5"}`)))
	ginContext.Request.Header.Set("x-oai-attestation", `{"v":1,"s":0,"t":"v1.untrusted"}`)
	account := &Account{ID: 42, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "chatgpt-acc"}}

	plain, err := svc.buildUpstreamRequest(ginContext.Request.Context(), ginContext, account, []byte(`{"model":"gpt-5"}`), "token", false, "", false)
	require.NoError(t, err)
	require.Empty(t, plain.Header.Get(liveAttestationHeader))
	ginContext.Request.Header.Set("User-Agent", "Codex Desktop/0.153.4 (Windows 10.0.26200; x86_64)")
	ginContext.Request.Header.Set("originator", "Codex Desktop")
	trusted, err := svc.buildUpstreamRequest(ginContext.Request.Context(), ginContext, account, []byte(`{"model":"gpt-5"}`), "token", false, "", false)
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":0,"t":"v1.untrusted"}`, trusted.Header.Get(liveAttestationHeader))

	attestedCtx := WithCodexAttestationRequestContext(ginContext.Request.Context(), CodexAttestationRequestContext{
		Key: key, Envelope: `{"v":1,"s":0,"t":"v1.client-token"}`,
	})
	attested, err := svc.buildUpstreamRequest(attestedCtx, ginContext, account, []byte(`{"model":"gpt-5"}`), "token", false, "", false)
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":0,"t":"v1.client-token"}`, attested.Header.Get(liveAttestationHeader))
}

func TestCodexAppServerAttestationAdapterRelaysClientToken(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	adapter, err := svc.NewCodexAppServerAttestationAdapter(liveattestation.SessionKey{
		AccountID: 42, ConnectionID: "conn-adapter", SessionID: "sess-adapter", ThreadID: "thread-adapter",
	})
	require.NoError(t, err)

	capability, err := adapter.ObserveInitialize([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	require.True(t, capability)

	payload, requestID, err := adapter.BeginGenerate()
	require.NoError(t, err)
	require.NotEmpty(t, payload)
	require.Positive(t, requestID)
	require.NoError(t, adapter.AcceptGenerateResponse([]byte(`{"jsonrpc":"2.0","id":1,"result":{"headerValue":"v1.client-token"}}`)))

	requestContext, err := adapter.RequestContext()
	require.NoError(t, err)
	require.Equal(t, int64(42), requestContext.Key.AccountID)
	require.Equal(t, "conn-adapter", requestContext.Key.ConnectionID)
	require.Equal(t, `{"v":1,"s":0,"t":"v1.client-token"}`, requestContext.Envelope)
	require.Equal(t, `{"v":1,"s":0,"t":"v1.client-token"}`, storeHeaderForTest(t, store, requestContext.Key))

	adapter.Clear()
	_, ok := store.HeaderForRequest(requestContext.Key)
	require.False(t, ok)
}

func TestCodexAttestationContextUsesCapturedEnvelopeAfterStoreChanges(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	key := liveattestation.SessionKey{AccountID: 42, ConnectionID: "conn-snapshot"}
	_, err := store.ObserveInitialize(key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	adapter, err := svc.NewCodexAppServerAttestationAdapter(key)
	require.NoError(t, err)
	require.NoError(t, adapter.AcceptGenerateResponse([]byte(`{"id":1,"result":{"headerValue":"opaque-a"}}`)))
	requestContext, err := adapter.RequestContext()
	require.NoError(t, err)

	// A later generate on the same store key must not change the proof already
	// captured for the in-flight upstream request.
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	require.NoError(t, store.AcceptGenerateResponse(key, []byte(`{"id":2,"result":{"headerValue":"opaque-b"}}`)))

	headers := make(http.Header)
	svc.applyCodexClientAttestation(
		WithCodexAttestationRequestContext(context.Background(), requestContext),
		&Account{ID: 42, Type: AccountTypeOAuth},
		headers,
	)
	require.Equal(t, `{"v":1,"s":0,"t":"opaque-a"}`, headers.Get(liveAttestationHeader))
}

func TestCodexAppServerAttestationAdapterAllowsRemoteBridgeLatency(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	adapter, err := svc.NewCodexAppServerAttestationAdapter(liveattestation.SessionKey{
		AccountID: 42, ConnectionID: "remote-bridge-latency",
	})
	require.NoError(t, err)
	_, err = adapter.ObserveInitialize([]byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)

	header, err := adapter.GenerateForRequestWithTimeout(context.Background(), func(_ context.Context, _ []byte) ([]byte, error) {
		time.Sleep(150 * time.Millisecond)
		return []byte(`{"id":1,"result":{"headerValue":"v1.remote-token"}}`), nil
	}, 500*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":0,"t":"v1.remote-token"}`, header)
}

func TestCodexAppServerAttestationAdapterGeneratesJustInTimeHeader(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	adapter, err := svc.NewCodexAppServerAttestationAdapter(liveattestation.SessionKey{
		AccountID: 42, ConnectionID: "conn-jit", SessionID: "sess-jit", ThreadID: "thread-jit",
	})
	require.NoError(t, err)
	_, err = adapter.ObserveInitialize([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	header, err := adapter.GenerateForRequest(context.Background(), func(_ context.Context, payload []byte) ([]byte, error) {
		require.Contains(t, string(payload), `"method":"attestation/generate"`)
		require.Contains(t, string(payload), `"params":{}`)
		return []byte(`{"jsonrpc":"2.0","id":1,"result":{"headerValue":"v1.jit-token"}}`), nil
	})
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":0,"t":"v1.jit-token"}`, header)
}

func TestCodexAppServerAttestationAdapterMapsTransportFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want string
	}{
		{name: "request_failed", ctx: func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, want: `{"v":1,"s":2}`},
		{name: "timeout", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), time.Nanosecond)
		}, want: `{"v":1,"s":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := liveattestation.NewAppServerAttestationStore(time.Minute)
			svc := &OpenAIGatewayService{codexAttestationStore: store}
			adapter, err := svc.NewCodexAppServerAttestationAdapter(liveattestation.SessionKey{AccountID: 42, ConnectionID: tc.name})
			require.NoError(t, err)
			_, err = adapter.ObserveInitialize([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
			require.NoError(t, err)
			ctx, cancel := tc.ctx()
			defer cancel()
			header, err := adapter.GenerateForRequest(ctx, func(ctx context.Context, _ []byte) ([]byte, error) {
				if tc.name == "timeout" {
					<-ctx.Done()
				}
				return nil, errors.New("transport failed")
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, header)
		})
	}
}

func TestCodexAppServerAttestationAdapterMapsLateSuccessToTimeout(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	adapter, err := svc.NewCodexAppServerAttestationAdapter(liveattestation.SessionKey{AccountID: 42, ConnectionID: "late-success"})
	require.NoError(t, err)
	_, err = adapter.ObserveInitialize([]byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	header, err := adapter.GenerateForRequestWithTimeout(context.Background(), func(_ context.Context, _ []byte) ([]byte, error) {
		time.Sleep(10 * time.Millisecond)
		return []byte(`{"id":1,"result":{"headerValue":"late-token"}}`), nil
	}, time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":1}`, header)
}

func TestCodexAppServerAttestationAdapterMapsCanceledRoundTripToCanceledStatus(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	adapter, err := svc.NewCodexAppServerAttestationAdapter(liveattestation.SessionKey{
		AccountID: 42, ConnectionID: "canceled-round-trip",
	})
	require.NoError(t, err)
	_, err = adapter.ObserveInitialize([]byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	header, err := adapter.GenerateForRequestWithTimeout(ctx, func(ctx context.Context, _ []byte) ([]byte, error) {
		return nil, ctx.Err()
	}, time.Second)
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":3}`, header)
}

func TestCodexAppServerAttestationAdapterMapsMismatchedResponseIDToMalformedStatus(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	adapter, err := svc.NewCodexAppServerAttestationAdapter(liveattestation.SessionKey{
		AccountID: 42, ConnectionID: "mismatched-response-id",
	})
	require.NoError(t, err)
	_, err = adapter.ObserveInitialize([]byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	header, err := adapter.GenerateForRequestWithTimeout(context.Background(), func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte(`{"id":999,"result":{"headerValue":"v1.wrong-id"}}`), nil
	}, time.Second)
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":4}`, header)
}

func TestCodexAppServerAttestationAdapterMapsMismatchedClientErrorIDToMalformedStatus(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	adapter, err := svc.NewCodexAppServerAttestationAdapter(liveattestation.SessionKey{
		AccountID: 42, ConnectionID: "mismatched-client-error-id",
	})
	require.NoError(t, err)
	_, err = adapter.ObserveInitialize([]byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	header, err := adapter.GenerateForRequestWithTimeout(context.Background(), func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte(`{"id":999,"error":{"code":-32000,"message":"client unavailable"}}`), nil
	}, time.Second)
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":4}`, header)
}

func TestCodexAppServerAttestationAdapterMapsClientRPCErrorToRequestFailedStatus(t *testing.T) {
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	adapter, err := svc.NewCodexAppServerAttestationAdapter(liveattestation.SessionKey{
		AccountID: 42, ConnectionID: "client-rpc-error",
	})
	require.NoError(t, err)
	_, err = adapter.ObserveInitialize([]byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	header, err := adapter.GenerateForRequestWithTimeout(context.Background(), func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte(`{"id":1,"error":{"code":-32000,"message":"client unavailable"}}`), nil
	}, time.Second)
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":2}`, header)
}

func storeHeaderForTest(t *testing.T, store *liveattestation.AppServerAttestationStore, key liveattestation.SessionKey) string {
	t.Helper()
	header, ok := store.HeaderForRequest(key)
	require.True(t, ok)
	return header
}
