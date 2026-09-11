package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestLocalCockpitWebSocketIngressIdentityMatchesHandshakeAndFirstFrame(t *testing.T) {
	for _, ingressMode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
		ingressMode := ingressMode
		t.Run(ingressMode, func(t *testing.T) {
			runLocalCockpitWebSocketIngressIdentityTest(t, ingressMode, false)
		})
	}
}

// 现代客户端在同一连接内压缩，后续帧必须切换窗口与显式缓存键。
func TestLocalCockpitWebSocketCompactionUpdatesFrameIdentity(t *testing.T) {
	for _, ingressMode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
		t.Run(ingressMode, func(t *testing.T) {
			runLocalCockpitWebSocketIngressIdentityTest(t, ingressMode, true)
		})
	}
}

// 实际两个 WS 入口均验证压缩后省略键、root 和 parent，不依赖只有显式键的用例。
func TestLocalCockpitWebSocketCompactionCarriesMissingCacheAndClearsRoot(t *testing.T) {
	for _, ingressMode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
		t.Run(ingressMode, func(t *testing.T) {
			runLocalCockpitWebSocketIngressIdentityTest(t, ingressMode, true, true)
		})
	}
}

func runLocalCockpitWebSocketIngressIdentityTest(t *testing.T, ingressMode string, compact bool, omitAfterCompact ...bool) {
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
		Credentials: map[string]any{"access_token": "oauth-token", "user_agent": "Codex Desktop/0.153.3 (Mac OS 26.5.2; arm64) unknown (Codex Desktop; 26.901.41123)"},
		Extra: map[string]any{
			"openai_oauth_responses_websockets_v2_mode": ingressMode,
			codexFingerprintModeExtraKey:                "cockpit",
			CodexFingerprintSeedExtraKey:                uuid.NewString(),
		},
	}

	firstMessage := []byte(`{
		"type":"response.create",
		"model":"gpt-5.1",
		"stream":false,
		"prompt_cache_key":"client-cache-a",
		"client_metadata":{
			"codex_version":"0.145.0",
			"session_id":"client-session-a",
			"thread_id":"client-thread-a",
			"turn_id":"client-turn-a",
			"x-codex-window-id":"client-thread-a:0",
			"x-codex-turn-metadata":"{\"codex_version\":\"0.145.0\",\"installation_id\":\"client-install\",\"session_id\":\"client-session-a\",\"thread_id\":\"client-thread-a\",\"turn_id\":\"client-turn-a\",\"window_id\":\"client-thread-a:0\",\"prompt_cache_key\":\"client-cache-a\"}"
		},
		"input":[{"type":"message","role":"user","content":"hello"}]
	}`)
	secondMessage := []byte(`{
		"type":"response.create",
		"model":"gpt-5.1",
		"stream":false,
		"prompt_cache_key":"client-cache-a",
		"client_metadata":{
			"codex_version":"0.145.0",
			"session_id":"client-session-a",
			"thread_id":"client-thread-a",
			"turn_id":"client-turn-b",
			"x-codex-window-id":"client-thread-a:0",
			"x-codex-turn-metadata":"{\"codex_version\":\"0.145.0\",\"installation_id\":\"client-install\",\"session_id\":\"client-session-a\",\"thread_id\":\"client-thread-a\",\"turn_id\":\"client-turn-b\",\"window_id\":\"client-thread-a:0\",\"prompt_cache_key\":\"client-cache-a\"}"
		},
		"input":[{"type":"message","role":"user","content":"world"}]
	}`)

	clientHeaders := make(http.Header)
	clientHeaders.Set("User-Agent", "codex_cli_rs/0.145.0")
	clientHeaders.Set("session-id", "client-session-a")
	clientHeaders.Set("conversation_id", "client-cache-a")
	clientHeaders.Set("x-codex-installation-id", "client-install")
	clientHeaders.Set("x-codex-window-id", "client-thread-a:0")
	clientHeaders.Set("x-codex-turn-metadata", `{"codex_version":"0.145.0","installation_id":"client-install","session_id":"client-session-a","thread_id":"client-thread-a","turn_id":"client-turn-a","window_id":"client-thread-a:0","prompt_cache_key":"client-cache-a"}`)
	if compact {
		clientHeaders.Set("User-Agent", "codex_cli_rs/0.153.4")
		for index, frame := range []*[]byte{&firstMessage, &secondMessage} {
			metadata := map[string]any{
				"codex_version": "0.153.4",
				"session_id":    "client-session-a", "thread_id": "client-thread-a",
				"turn_id":       []string{"client-turn-a", "client-turn-b"}[index],
				"window_id":     []string{"client-thread-a:0", "client-thread-a:1"}[index],
				"window_number": index, "parent_turn_id": "client-parent", "root_turn_id": "client-root",
			}
			encoded, err := json.Marshal(metadata)
			require.NoError(t, err)
			*frame, err = sjson.SetBytes(*frame, "client_metadata.x-codex-turn-metadata", string(encoded))
			require.NoError(t, err)
			*frame, err = sjson.SetBytes(*frame, "prompt_cache_key", []string{"client-cache-a", "client-cache-b"}[index])
			require.NoError(t, err)
		}
	}
	omit := len(omitAfterCompact) > 0 && omitAfterCompact[0]
	if omit {
		var err error
		secondMessage, err = sjson.DeleteBytes(secondMessage, "prompt_cache_key")
		require.NoError(t, err)
		metadata := gjson.GetBytes(secondMessage, "client_metadata.x-codex-turn-metadata").String()
		for _, key := range []string{"parent_turn_id", "root_turn_id"} {
			metadata, err = sjson.Delete(metadata, key)
			require.NoError(t, err)
		}
		secondMessage, err = sjson.SetBytes(secondMessage, "client_metadata.x-codex-turn-metadata", metadata)
		require.NoError(t, err)
	}
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
	// 首帧及后续帧均按握手 UA 的引擎版本对齐，原有会话/缓存断言继续生效。
	require.Equal(t, "0.153.3", captureDialer.lastHeaders.Get("Version"))
	require.Empty(t, captureDialer.lastHeaders.Get("codex_version"))
	headerMetadata := captureDialer.lastHeaders.Get("x-codex-turn-metadata")
	require.Equal(t, "0.153.3", gjson.Get(headerMetadata, "codex_version").String())
	upstreamTurnID := gjson.Get(headerMetadata, "turn_id").String()
	require.NotEmpty(t, upstreamTurnID)
	require.Len(t, upstreamConn.writes, 2)
	forwarded := requestToJSONString(upstreamConn.writes[0])
	require.Equal(t, "0.153.3", gjson.Get(forwarded, "client_metadata.codex_version").String())
	require.Equal(t, expectedIDs.promptCacheKey, gjson.Get(forwarded, "prompt_cache_key").String())
	require.Equal(t, expectedIDs.installationID, gjson.Get(forwarded, "client_metadata.x-codex-installation-id").String())
	require.Equal(t, expectedIDs.sessionID, gjson.Get(forwarded, "client_metadata.session_id").String())
	require.Equal(t, expectedIDs.threadID, gjson.Get(forwarded, "client_metadata.thread_id").String())
	require.Equal(t, upstreamTurnID, gjson.Get(forwarded, "client_metadata.turn_id").String())
	require.Equal(t, expectedIDs.windowID, gjson.Get(forwarded, "client_metadata.x-codex-window-id").String())
	metadata := gjson.Get(forwarded, "client_metadata.x-codex-turn-metadata").String()
	require.Equal(t, "0.153.3", gjson.Get(metadata, "codex_version").String())
	require.Equal(t, upstreamTurnID, gjson.Get(metadata, "turn_id").String())

	forwardedSecond := requestToJSONString(upstreamConn.writes[1])
	require.Equal(t, "0.153.3", gjson.Get(forwardedSecond, "client_metadata.codex_version").String())
	if compact && !omit {
		require.Equal(t, "client-cache-b", gjson.Get(forwardedSecond, "prompt_cache_key").String())
	} else {
		require.Equal(t, expectedIDs.promptCacheKey, gjson.Get(forwardedSecond, "prompt_cache_key").String())
	}
	require.Equal(t, expectedIDs.installationID, gjson.Get(forwardedSecond, "client_metadata.x-codex-installation-id").String())
	require.Equal(t, expectedIDs.sessionID, gjson.Get(forwardedSecond, "client_metadata.session_id").String())
	require.Equal(t, expectedIDs.threadID, gjson.Get(forwardedSecond, "client_metadata.thread_id").String())
	secondTurnID := gjson.Get(forwardedSecond, "client_metadata.turn_id").String()
	require.NotEmpty(t, secondTurnID)
	if compact {
		require.NotEqual(t, upstreamTurnID, secondTurnID, "新的客户端回合需独立身份，连接握手保持首帧快照")
		require.Equal(t, expectedIDs.threadID+":1", gjson.Get(forwardedSecond, "client_metadata.x-codex-window-id").String())
		require.Equal(t, gjson.Get(forwarded, "client_metadata.context_window_id").String(), gjson.Get(forwardedSecond, "client_metadata.previous_window_id").String())
		for index, frame := range []string{forwarded, forwardedSecond} {
			require.False(t, gjson.Get(frame, "parent_turn_id").Exists())
			require.False(t, gjson.Get(frame, "root_turn_id").Exists())
			if omit && index == 1 {
				require.False(t, gjson.Get(frame, "client_metadata.root_turn_id").Exists())
				require.False(t, gjson.Get(frame, "client_metadata.parent_turn_id").Exists())
				embedded := gjson.Get(frame, "client_metadata.x-codex-turn-metadata").String()
				require.False(t, gjson.Get(embedded, "root_turn_id").Exists())
			} else {
				require.Equal(t, expectedIDs.parentTurnID, gjson.Get(frame, "client_metadata.parent_turn_id").String())
			}
			var wire struct {
				Metadata map[string]string `json:"client_metadata"`
			}
			require.NoError(t, json.Unmarshal([]byte(frame), &wire))
		}
	} else {
		require.Equal(t, upstreamTurnID, secondTurnID, "旧客户端保持连接内兼容身份行为")
	}
	secondMetadata := gjson.Get(forwardedSecond, "client_metadata.x-codex-turn-metadata").String()
	require.Equal(t, "0.153.3", gjson.Get(secondMetadata, "codex_version").String())
	require.Equal(t, secondTurnID, gjson.Get(secondMetadata, "turn_id").String())
}
