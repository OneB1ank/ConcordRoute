package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/platform/liveattestation"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCodexAppServerBridgeRoundTripBindsAttestationContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := &OpenAIGatewayService{
		codexAttestationStore: liveattestation.NewAppServerAttestationStore(time.Minute),
		codexAppServerBridges: newCodexAppServerBridgeRegistry(),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_ = svc.ServeCodexAppServerBridge(ctx, 42, conn, "session-a", "thread-a")
	}))
	defer server.Close()

	client, _, err := coderws.Dial(ctx, "ws"+server.URL[len("http"):], nil)
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()

	initialize := []byte(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"codex","version":"0.153.4"},"capabilities":{"requestAttestation":true}}}`)
	require.NoError(t, client.Write(ctx, coderws.MessageText, initialize))
	_, response, err := client.Read(ctx)
	require.NoError(t, err)
	_, hasJSONRPC := objectValue(response)["jsonrpc"]
	require.False(t, hasJSONRPC)
	require.Equal(t, float64(1), numberValue(response, "id"))
	require.Equal(t, "ConcordRoute app-server bridge", stringValue(response, "result.userAgent"))
	require.Equal(t, "/", stringValue(response, "result.codexHome"))
	require.Equal(t, "unix", stringValue(response, "result.platformFamily"))
	require.Equal(t, "linux", stringValue(response, "result.platformOs"))
	// Official app-server clients send an initialized notification without a
	// jsonrpc field after receiving initialize.
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"method":"initialized"}`)))
	// The notification is consumed by the bridge read loop asynchronously;
	// wait until the registry publishes the initialized capability before
	// starting the request-side attestation round trip.
	readyDeadline := time.Now().Add(time.Second)
	for {
		bridge, findErr := svc.codexAppServerBridges.find(42, "session-a", "thread-a")
		if findErr == nil && bridge != nil {
			break
		}
		if time.Now().After(readyDeadline) {
			t.Fatal("app-server bridge did not process initialized notification")
		}
		time.Sleep(time.Millisecond)
	}

	account := &Account{ID: 99, Type: AccountTypeOAuth}
	generateDone := make(chan struct{})
	var bound context.Context
	var bindErr error
	go func() {
		bound, bindErr = svc.bindCodexAppServerAttestationContextForAPIKey(ctx, 42, account, "session-a", "thread-a")
		close(generateDone)
	}()
	_, generateRequest, err := client.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "attestation/generate", stringValue(generateRequest, "method"))
	requestID := numberValue(generateRequest, "id")
	responseBody, err := json.Marshal(map[string]any{
		"id":     requestID,
		"result": map[string]any{"token": "v1.test-token"},
	})
	require.NoError(t, err)
	require.NoError(t, client.Write(ctx, coderws.MessageText, responseBody))
	select {
	case <-generateDone:
	case <-time.After(time.Second):
		t.Fatal("attestation round trip did not finish")
	}
	require.NoError(t, bindErr)
	_, ok := svc.codexAttestationStore.HeaderForRequest(liveattestation.SessionKey{AccountID: 99, ConnectionID: "unused"})
	require.False(t, ok)
	requestContext, ok := codexAttestationContextFrom(bound)
	require.True(t, ok)
	require.Equal(t, int64(99), requestContext.Key.AccountID)
	require.Equal(t, "session-a", requestContext.Key.SessionID)
	require.NotEmpty(t, requestContext.Key.ConnectionID)

	// A single bridge is pinned to the first OAuth account that consumed its
	// proof channel.  A later scheduler choice on the same API key proceeds
	// without attaching that proof to the other account.
	otherContext, otherErr := svc.bindCodexAppServerAttestationContextForAPIKey(
		ctx, 42, &Account{ID: 100, Type: AccountTypeOAuth}, "session-a", "thread-a",
	)
	require.NoError(t, otherErr)
	_, otherBound := codexAttestationContextFrom(otherContext)
	require.False(t, otherBound)

	require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
	deadline := time.Now().Add(time.Second)
	for liveattestation.AppServerAttestationTransportEnabled() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	require.False(t, liveattestation.AppServerAttestationTransportEnabled())
}

func TestCodexAppServerBridgeAcceptsHTTP11WebSocketUpgrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Proto
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close(coderws.StatusNormalClosure, "done")
	}))
	defer server.Close()

	// coder/websocket 接受 http/https URL，并通过 HTTP/1.1 完成 WebSocket 握手，
	// 与界面展示的采集器地址保持一致。
	client, _, err := coderws.Dial(ctx, server.URL, nil)
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	select {
	case proto := <-seen:
		require.Equal(t, "HTTP/1.1", proto)
	case <-ctx.Done():
		t.Fatal("HTTP/1.1 WebSocket upgrade was not observed")
	}
}

func TestCodexAppServerBridgeCollectorCapturesRealRoundTripSummary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collector := NewCodexAppServerAttestationCollector()
	collector.Start()
	session, err := collector.CreateSession()
	require.NoError(t, err)
	svc := &OpenAIGatewayService{
		codexAttestationStore:     liveattestation.NewAppServerAttestationStore(time.Minute),
		codexAppServerBridges:     newCodexAppServerBridgeRegistry(),
		codexAttestationCollector: collector,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, acceptErr := coderws.Accept(w, r, nil)
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_ = svc.serveCodexAppServerBridge(ctx, 42, conn, "session-capture", "thread-capture", CodexAttestationHandshakeMetadata{
			HTTPProtocol: r.Proto,
			Transport:    "websocket",
			UserAgent:    r.Header.Get("User-Agent"),
			Originator:   r.Header.Get("originator"),
		}, session.Token)
	}))
	defer server.Close()

	client, _, err := coderws.Dial(ctx, "ws"+server.URL[len("http"):], &coderws.DialOptions{HTTPHeader: http.Header{
		"User-Agent": {"codex-tui/0.153.4 (Windows)"},
		"originator": {"codex_cli_rs"},
	}})
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"codex-tui","version":"0.153.4"},"capabilities":{"requestAttestation":true}}}`)))
	_, _, err = client.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"method":"initialized"}`)))

	deadline := time.Now().Add(time.Second)
	for {
		bridge, findErr := svc.codexAppServerBridges.find(42, "session-capture", "thread-capture")
		if findErr == nil && bridge != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("collector bridge did not initialize")
		}
		time.Sleep(time.Millisecond)
	}

	done := make(chan struct{})
	go func() {
		_, _ = svc.bindCodexAppServerAttestationContextForAPIKey(ctx, 42, &Account{ID: 99, Type: AccountTypeOAuth}, "session-capture", "thread-capture")
		close(done)
	}()
	_, generateRequest, err := client.Read(ctx)
	require.NoError(t, err)
	requestID := numberValue(generateRequest, "id")
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"id":`+fmt.Sprintf("%.0f", requestID)+`,"result":{"headerValue":"v1.collector-proof"}}`)))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("collector attestation round trip did not finish")
	}
	records, err := collector.ListCaptures(session.Token)
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, "generate", records[0].Event)
	require.Equal(t, "success", records[0].Status)
	require.Equal(t, int64(42), records[0].APIKeyID)
	require.Equal(t, int64(99), records[0].AccountID)
	require.Equal(t, "codex-tui", records[0].ClientName)
	require.Equal(t, "HTTP/1.1", records[1].HandshakeProtocol)
	require.Equal(t, "websocket", records[1].HandshakeTransport)
	require.Equal(t, "codex-tui/0.153.4 (Windows)", records[1].HandshakeUserAgent)
	require.Equal(t, "codex_cli_rs", records[1].HandshakeOriginator)
}

func TestCodexAppServerBridgeHandlerCapturesOfficialHeaderValue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	collector := NewCodexAppServerAttestationCollector()
	collector.Start()
	session, err := collector.CreateSession()
	require.NoError(t, err)
	svc := &OpenAIGatewayService{
		codexAttestationStore:     liveattestation.NewAppServerAttestationStore(time.Minute),
		codexAppServerBridges:     newCodexAppServerBridgeRegistry(),
		codexAttestationCollector: collector,
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/backend-api/codex/app-server", func(c *gin.Context) {
		svc.HandleCodexAppServerBridge(c, 42)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	endpoint := "ws" + server.URL[len("http"):]
	endpoint += "/backend-api/codex/app-server"
	endpoint += "?" + session.BridgeQuery
	client, _, err := coderws.Dial(ctx, endpoint, &coderws.DialOptions{HTTPHeader: http.Header{
		"User-Agent": {"codex-tui/0.153.4 (Windows)"},
		"originator": {"codex_cli_rs"},
	}})
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"codex-tui","version":"0.153.4"},"capabilities":{"requestAttestation":true}}}`)))
	_, _, err = client.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"method":"initialized"}`)))

	deadline := time.Now().Add(time.Second)
	for {
		bridge, findErr := svc.codexAppServerBridges.find(42, "", "")
		if findErr == nil && bridge != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handler did not register initialized bridge")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() {
		_, _ = svc.bindCodexAppServerAttestationContextForAPIKey(ctx, 42, &Account{ID: 99, Type: AccountTypeOAuth}, "", "")
		close(done)
	}()
	_, generateRequest, err := client.Read(ctx)
	require.NoError(t, err)
	requestID := numberValue(generateRequest, "id")
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"id":`+fmt.Sprintf("%.0f", requestID)+`,"result":{"headerValue":"v1.handler-proof"}}`)))
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("handler attestation round trip did not finish")
	}
	records, err := collector.ListCaptures(session.Token)
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, "success", records[0].Status)
	require.Equal(t, "codex-tui/0.153.4 (Windows)", records[0].HandshakeUserAgent)
	require.Equal(t, "codex_cli_rs", records[0].HandshakeOriginator)
	require.Equal(t, "HTTP/1.1", records[0].HandshakeProtocol)
}

func TestCodexAppServerBridgeDoesNotRegisterUnnegotiatedConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := newCodexAppServerBridgeRegistry()
	svc := &OpenAIGatewayService{
		codexAttestationStore: liveattestation.NewAppServerAttestationStore(time.Minute),
		codexAppServerBridges: registry,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_ = svc.ServeCodexAppServerBridge(ctx, 77, conn, "session-no-proof", "thread-no-proof")
	}))
	defer server.Close()

	client, _, err := coderws.Dial(ctx, "ws"+server.URL[len("http"):], nil)
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":false}}}`)))
	_, _, err = client.Read(ctx)
	require.NoError(t, err)

	registry.mu.RLock()
	_, registered := registry.bridges[77]
	registry.mu.RUnlock()
	require.False(t, registered, "a non-capable connection must not consume bridge registry capacity")

	cancel()
}

func TestCodexAppServerBridgeRejectsInitializeWithoutRequestID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := &OpenAIGatewayService{
		codexAttestationStore: liveattestation.NewAppServerAttestationStore(time.Minute),
		codexAppServerBridges: newCodexAppServerBridgeRegistry(),
	}
	errCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		errCh <- svc.ServeCodexAppServerBridge(ctx, 78, conn, "", "")
	}))
	defer server.Close()

	client, _, err := coderws.Dial(ctx, "ws"+server.URL[len("http"):], nil)
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`)))
	select {
	case err := <-errCh:
		require.ErrorContains(t, err, "missing id")
	case <-time.After(time.Second):
		t.Fatal("bridge did not reject initialize without a request id")
	}
	registry := svc.codexAppServerBridges
	registry.mu.RLock()
	_, registered := registry.bridges[78]
	registry.mu.RUnlock()
	require.False(t, registered)
}

func TestCodexAppServerBridgeUnnegotiatedConnectionFallsBackWithoutProof(t *testing.T) {
	registry := newCodexAppServerBridgeRegistry()
	bridge := &codexAppServerBridge{
		apiKeyID: 7, connectionID: "pending", closed: make(chan struct{}),
		pending: make(map[string]chan []byte),
	}
	require.NoError(t, registry.add(bridge))
	svc := &OpenAIGatewayService{
		codexAttestationStore: liveattestation.NewAppServerAttestationStore(time.Minute),
		codexAppServerBridges: registry,
	}
	ctx := context.Background()
	bound, err := svc.bindCodexAppServerAttestationContextForAPIKey(
		ctx, 7, &Account{ID: 99, Type: AccountTypeOAuth}, "", "",
	)
	require.NoError(t, err)
	require.NotNil(t, bound)
	_, hasProofContext := codexAttestationContextFrom(bound)
	require.False(t, hasProofContext)
	registry.remove(bridge)
}

func TestCodexAppServerBridgeRegistryDoesNotFallbackAcrossSessions(t *testing.T) {
	registry := newCodexAppServerBridgeRegistry()
	first := &codexAppServerBridge{
		apiKeyID: 7, connectionID: "first", sessionID: "session-a", closed: make(chan struct{}),
	}
	second := &codexAppServerBridge{
		apiKeyID: 7, connectionID: "second", sessionID: "session-b", closed: make(chan struct{}),
	}
	first.negotiated.Store(true)
	second.negotiated.Store(true)
	first.initialized.Store(true)
	second.initialized.Store(true)
	require.NoError(t, registry.add(first))
	require.NoError(t, registry.add(second))
	got, err := registry.find(7, "session-a", "")
	require.NoError(t, err)
	require.Same(t, first, got)
	got, err = registry.find(7, "session-missing", "")
	require.ErrorIs(t, err, ErrCodexAppServerBridgeAmbiguous)
	require.Nil(t, got)
	registry.remove(first)
	registry.remove(second)
}

func TestCodexAppServerBridgeRegistryIgnoresUnnegotiatedBridge(t *testing.T) {
	registry := newCodexAppServerBridgeRegistry()
	bridge := &codexAppServerBridge{
		apiKeyID: 7, connectionID: "pending", sessionID: "session-a", closed: make(chan struct{}),
	}
	require.NoError(t, registry.add(bridge))
	got, err := registry.find(7, "session-a", "")
	require.NoError(t, err)
	require.Nil(t, got)
	registry.remove(bridge)
}

func TestCodexAppServerBridgeRegistryWaitsForInitializedNotification(t *testing.T) {
	registry := newCodexAppServerBridgeRegistry()
	bridge := &codexAppServerBridge{
		apiKeyID: 7, connectionID: "pending-initialized", sessionID: "session-a", closed: make(chan struct{}),
	}
	bridge.negotiated.Store(true)
	require.NoError(t, registry.add(bridge))

	got, err := registry.find(7, "session-a", "")
	require.NoError(t, err)
	require.Nil(t, got)

	bridge.initialized.Store(true)
	got, err = registry.find(7, "session-a", "")
	require.NoError(t, err)
	require.Same(t, bridge, got)
	registry.remove(bridge)
}

func TestCodexAppServerBridgeRegistryDoesNotFallbackAcrossConflictingSingleSession(t *testing.T) {
	registry := newCodexAppServerBridgeRegistry()
	bridge := &codexAppServerBridge{
		apiKeyID: 7, connectionID: "first", sessionID: "session-a", closed: make(chan struct{}),
	}
	bridge.negotiated.Store(true)
	bridge.initialized.Store(true)
	require.NoError(t, registry.add(bridge))
	got, err := registry.find(7, "session-b", "")
	require.ErrorIs(t, err, ErrCodexAppServerBridgeAmbiguous)
	require.Nil(t, got)
	registry.remove(bridge)
}

func TestCodexAppServerBridgeAmbiguityFailsOpenForBusinessRequest(t *testing.T) {
	registry := newCodexAppServerBridgeRegistry()
	first := &codexAppServerBridge{
		apiKeyID: 7, connectionID: "first", sessionID: "session-a", closed: make(chan struct{}),
	}
	second := &codexAppServerBridge{
		apiKeyID: 7, connectionID: "second", sessionID: "session-b", closed: make(chan struct{}),
	}
	for _, bridge := range []*codexAppServerBridge{first, second} {
		bridge.negotiated.Store(true)
		bridge.initialized.Store(true)
		require.NoError(t, registry.add(bridge))
	}

	svc := &OpenAIGatewayService{
		codexAttestationStore: liveattestation.NewAppServerAttestationStore(time.Minute),
		codexAppServerBridges: registry,
	}
	ctx, err := svc.bindCodexAppServerAttestationContextForAPIKey(
		context.Background(), 7, &Account{ID: 99, Type: AccountTypeOAuth}, "session-missing", "",
	)
	require.NoError(t, err)
	require.NotNil(t, ctx)
	_, bound := codexAttestationContextFrom(ctx)
	require.False(t, bound, "ambiguous bridge must omit proof instead of failing the business request")
	registry.remove(first)
	registry.remove(second)
}

func TestCodexAppServerBridgeInvalidSessionHintFailsOpen(t *testing.T) {
	registry := newCodexAppServerBridgeRegistry()
	bridge := &codexAppServerBridge{
		apiKeyID: 7, connectionID: "unscoped", closed: make(chan struct{}),
		pending: make(map[string]chan []byte),
	}
	bridge.negotiated.Store(true)
	bridge.initialized.Store(true)
	require.NoError(t, registry.add(bridge))

	svc := &OpenAIGatewayService{
		codexAttestationStore: liveattestation.NewAppServerAttestationStore(time.Minute),
		codexAppServerBridges: registry,
	}
	ctx, err := svc.bindCodexAppServerAttestationContextForAPIKey(
		context.Background(), 7, &Account{ID: 99, Type: AccountTypeOAuth}, strings.Repeat("x", 300), "",
	)
	require.NoError(t, err)
	require.NotNil(t, ctx)
	_, bound := codexAttestationContextFrom(ctx)
	require.False(t, bound, "invalid session metadata must not turn an optional attestation sidecar into a request failure")
	registry.remove(bridge)
}

func TestCodexAppServerSessionHintsPreferCanonicalHyphenHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", nil)
	ctx.Request.Header.Set("session-id", "canonical-session")
	ctx.Request.Header.Set("session_id", "legacy-session")
	ctx.Request.Header.Set("thread-id", "canonical-thread")
	ctx.Request.Header.Set("thread_id", "legacy-thread")

	sessionID, threadID := codexAppServerSessionHints(nil, ctx)
	require.Equal(t, "canonical-session", sessionID)
	require.Equal(t, "canonical-thread", threadID)
}

func TestCodexAppServerSessionHintsAcceptConversationAliases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", nil)
	ctx.Request.Header.Set("session_id", "legacy-session")
	ctx.Request.Header.Set("conversation-id", "conversation-thread")

	sessionID, threadID := codexAppServerSessionHints(nil, ctx)
	require.Equal(t, "legacy-session", sessionID)
	require.Equal(t, "conversation-thread", threadID)
}

func TestCodexAppServerBridgeRegistryDoesNotUseScopedBridgeWithoutHints(t *testing.T) {
	registry := newCodexAppServerBridgeRegistry()
	bridge := &codexAppServerBridge{
		apiKeyID: 7, connectionID: "scoped", sessionID: "session-a", threadID: "thread-a", closed: make(chan struct{}),
	}
	bridge.negotiated.Store(true)
	bridge.initialized.Store(true)
	require.NoError(t, registry.add(bridge))

	got, err := registry.find(7, "", "")
	require.ErrorIs(t, err, ErrCodexAppServerBridgeAmbiguous)
	require.Nil(t, got)

	got, err = registry.find(7, "session-a", "thread-a")
	require.NoError(t, err)
	require.Same(t, bridge, got)
	registry.remove(bridge)
}

func TestCodexAppServerBridgeCloseRemovesPendingWithoutClosingWaiter(t *testing.T) {
	bridge := &codexAppServerBridge{
		closed:  make(chan struct{}),
		pending: make(map[string]chan []byte),
	}
	waiter := make(chan []byte, 1)
	bridge.pending["n:1"] = waiter

	bridge.close()

	select {
	case <-bridge.closed:
	default:
		t.Fatal("bridge close signal was not published")
	}
	bridge.pendingMu.Lock()
	_, stillPending := bridge.pending["n:1"]
	bridge.pendingMu.Unlock()
	require.False(t, stillPending)

	// The waiter remains safe for a concurrent readLoop send after close;
	// roundTrip observes bridge.closed and returns without needing a closed
	// waiter channel.
	select {
	case waiter <- []byte(`{"id":1}`):
	default:
		t.Fatal("pending waiter was unexpectedly closed or unavailable")
	}
}

func TestBridgeRequestIDRejectsNonIntegerNumber(t *testing.T) {
	_, err := bridgeRequestID(json.RawMessage(`1.0`))
	require.Error(t, err)
	_, err = bridgeRequestID(json.RawMessage(`1e0`))
	require.Error(t, err)
	got, err := bridgeRequestID(json.RawMessage(`1`))
	require.NoError(t, err)
	require.Equal(t, "n:1", got)
}

func stringValue(body []byte, path string) string {
	value, _ := objectValuePath(body, path).(string)
	return value
}

func numberValue(body []byte, path string) float64 {
	value, _ := objectValuePath(body, path).(float64)
	return value
}

func objectValue(body []byte) map[string]any {
	var object map[string]any
	_ = json.Unmarshal(body, &object)
	return object
}

func objectValuePath(body []byte, path string) any {
	var object map[string]any
	if json.Unmarshal(body, &object) != nil {
		return nil
	}
	parts := strings.Split(path, ".")
	var current any = object
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[part]
	}
	return current
}
