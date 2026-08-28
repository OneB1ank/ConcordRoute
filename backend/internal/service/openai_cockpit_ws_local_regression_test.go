package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestLocalCockpitWebSocketIngressIdentityMatchesHandshakeAndFirstFrame(t *testing.T) {
	for _, ingressMode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
		ingressMode := ingressMode
		t.Run(ingressMode, func(t *testing.T) {
			runLocalCockpitWebSocketIngressIdentityTest(t, ingressMode)
		})
	}
}

func runLocalCockpitWebSocketIngressIdentityTest(t *testing.T, ingressMode string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	upstreamConn := &openAIWSCaptureConn{
		readDelays: []time.Duration{0, 200 * time.Millisecond},
		events: [][]byte{
			[]byte(`{"type":"response.completed","response":{"id":"resp_cockpit_local","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_cockpit_local_2","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`),
		},
	}
	captureDialer := &openAIWSCaptureDialer{conn: upstreamConn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(captureDialer)
	defer pool.Close()
	svc := &OpenAIGatewayService{
		cfg:                       cfg,
		httpUpstream:              &httpUpstreamRecorder{},
		cache:                     &stubGatewayCache{},
		openaiWSResolver:          NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:             NewCodexToolCorrector(),
		openaiWSPool:              pool,
		openaiWSPassthroughDialer: captureDialer,
	}
	account := &Account{
		ID:          9291,
		Name:        "local-cockpit-ws",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra: map[string]any{
			"openai_oauth_responses_websockets_v2_mode": ingressMode,
			codexFingerprintModeExtraKey:                "cockpit",
			CodexFingerprintSeedExtraKey:                "b8ad8772-3594-4a7a-9f5b-d7394024a8c3",
		},
	}

	firstMessage := []byte(`{
		"type":"response.create",
		"model":"gpt-5.1",
		"stream":false,
		"prompt_cache_key":"client-cache-a",
		"client_metadata":{
			"session_id":"client-session-a",
			"thread_id":"client-thread-a",
			"turn_id":"client-turn-a",
			"x-codex-window-id":"client-thread-a:0",
			"x-codex-turn-metadata":"{\"installation_id\":\"client-install\",\"session_id\":\"client-session-a\",\"thread_id\":\"client-thread-a\",\"turn_id\":\"client-turn-a\",\"window_id\":\"client-thread-a:0\",\"prompt_cache_key\":\"client-cache-a\"}"
		},
		"input":[{"type":"message","role":"user","content":"hello"}]
	}`)
	secondMessage := []byte(`{
		"type":"response.create",
		"model":"gpt-5.1",
		"stream":false,
		"prompt_cache_key":"client-cache-a",
		"client_metadata":{
			"session_id":"client-session-a",
			"thread_id":"client-thread-a",
			"turn_id":"client-turn-b",
			"x-codex-window-id":"client-thread-a:0",
			"x-codex-turn-metadata":"{\"installation_id\":\"client-install\",\"session_id\":\"client-session-a\",\"thread_id\":\"client-thread-a\",\"turn_id\":\"client-turn-b\",\"window_id\":\"client-thread-a:0\",\"prompt_cache_key\":\"client-cache-a\"}"
		},
		"input":[{"type":"message","role":"user","content":"world"}]
	}`)

	clientHeaders := make(http.Header)
	clientHeaders.Set("User-Agent", "codex_cli_rs/0.145.0")
	clientHeaders.Set("session-id", "client-session-a")
	clientHeaders.Set("conversation_id", "client-cache-a")
	clientHeaders.Set("x-codex-installation-id", "client-install")
	clientHeaders.Set("x-codex-window-id", "client-thread-a:0")
	clientHeaders.Set("x-codex-turn-metadata", `{"installation_id":"client-install","session_id":"client-session-a","thread_id":"client-thread-a","turn_id":"client-turn-a","window_id":"client-thread-a:0","prompt_cache_key":"client-cache-a"}`)
	expectedIDs := bindCodexFingerprintIDsToAccount(
		resolveCodexFingerprintIDsFromRawRequest(account, clientHeaders, firstMessage),
		account,
	)
	require.NotNil(t, expectedIDs)

	serverErrCh := make(chan error, 1)
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()

		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)
		req := r.Clone(r.Context())
		req.Header = clientHeaders.Clone()
		ginCtx.Request = req

		readCtx, cancelRead := context.WithTimeout(r.Context(), 3*time.Second)
		msgType, message, readErr := conn.Read(readCtx)
		cancelRead()
		if readErr != nil {
			serverErrCh <- readErr
			return
		}
		if msgType != coderws.MessageText {
			serverErrCh <- errors.New("unexpected websocket message type")
			return
		}
		serverErrCh <- svc.ProxyResponsesWebSocketFromClient(r.Context(), ginCtx, conn, account, "oauth-token", message, nil)
	}))
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText, firstMessage))
	cancelWrite()
	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, event, readErr := clientConn.Read(readCtx)
	cancelRead()
	require.NoError(t, readErr)
	require.Equal(t, "resp_cockpit_local", gjson.GetBytes(event, "response.id").String())
	writeCtx, cancelWrite = context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText, secondMessage))
	cancelWrite()
	readCtx, cancelRead = context.WithTimeout(context.Background(), 3*time.Second)
	_, event, readErr = clientConn.Read(readCtx)
	cancelRead()
	require.NoError(t, readErr)
	require.Equal(t, "resp_cockpit_local_2", gjson.GetBytes(event, "response.id").String())
	_ = clientConn.Close(coderws.StatusNormalClosure, "done")

	select {
	case serverErr := <-serverErrCh:
		if serverErr != nil {
			require.True(t,
				strings.Contains(serverErr.Error(), "StatusNormalClosure") ||
					strings.Contains(serverErr.Error(), "connection closed"),
				"unexpected websocket shutdown: %v",
				serverErr,
			)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for websocket proxy")
	}

	require.Equal(t, expectedIDs.installationID, captureDialer.lastHeaders.Get("x-codex-installation-id"))
	require.Equal(t, expectedIDs.sessionID, captureDialer.lastHeaders.Get("session-id"))
	require.Equal(t, expectedIDs.sessionID, captureDialer.lastHeaders.Get("session_id"))
	require.Equal(t, expectedIDs.threadID, captureDialer.lastHeaders.Get("thread-id"))
	require.Equal(t, expectedIDs.promptCacheKey, captureDialer.lastHeaders.Get("conversation_id"))
	headerMetadata := captureDialer.lastHeaders.Get("x-codex-turn-metadata")
	upstreamTurnID := gjson.Get(headerMetadata, "turn_id").String()
	require.NotEmpty(t, upstreamTurnID)
	require.Len(t, upstreamConn.writes, 2)
	forwarded := requestToJSONString(upstreamConn.writes[0])
	require.Equal(t, expectedIDs.promptCacheKey, gjson.Get(forwarded, "prompt_cache_key").String())
	require.Equal(t, expectedIDs.installationID, gjson.Get(forwarded, "client_metadata.x-codex-installation-id").String())
	require.Equal(t, expectedIDs.sessionID, gjson.Get(forwarded, "client_metadata.session_id").String())
	require.Equal(t, expectedIDs.threadID, gjson.Get(forwarded, "client_metadata.thread_id").String())
	require.Equal(t, upstreamTurnID, gjson.Get(forwarded, "client_metadata.turn_id").String())
	require.Equal(t, expectedIDs.windowID, gjson.Get(forwarded, "client_metadata.x-codex-window-id").String())
	metadata := gjson.Get(forwarded, "client_metadata.x-codex-turn-metadata").String()
	require.Equal(t, upstreamTurnID, gjson.Get(metadata, "turn_id").String())

	forwardedSecond := requestToJSONString(upstreamConn.writes[1])
	require.Equal(t, expectedIDs.promptCacheKey, gjson.Get(forwardedSecond, "prompt_cache_key").String())
	require.Equal(t, expectedIDs.installationID, gjson.Get(forwardedSecond, "client_metadata.x-codex-installation-id").String())
	require.Equal(t, expectedIDs.sessionID, gjson.Get(forwardedSecond, "client_metadata.session_id").String())
	require.Equal(t, expectedIDs.threadID, gjson.Get(forwardedSecond, "client_metadata.thread_id").String())
	secondTurnID := gjson.Get(forwardedSecond, "client_metadata.turn_id").String()
	require.NotEmpty(t, secondTurnID)
	require.Equal(t, upstreamTurnID, secondTurnID, "同一 WS 连接必须复用首帧身份快照")
	secondMetadata := gjson.Get(forwardedSecond, "client_metadata.x-codex-turn-metadata").String()
	require.Equal(t, secondTurnID, gjson.Get(secondMetadata, "turn_id").String())
}
