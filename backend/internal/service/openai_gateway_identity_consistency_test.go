package service

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/platform/liveattestation"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func assertCodexOutboundIdentityTuple(t *testing.T, headers http.Header, wantUA, wantOriginator, wantVersion string) {
	t.Helper()
	require.Equal(t, wantUA, headers.Get("User-Agent"))
	require.Equal(t, wantOriginator, headers.Get("Originator"))
	require.Equal(t, wantVersion, headers.Get("Version"))
	require.Empty(t, headers.Get("codex_version"))
}

func TestOpenAIGatewayCodexIdentityTupleAcrossOutboundBuilders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const canonicalUA = "codex-tui/0.153.4 (Windows 10.0.26200; x86_64) xterm-256color (codex-tui; 0.153.4)"
	withCodexCanonicalUA(t, canonicalUA)

	newContext := func(path string) *gin.Context {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{"model":"gpt-5"}`)))
		c.Request.Header.Set("User-Agent", canonicalUA)
		c.Request.Header.Set("originator", "codex-tui")
		c.Request.Header.Set("version", "9.9.9")
		c.Request.Header.Set("x-oai-attestation", `{"v":1,"s":0,"t":"v1.client-envelope"}`)
		return c
	}
	account := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-account",
		},
	}
	store := liveattestation.NewAppServerAttestationStore(time.Minute)
	svc := &OpenAIGatewayService{codexAttestationStore: store}
	key := liveattestation.SessionKey{AccountID: account.ID, ConnectionID: "identity-consistency"}
	_, err := store.ObserveInitialize(key, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	_, _, err = store.BeginGenerate(key)
	require.NoError(t, err)
	require.NoError(t, store.AcceptGenerateResponse(key, []byte(`{"jsonrpc":"2.0","id":1,"result":{"headerValue":"v1.client-envelope"}}`)))
	attestedCtx := WithCodexAttestationRequestContext(context.Background(), CodexAttestationRequestContext{
		Key: key, Envelope: `{"v":1,"s":0,"t":"v1.client-envelope"}`,
	})
	body := []byte(`{"model":"gpt-5"}`)

	normal, err := svc.buildUpstreamRequest(
		attestedCtx, newContext("/v1/responses"), account, body, "token", false, "", true,
	)
	require.NoError(t, err)
	assertCodexOutboundIdentityTuple(t, normal.Header, canonicalUA, "codex-tui", "0.153.4")
	require.Equal(t, `{"v":1,"s":0,"t":"v1.client-envelope"}`, normal.Header.Get(liveAttestationHeader))

	passthrough, err := svc.buildUpstreamRequestOpenAIPassthrough(
		attestedCtx, newContext("/v1/responses"), account, body, "token",
	)
	require.NoError(t, err)
	assertCodexOutboundIdentityTuple(t, passthrough.Header, canonicalUA, "codex-tui", "0.153.4")
	require.Equal(t, `{"v":1,"s":0,"t":"v1.client-envelope"}`, passthrough.Header.Get(liveAttestationHeader))

	compact, err := svc.buildUpstreamRequest(
		attestedCtx, newContext("/v1/responses/compact"), account, body, "token", false, "", true,
	)
	require.NoError(t, err)
	assertCodexOutboundIdentityTuple(t, compact.Header, canonicalUA, "codex-tui", "0.153.4")
	require.Equal(t, `{"v":1,"s":0,"t":"v1.client-envelope"}`, compact.Header.Get(liveAttestationHeader))

	ws, _, err := svc.buildOpenAIWSHeaders(
		attestedCtx, newContext("/v1/responses"), account, "token",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		true, "", "", "", "", "",
	)
	require.NoError(t, err)
	assertCodexOutboundIdentityTuple(t, ws, canonicalUA, "codex-tui", "0.153.4")
	require.Equal(t, `{"v":1,"s":0,"t":"v1.client-envelope"}`, ws.Get(liveAttestationHeader))
}
