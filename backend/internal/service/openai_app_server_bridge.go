package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/platform/liveattestation"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

var (
	ErrCodexAppServerBridgeUnavailable   = errors.New("codex app-server bridge is unavailable")
	ErrCodexAppServerBridgeNotNegotiated = errors.New("codex app-server bridge did not negotiate attestation")
	ErrCodexAppServerBridgeAmbiguous     = errors.New("multiple codex app-server bridges match the request")
	ErrCodexAppServerBridgeLimit         = errors.New("codex app-server bridge limit reached")
)

const (
	codexAppServerBridgeFirstMessageTimeout = 10 * time.Second
	codexAppServerBridgeMessageTimeout      = 5 * time.Second
	codexAppServerBridgeAttestationTimeout  = 2 * time.Second
	codexAppServerBridgeIdleTimeout         = 5 * time.Minute
	codexAppServerBridgeReadLimit           = 1 << 20
	codexAppServerBridgePerAPIKeyLimit      = 4
)

type codexAppServerBridge struct {
	apiKeyID       int64
	connectionID   string
	sessionID      string
	threadID       string
	collectorToken string
	handshake      CodexAttestationHandshakeMetadata
	conn           *coderws.Conn
	registry       *codexAppServerBridgeRegistry
	// boundAccountID prevents a single client proof channel from being reused
	// with a different OAuth account selected behind the same API key.  A
	// mismatching account continues without attestation rather than failing the
	// business request; the proof is never sent with the wrong credentials.
	boundAccountID atomic.Int64

	lockInit  sync.Once
	writeMu   *contextMutex
	roundMu   *contextMutex
	pendingMu sync.Mutex
	pending   map[string]chan []byte

	initializeRaw []byte
	negotiated    atomic.Bool
	initialized   atomic.Bool
	closed        chan struct{}
	closeOnce     sync.Once
}

// 锁按需初始化，兼容未建立网络连接的协议测试及零值构造路径。
func (b *codexAppServerBridge) initLocks() {
	b.lockInit.Do(func() {
		b.writeMu = newContextMutex()
		b.roundMu = newContextMutex()
	})
}

type codexAppServerBridgeRegistry struct {
	mu      sync.RWMutex
	bridges map[int64]map[string]*codexAppServerBridge
}

func newCodexAppServerBridgeRegistry() *codexAppServerBridgeRegistry {
	return &codexAppServerBridgeRegistry{bridges: make(map[int64]map[string]*codexAppServerBridge)}
}

func (r *codexAppServerBridgeRegistry) add(bridge *codexAppServerBridge) error {
	if bridge == nil {
		return ErrCodexAppServerBridgeUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bridges[bridge.apiKeyID] == nil {
		r.bridges[bridge.apiKeyID] = make(map[string]*codexAppServerBridge)
	}
	if len(r.bridges[bridge.apiKeyID]) >= codexAppServerBridgePerAPIKeyLimit {
		return ErrCodexAppServerBridgeLimit
	}
	bridge.registry = r
	r.bridges[bridge.apiKeyID][bridge.connectionID] = bridge
	r.refreshCapabilityLocked()
	return nil
}

func (r *codexAppServerBridgeRegistry) remove(bridge *codexAppServerBridge) {
	r.mu.Lock()
	if byID := r.bridges[bridge.apiKeyID]; byID != nil {
		delete(byID, bridge.connectionID)
		if len(byID) == 0 {
			delete(r.bridges, bridge.apiKeyID)
		}
	}
	r.refreshCapabilityLocked()
	r.mu.Unlock()
}

func (r *codexAppServerBridgeRegistry) refreshCapabilityLocked() {
	for _, byID := range r.bridges {
		for _, bridge := range byID {
			if bridge.negotiated.Load() && bridge.initialized.Load() && !bridge.closedState() {
				liveattestation.SetAppServerAttestationTransport(true)
				return
			}
		}
	}
	liveattestation.SetAppServerAttestationTransport(false)
}

func (r *codexAppServerBridgeRegistry) find(apiKeyID int64, sessionID, threadID string) (*codexAppServerBridge, error) {
	if r == nil || apiKeyID <= 0 {
		return nil, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	threadID = strings.TrimSpace(threadID)
	r.mu.RLock()
	defer r.mu.RUnlock()
	byID := r.bridges[apiKeyID]
	if len(byID) == 0 {
		return nil, nil
	}
	activeCount := 0
	var only *codexAppServerBridge
	var exact *codexAppServerBridge
	for _, bridge := range byID {
		// A bridge is not usable until initialize has completed and the client
		// has explicitly opted into requestAttestation.  Before that point the
		// official app-server behavior is to omit x-oai-attestation, not to fail
		// unrelated Responses/Live requests because a control socket happens to
		// be connected.
		if bridge.closedState() || !bridge.negotiated.Load() || !bridge.initialized.Load() {
			continue
		}
		activeCount++
		only = bridge
		// 桥已声明的作用域必须完整匹配；请求缺省字段不应充当通配符。
		if bridge.sessionID != "" && bridge.sessionID != sessionID {
			continue
		}
		if bridge.threadID != "" && bridge.threadID != threadID {
			continue
		}
		if (sessionID != "" && bridge.sessionID == sessionID) || (threadID != "" && bridge.threadID == threadID) {
			if exact != nil {
				return nil, ErrCodexAppServerBridgeAmbiguous
			}
			exact = bridge
		}
	}
	if activeCount == 0 {
		return nil, nil
	}
	if exact != nil {
		return exact, nil
	}
	if activeCount == 1 && only != nil {
		// An unscoped bridge can serve the only active client connection. A
		// bridge carrying an explicit session/thread hint requires the request
		// to provide the same hint; an unscoped request must not inherit a
		// scoped proof merely because it is the only connection on the API key.
		if (only.sessionID == "" || sessionID == only.sessionID) &&
			(only.threadID == "" || threadID == only.threadID) &&
			(sessionID != "" || only.sessionID == "") &&
			(threadID != "" || only.threadID == "") {
			return only, nil
		}
	}
	return nil, ErrCodexAppServerBridgeAmbiguous
}

func (b *codexAppServerBridge) closedState() bool {
	select {
	case <-b.closed:
		return true
	default:
		return false
	}
}

func (b *codexAppServerBridge) close() {
	b.closeOnce.Do(func() {
		close(b.closed)
		b.pendingMu.Lock()
		for id := range b.pending {
			delete(b.pending, id)
			// Do not close waiter here. readLoop may have taken the channel
			// pointer just before this lock and could otherwise send to a
			// closed channel. roundTrip also selects on b.closed, so removing
			// the waiter is sufficient to wake/cancel the caller without a
			// send-on-closed-channel race.
		}
		b.pendingMu.Unlock()
		if b.conn != nil {
			_ = b.conn.CloseNow()
		}
	})
}

func (b *codexAppServerBridge) write(ctx context.Context, payload []byte) error {
	if b == nil || b.conn == nil || b.closedState() {
		return ErrCodexAppServerBridgeUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	writeCtx, cancel := context.WithTimeout(ctx, codexAppServerBridgeMessageTimeout)
	defer cancel()
	b.initLocks()
	if err := writeCtx.Err(); err != nil {
		return err
	}
	// 写锁等待也计入当前 RPC 的剩余预算，避免绕开证明请求的总超时。
	if err := b.writeMu.Lock(writeCtx); err != nil {
		return err
	}
	defer b.writeMu.Unlock()
	if err := writeCtx.Err(); err != nil {
		return err
	}
	return b.conn.Write(writeCtx, coderws.MessageText, payload)
}

func (b *codexAppServerBridge) roundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	var message liveattestation.JSONRPCMessage
	if err := json.Unmarshal(payload, &message); err != nil || len(message.ID) == 0 {
		return nil, errors.New("invalid app-server request id")
	}
	id, err := bridgeRequestID(message.ID)
	if err != nil {
		return nil, err
	}
	waiter := make(chan []byte, 1)
	b.pendingMu.Lock()
	if b.closedState() {
		b.pendingMu.Unlock()
		return nil, ErrCodexAppServerBridgeUnavailable
	}
	b.pending[id] = waiter
	b.pendingMu.Unlock()
	defer func() {
		b.pendingMu.Lock()
		delete(b.pending, id)
		b.pendingMu.Unlock()
	}()
	if err := b.write(ctx, payload); err != nil {
		return nil, err
	}
	select {
	case response, ok := <-waiter:
		if !ok {
			return nil, ErrCodexAppServerBridgeUnavailable
		}
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.closed:
		return nil, ErrCodexAppServerBridgeUnavailable
	}
}

func (b *codexAppServerBridge) readLoop(ctx context.Context) error {
	for {
		readCtx, cancel := context.WithTimeout(ctx, codexAppServerBridgeIdleTimeout)
		_, payload, err := b.conn.Read(readCtx)
		cancel()
		if err != nil {
			return err
		}
		var message liveattestation.JSONRPCMessage
		if err := json.Unmarshal(payload, &message); err != nil ||
			(message.JSONRPC != "" && message.JSONRPC != "2.0") {
			return errors.New("invalid app-server JSON-RPC message")
		}
		// The official app-server sends an `initialized` notification after it
		// receives the initialize response.  Attestation is only eligible after
		// that lifecycle point; a capability-bearing but half-open connection
		// must not be selected for an upstream request.
		if message.Method == "initialized" && len(message.ID) == 0 {
			b.initialized.Store(true)
			if registry := b.registry; registry != nil {
				registry.mu.Lock()
				registry.refreshCapabilityLocked()
				registry.mu.Unlock()
			}
			continue
		}
		if len(message.ID) == 0 {
			continue
		}
		id, err := bridgeRequestID(message.ID)
		if err != nil {
			continue
		}
		b.pendingMu.Lock()
		waiter := b.pending[id]
		b.pendingMu.Unlock()
		if waiter != nil {
			select {
			case waiter <- append([]byte(nil), payload...):
			default:
			}
			continue
		}
		// 客户端不应向 bridge 发起除 initialize 外的请求；未知请求返回标准错误，
		// 避免把 app-server 控制面误当成上游业务协议。
		if message.Method != "" {
			_ = b.write(ctx, buildJSONRPCError(message.ID, -32601, "method not supported by ConcordRoute app-server bridge"))
		}
	}
}

// ServeCodexAppServerBridge 承载一条真实的 app-server JSON-RPC WebSocket。
// 首帧必须是 initialize，后续 attestation/generate 响应按 JSON-RPC id 投递给
// 等待中的上游请求；普通 Responses/WS 数据不会混入此连接。
func (s *OpenAIGatewayService) ServeCodexAppServerBridge(ctx context.Context, apiKeyID int64, conn *coderws.Conn, sessionID, threadID string, collectorTokens ...string) error {
	collectorToken := ""
	if len(collectorTokens) > 0 {
		collectorToken = strings.TrimSpace(collectorTokens[0])
	}
	return s.serveCodexAppServerBridge(ctx, apiKeyID, conn, sessionID, threadID, CodexAttestationHandshakeMetadata{}, collectorToken)
}

// serveCodexAppServerBridge 是 HTTP handler 使用的传输感知实现。公开包装函数继续兼容
// 无握手元数据的进程内调用方和测试。
func (s *OpenAIGatewayService) serveCodexAppServerBridge(ctx context.Context, apiKeyID int64, conn *coderws.Conn, sessionID, threadID string, handshake CodexAttestationHandshakeMetadata, collectorToken string) error {
	if s == nil || s.codexAppServerBridges == nil || apiKeyID <= 0 || conn == nil {
		return ErrCodexAppServerBridgeUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	bridge := &codexAppServerBridge{
		apiKeyID:       apiKeyID,
		connectionID:   uuid.NewString(),
		sessionID:      strings.TrimSpace(sessionID),
		threadID:       strings.TrimSpace(threadID),
		collectorToken: collectorToken,
		handshake:      normalizeCollectorHandshakeMetadata(handshake),
		conn:           conn,
		pending:        make(map[string]chan []byte),
		closed:         make(chan struct{}),
	}
	defer func() {
		bridge.close()
		s.codexAppServerBridges.remove(bridge)
	}()
	conn.SetReadLimit(codexAppServerBridgeReadLimit)
	firstCtx, cancel := context.WithTimeout(ctx, codexAppServerBridgeFirstMessageTimeout)
	_, first, err := conn.Read(firstCtx)
	cancel()
	if err != nil {
		return err
	}
	capability, err := liveattestation.ParseInitializeRequest(first)
	if err != nil || gjson.GetBytes(first, "method").String() != "initialize" {
		return errors.New("app-server bridge requires initialize as the first message")
	}
	// `initialize` is a JSON-RPC request, not a notification.  Require a
	// correlatable request id before registering the bridge; otherwise the
	// client could receive no initialize response while the connection still
	// consumes an attestation slot.
	initializeID := gjson.GetBytes(first, "id")
	if !initializeID.Exists() || initializeID.Type == gjson.Null {
		return errors.New("app-server initialize request is missing id")
	}
	if _, err := bridgeRequestID(json.RawMessage(initializeID.Raw)); err != nil {
		return fmt.Errorf("invalid app-server initialize request id: %w", err)
	}
	bridge.initializeRaw = append([]byte(nil), first...)
	bridge.negotiated.Store(capability)
	if collectorToken != "" && s.codexAttestationCollector != nil {
		// initialize 阶段尚未选定 OAuth 账号，使用 API Key ID 作为临时
		// 连接命名空间；后续 generate 记录会替换为实际账号 ID。
		key := liveattestation.SessionKey{AccountID: apiKeyID, ConnectionID: bridge.connectionID, SessionID: bridge.sessionID, ThreadID: bridge.threadID}
		// initialize 阶段尚未选定 OAuth 账号，先记录能力与连接；业务请求
		// 到达后会追加带真实账号 ID 的 generate 记录。
		_ = s.codexAttestationCollector.RecordInitializeForAPIKeyWithMetadata(collectorToken, apiKeyID, key, first, bridge.handshake)
	}
	// Register only after the first frame has been validated. A client that
	// never sends initialize must not consume the per-API-key bridge quota.
	// Non-capable clients remain ordinary control connections and do not need
	// registry state because they can never be selected for attestation.
	if capability {
		if err := s.codexAppServerBridges.add(bridge); err != nil {
			return err
		}
	}
	if id := gjson.GetBytes(first, "id"); id.Exists() {
		if err := bridge.write(ctx, buildJSONRPCResult(json.RawMessage(id.Raw), map[string]any{
			// These are the required v1 InitializeResponse fields from the
			// official app-server protocol.  Capabilities are client-declared;
			// do not fabricate requestAttestation=true in the server response.
			"userAgent":      "ConcordRoute app-server bridge",
			"codexHome":      "/",
			"platformFamily": "unix",
			"platformOs":     "linux",
		})); err != nil {
			return err
		}
	}
	return bridge.readLoop(ctx)
}

// bindCodexAppServerAttestationContext 在账号已选定后把请求绑定到活动 bridge，
// 并按需执行真实 attestation/generate 往返。没有 bridge 时保持旧的无证明行为。
func (s *OpenAIGatewayService) bindCodexAppServerAttestationContext(ctx context.Context, c *gin.Context, account *Account, sessionID, threadID string) (context.Context, error) {
	return s.bindCodexAppServerAttestationContextForAPIKey(ctx, getAPIKeyIDFromContext(c), account, sessionID, threadID)
}

func (s *OpenAIGatewayService) bindCodexAppServerAttestationContextForAPIKey(ctx context.Context, apiKeyID int64, account *Account, sessionID, threadID string) (context.Context, error) {
	if s == nil || !accountSupportsCodexAppServerAttestation(account) || s.codexAppServerBridges == nil {
		return ctx, nil
	}
	bridge, err := s.codexAppServerBridges.find(apiKeyID, sessionID, threadID)
	if err != nil {
		// Attestation is an optional app-server sidecar.  If more than one
		// initialized bridge could match, selecting either one could attach a
		// proof from the wrong desktop session.  Likewise, any transient lookup
		// error must fail open: omit x-oai-attestation and let the business
		// request proceed, matching Codex's provider semantics.
		return ctx, nil
	}
	if bridge == nil {
		return ctx, nil
	}
	if !bridge.negotiated.Load() {
		// A connected app-server that did not opt into requestAttestation is
		// still a valid control connection.  Official Codex omits
		// x-oai-attestation in this case and proceeds with the request.
		return ctx, nil
	}
	if account.ID <= 0 {
		return ctx, nil
	}
	if bound := bridge.boundAccountID.Load(); bound != 0 && bound != account.ID {
		return ctx, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 两秒预算覆盖排队和完整 RPC；保留原始业务 ctx，避免成功后附带已取消的子上下文。
	roundCtx, cancel := context.WithTimeout(ctx, codexAppServerBridgeAttestationTimeout)
	defer cancel()
	bridge.initLocks()
	if roundCtx.Err() != nil {
		return ctx, nil
	}
	if err := bridge.roundMu.Lock(roundCtx); err != nil {
		return ctx, nil
	}
	defer bridge.roundMu.Unlock()
	if roundCtx.Err() != nil || bridge.closedState() {
		return ctx, nil
	}
	// 排队期间其它请求可能完成账号绑定，拿锁后必须再次校验。
	if bound := bridge.boundAccountID.Load(); bound != 0 && bound != account.ID {
		return ctx, nil
	}
	if bridge.boundAccountID.Load() == 0 {
		bridge.boundAccountID.Store(account.ID)
	}
	key := liveattestation.SessionKey{AccountID: account.ID, ConnectionID: bridge.connectionID, SessionID: sessionID, ThreadID: threadID}
	adapter, err := s.NewCodexAppServerAttestationAdapter(key)
	if err != nil {
		return ctx, nil
	}
	if _, err := adapter.ObserveInitialize(bridge.initializeRaw); err != nil {
		return ctx, nil
	}
	adapter.SetCollector(s.codexAttestationCollector, bridge.collectorToken, bridge.apiKeyID)
	if _, err := adapter.GenerateForRequestWithTimeout(roundCtx, bridge.roundTrip, codexAppServerBridgeAttestationTimeout); err != nil {
		return ctx, nil
	}
	requestCtx, err := adapter.RequestContext()
	if err != nil {
		return ctx, nil
	}
	return WithCodexAttestationRequestContext(ctx, requestCtx), nil
}

func bridgeRequestID(raw json.RawMessage) (string, error) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	switch v := value.(type) {
	case string:
		if v == "" {
			return "", errors.New("empty JSON-RPC id")
		}
		return "s:" + v, nil
	case json.Number:
		// Codex app-server RequestId is an integer or string.  Do not accept
		// JSON floating-point spellings such as 1.0 or 1e0; a permissive key
		// would let a malformed response bypass exact request correlation.
		integer, err := strconv.ParseInt(v.String(), 10, 64)
		if err != nil {
			return "", errors.New("JSON-RPC id must be an integer or string")
		}
		return "n:" + strconv.FormatInt(integer, 10), nil
	default:
		return "", errors.New("JSON-RPC id must be string or number")
	}
}

func buildJSONRPCResult(id json.RawMessage, result any) []byte {
	payload, err := json.Marshal(struct {
		ID     json.RawMessage `json:"id"`
		Result any             `json:"result"`
	}{ID: id, Result: result})
	if err != nil {
		return []byte(`{"id":null,"result":{}}`)
	}
	return payload
}

func buildJSONRPCError(id json.RawMessage, code int, message string) []byte {
	payload, err := json.Marshal(struct {
		ID    json.RawMessage `json:"id"`
		Error map[string]any  `json:"error"`
	}{ID: id, Error: map[string]any{"code": code, "message": message}})
	if err != nil {
		return []byte(`{"id":null,"error":{"code":-32600,"message":"invalid request"}}`)
	}
	return payload
}

// HandleCodexAppServerBridge 是 HTTP handler 使用的窄入口。
func (s *OpenAIGatewayService) HandleCodexAppServerBridge(c *gin.Context, apiKeyID int64) {
	if c == nil || c.Request == nil {
		return
	}
	if !isCodexAppServerWSUpgradeRequest(c.Request) {
		c.JSON(http.StatusUpgradeRequired, gin.H{"error": gin.H{"message": "WebSocket upgrade required"}})
		return
	}
	conn, err := coderws.Accept(c.Writer, c.Request, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	sessionID, threadID := codexAppServerHeaderHints(c)
	collectorToken := codexAppServerCollectorToken(c)
	handshake := CodexAttestationHandshakeMetadata{
		HTTPProtocol: c.Request.Proto,
		Transport:    "websocket",
		UserAgent:    c.GetHeader("User-Agent"),
		Originator:   c.GetHeader("originator"),
	}
	if err := s.serveCodexAppServerBridge(c.Request.Context(), apiKeyID, conn, sessionID, threadID, handshake, collectorToken); err != nil {
		_ = conn.Close(coderws.StatusPolicyViolation, err.Error())
	}
}

func codexAppServerCollectorToken(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	for _, name := range []string{"X-Codex-Attestation-Collector-Token", "X-Attestation-Collector-Token"} {
		if value := strings.TrimSpace(c.GetHeader(name)); value != "" {
			return value
		}
	}
	for _, name := range []string{"collector_token", "attestation_capture", "capture_token"} {
		if value := strings.TrimSpace(c.Query(name)); value != "" {
			return value
		}
	}
	return ""
}

// codexAppServerHeaderHints accepts the canonical hyphenated Codex headers
// and the legacy underscore aliases used by older clients.  Header names are
// case-insensitive at the HTTP layer; GetHeader handles that normalization.
// The canonical form wins when both spellings are present so a stale legacy
// value cannot override the current client session.
func codexAppServerHeaderHints(c *gin.Context) (string, string) {
	if c == nil {
		return "", ""
	}
	firstHeader := func(names ...string) string {
		for _, name := range names {
			if value := strings.TrimSpace(c.GetHeader(name)); value != "" {
				return value
			}
		}
		return ""
	}
	sessionID := firstHeader("session-id", "session_id", "x-session-id")
	threadID := firstHeader("thread-id", "thread_id", "x-thread-id")
	if threadID == "" {
		threadID = firstHeader("conversation-id", "conversation_id", "x-conversation-id")
	}
	return sessionID, threadID
}

func codexAppServerSessionHints(body []byte, c *gin.Context) (string, string) {
	var sessionID, threadID string
	if c != nil {
		sessionID, threadID = codexAppServerHeaderHints(c)
	}
	if sessionID == "" {
		for _, path := range []string{"session_id", "session-id", "client_metadata.session_id", "client_metadata.session-id"} {
			if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); value != "" {
				sessionID = value
				break
			}
		}
	}
	if threadID == "" {
		for _, path := range []string{"thread_id", "thread-id", "client_metadata.thread_id", "client_metadata.thread-id"} {
			if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); value != "" {
				threadID = value
				break
			}
		}
	}
	if threadID == "" {
		for _, path := range []string{"conversation_id", "conversation-id", "client_metadata.conversation_id", "client_metadata.conversation-id"} {
			if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); value != "" {
				threadID = value
				break
			}
		}
	}
	return sessionID, threadID
}

func (s *OpenAIGatewayService) bindCodexAppServerForBody(ctx context.Context, c *gin.Context, account *Account, body []byte) (context.Context, error) {
	sessionID, threadID := codexAppServerSessionHints(body, c)
	return s.bindCodexAppServerAttestationContext(ctx, c, account, sessionID, threadID)
}

func isCodexAppServerWSUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}
