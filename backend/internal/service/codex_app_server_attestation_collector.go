package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/platform/liveattestation"
)

const (
	codexAttestationCollectorDefaultTTL        = 30 * time.Minute
	codexAttestationCollectorDefaultMaxRecords = 100
	codexAttestationCollectorTokenMaxBytes     = 128
	codexAttestationCollectorMaxSessions       = 256
)

var errCodexAttestationCollectorFull = errors.New("attestation collector session capacity reached")

// CodexAppServerAttestationCollectorStatus 描述采集器运行状态。
type CodexAppServerAttestationCollectorStatus struct {
	Running              bool       `json:"running"`
	SessionTTLSeconds    int        `json:"session_ttl_seconds"`
	MaxRecordsPerSession int        `json:"max_records_per_session"`
	ActiveSessions       int        `json:"active_sessions"`
	StartedAt            *time.Time `json:"started_at,omitempty"`
}

// CodexAppServerAttestationCollectorSession 是一次短期采集会话。
// token 只用于把一个 app-server bridge 绑定到采集会话，不是证明本身。
type CodexAppServerAttestationCollectorSession struct {
	Token       string    `json:"token"`
	ExpiresAt   time.Time `json:"expires_at"`
	BridgeQuery string    `json:"bridge_query"`
	HeaderName  string    `json:"header_name"`
}

// CodexAttestationHandshakeMetadata 保存 app-server WebSocket 握手中观察到的
// 非敏感传输元数据。它与协议帧分离，测试直接调用采集服务时可以省略。
type CodexAttestationHandshakeMetadata struct {
	HTTPProtocol string
	Transport    string
	UserAgent    string
	Originator   string
}

// CodexAppServerAttestationCaptureRecord 只保存证明的可审计摘要，不保存
// opaque proof 原文。这样管理员仍能确认真实客户端完成了协商与生成，
// 但采集器不会变成长期凭据存储。
type CodexAppServerAttestationCaptureRecord struct {
	ID                  string    `json:"id"`
	CapturedAt          time.Time `json:"captured_at"`
	Event               string    `json:"event"`
	APIKeyID            int64     `json:"api_key_id"`
	AccountID           int64     `json:"account_id"`
	ConnectionID        string    `json:"connection_id"`
	SessionID           string    `json:"session_id,omitempty"`
	ThreadID            string    `json:"thread_id,omitempty"`
	ClientName          string    `json:"client_name,omitempty"`
	ClientVersion       string    `json:"client_version,omitempty"`
	JSONRPCVersion      string    `json:"jsonrpc_version,omitempty"`
	CapabilityKeys      []string  `json:"capability_keys,omitempty"`
	RequestAttestation  bool      `json:"request_attestation"`
	InitializeID        string    `json:"initialize_id,omitempty"`
	FrameSHA256         string    `json:"frame_sha256,omitempty"`
	GenerateRequestID   uint64    `json:"generate_request_id,omitempty"`
	GenerateResponseID  string    `json:"generate_response_id,omitempty"`
	Status              string    `json:"status"`
	ProofLength         int       `json:"proof_length,omitempty"`
	ProofSHA256         string    `json:"proof_sha256,omitempty"`
	HandshakeProtocol   string    `json:"handshake_protocol,omitempty"`
	HandshakeTransport  string    `json:"handshake_transport,omitempty"`
	HandshakeUserAgent  string    `json:"handshake_user_agent,omitempty"`
	HandshakeOriginator string    `json:"handshake_originator,omitempty"`
}

type codexAttestationCollectorConnection struct {
	apiKeyID           int64
	key                liveattestation.SessionKey
	clientName         string
	clientVersion      string
	requestAttestation bool
	initializeID       string
	jsonRPCVersion     string
	capabilityKeys     []string
	frameSHA256        string
	handshake          CodexAttestationHandshakeMetadata
}

type codexAttestationCollectorSessionState struct {
	token       string
	expiresAt   time.Time
	records     []*CodexAppServerAttestationCaptureRecord
	connections map[string]codexAttestationCollectorConnection
}

// CodexAppServerAttestationCollector 采集真实 app-server 客户端的能力协商、
// generate 往返和证明摘要。它不生成、签名或复用任何设备证明。
type CodexAppServerAttestationCollector struct {
	mu        sync.Mutex
	running   bool
	startedAt *time.Time
	ttl       time.Duration
	max       int
	nextID    uint64
	sessions  map[string]*codexAttestationCollectorSessionState
}

// NewCodexAppServerAttestationCollector 创建默认短期内存采集器。
func NewCodexAppServerAttestationCollector() *CodexAppServerAttestationCollector {
	return &CodexAppServerAttestationCollector{
		ttl:      codexAttestationCollectorDefaultTTL,
		max:      codexAttestationCollectorDefaultMaxRecords,
		sessions: make(map[string]*codexAttestationCollectorSessionState),
	}
}

// Start 开启采集会话创建和记录。
func (c *CodexAppServerAttestationCollector) Start() CodexAppServerAttestationCollectorStatus {
	if c == nil {
		return CodexAppServerAttestationCollectorStatus{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.running = true
	c.startedAt = &now
	c.purgeLocked(now)
	return c.statusLocked()
}

// Stop 关闭采集并立即清除所有会话及关联摘要。
func (c *CodexAppServerAttestationCollector) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.running = false
	c.startedAt = nil
	c.sessions = make(map[string]*codexAttestationCollectorSessionState)
	c.mu.Unlock()
}

// Status 返回当前状态，并清理过期会话。
func (c *CodexAppServerAttestationCollector) Status() CodexAppServerAttestationCollectorStatus {
	if c == nil {
		return CodexAppServerAttestationCollectorStatus{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked(time.Now())
	return c.statusLocked()
}

// CreateSession 为一次真实 app-server 连接创建短期 token。
func (c *CodexAppServerAttestationCollector) CreateSession() (*CodexAppServerAttestationCollectorSession, error) {
	if c == nil {
		return nil, errors.New("attestation collector is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return nil, errors.New("attestation collector is not running")
	}
	now := time.Now()
	c.purgeLocked(now)
	if len(c.sessions) >= codexAttestationCollectorMaxSessions {
		return nil, errCodexAttestationCollectorFull
	}
	token, err := randomHexToken(24)
	if err != nil {
		return nil, err
	}
	state := &codexAttestationCollectorSessionState{
		token:       token,
		expiresAt:   now.Add(c.ttl),
		connections: make(map[string]codexAttestationCollectorConnection),
	}
	c.sessions[token] = state
	return &CodexAppServerAttestationCollectorSession{
		Token:       token,
		ExpiresAt:   state.expiresAt,
		BridgeQuery: "collector_token=" + token,
		HeaderName:  "X-Codex-Attestation-Collector-Token",
	}, nil
}

// DeleteSession 删除一条采集会话及其证明摘要。
func (c *CodexAppServerAttestationCollector) DeleteSession(token string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.sessions, strings.TrimSpace(token))
	c.mu.Unlock()
}

// ListCaptures 返回按时间倒序排列的采集摘要。
func (c *CodexAppServerAttestationCollector) ListCaptures(token string) ([]*CodexAppServerAttestationCaptureRecord, error) {
	if c == nil {
		return nil, errors.New("attestation collector is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.sessionLocked(token, time.Now())
	if err != nil {
		return nil, err
	}
	result := make([]*CodexAppServerAttestationCaptureRecord, 0, len(state.records))
	for _, record := range state.records {
		copyRecord := *record
		result = append(result, &copyRecord)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CapturedAt.Equal(result[j].CapturedAt) {
			return result[i].ID > result[j].ID
		}
		return result[i].CapturedAt.After(result[j].CapturedAt)
	})
	return result, nil
}

// RecordInitialize 保存 initialize 的能力协商摘要。无论客户端是否声明
// requestAttestation，都保留这一事件，便于确认真实客户端行为。
func (c *CodexAppServerAttestationCollector) RecordInitialize(token string, key liveattestation.SessionKey, raw []byte) error {
	return c.RecordInitializeForAPIKey(token, 0, key, raw)
}

// RecordInitializeForAPIKey 在管理员采集记录中同时保留 API Key 关联。
func (c *CodexAppServerAttestationCollector) RecordInitializeForAPIKey(token string, apiKeyID int64, key liveattestation.SessionKey, raw []byte) error {
	return c.RecordInitializeForAPIKeyWithMetadata(token, apiKeyID, key, raw, CodexAttestationHandshakeMetadata{})
}

// RecordInitializeForAPIKeyWithMetadata 同时记录 initialize 帧及安全的协议、握手摘要。
// 原始帧只计算哈希而不保留，因此可以关联能力名称和精确帧，又不会持久化客户端载荷。
func (c *CodexAppServerAttestationCollector) RecordInitializeForAPIKeyWithMetadata(token string, apiKeyID int64, key liveattestation.SessionKey, raw []byte, metadata CodexAttestationHandshakeMetadata) error {
	if c == nil {
		return errors.New("attestation collector is nil")
	}
	normalized, err := keyForCollector(key)
	if err != nil {
		return err
	}
	clientName, clientVersion, capability, initializeID, jsonRPCVersion, capabilityKeys, err := parseCollectorInitialize(raw)
	if err != nil {
		return err
	}
	metadata = normalizeCollectorHandshakeMetadata(metadata)
	frameDigest := sha256.Sum256(raw)
	frameSHA256 := hex.EncodeToString(frameDigest[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.sessionLocked(token, time.Now())
	if err != nil {
		return err
	}
	connectionKey := normalized.ConnectionID
	state.connections[connectionKey] = codexAttestationCollectorConnection{
		apiKeyID: apiKeyID,
		key:      normalized, clientName: clientName, clientVersion: clientVersion,
		requestAttestation: capability, initializeID: initializeID,
		jsonRPCVersion: jsonRPCVersion, capabilityKeys: append([]string(nil), capabilityKeys...),
		frameSHA256: frameSHA256, handshake: metadata,
	}
	c.appendRecordLocked(state, &CodexAppServerAttestationCaptureRecord{
		Event: "initialize", APIKeyID: apiKeyID, AccountID: 0,
		ConnectionID: normalized.ConnectionID, SessionID: normalized.SessionID,
		ThreadID: normalized.ThreadID, ClientName: clientName, ClientVersion: clientVersion,
		JSONRPCVersion: jsonRPCVersion, CapabilityKeys: append([]string(nil), capabilityKeys...),
		RequestAttestation: capability, InitializeID: initializeID, FrameSHA256: frameSHA256,
		HandshakeProtocol: metadata.HTTPProtocol, HandshakeTransport: metadata.Transport,
		HandshakeUserAgent: metadata.UserAgent, HandshakeOriginator: metadata.Originator,
		Status: map[bool]string{true: "negotiated", false: "not_requested"}[capability],
	})
	return nil
}

// RecordGenerateResponse 记录成功或 malformed 的 attestation/generate 响应。
// 成功时仅写入 headerValue 长度和 SHA-256，不把 headerValue 原文落入内存记录。
func (c *CodexAppServerAttestationCollector) RecordGenerateResponse(token string, key liveattestation.SessionKey, requestID uint64, raw []byte) error {
	return c.RecordGenerateResponseForAPIKey(token, 0, key, requestID, raw)
}

// RecordGenerateResponseForAPIKey 记录带 API Key 关联的 generate 响应摘要。
func (c *CodexAppServerAttestationCollector) RecordGenerateResponseForAPIKey(token string, apiKeyID int64, key liveattestation.SessionKey, requestID uint64, raw []byte) error {
	if c == nil {
		return errors.New("attestation collector is nil")
	}
	normalized, err := keyForCollector(key)
	if err != nil {
		return err
	}
	_, proof, parseErr := liveattestation.ParseAttestationGenerateResponse(raw)
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.sessionLocked(token, time.Now())
	if err != nil {
		return err
	}
	connection, ok := state.connections[normalized.ConnectionID]
	if !ok || (apiKeyID > 0 && connection.apiKeyID > 0 && connection.apiKeyID != apiKeyID) {
		return errors.New("attestation collector connection was not initialized")
	}
	c.bindCollectorConnectionAccountLocked(state, normalized.ConnectionID, normalized.AccountID)
	record := &CodexAppServerAttestationCaptureRecord{
		Event: "generate", APIKeyID: apiKeyID, AccountID: normalized.AccountID, ConnectionID: normalized.ConnectionID,
		SessionID: normalized.SessionID, ThreadID: normalized.ThreadID,
		ClientName: connection.clientName, ClientVersion: connection.clientVersion,
		JSONRPCVersion: connection.jsonRPCVersion, CapabilityKeys: append([]string(nil), connection.capabilityKeys...),
		RequestAttestation: connection.requestAttestation, InitializeID: connection.initializeID,
		GenerateRequestID: requestID, GenerateResponseID: rawJSONRPCID(raw),
		HandshakeProtocol: connection.handshake.HTTPProtocol, HandshakeTransport: connection.handshake.Transport,
		HandshakeUserAgent: connection.handshake.UserAgent, HandshakeOriginator: connection.handshake.Originator,
	}
	frameDigest := sha256.Sum256(raw)
	record.FrameSHA256 = hex.EncodeToString(frameDigest[:])
	if parseErr != nil {
		record.Status = "malformed_response"
	} else {
		record.Status = "success"
		record.ProofLength = len(proof)
		digest := sha256.Sum256([]byte(proof))
		record.ProofSHA256 = hex.EncodeToString(digest[:])
	}
	c.appendRecordLocked(state, record)
	return nil
}

// RecordGenerateFailure 记录 app-server 官方失败状态，不保存任何 proof。
func (c *CodexAppServerAttestationCollector) RecordGenerateFailure(token string, key liveattestation.SessionKey, requestID uint64, status liveattestation.AttestationStatus) error {
	return c.RecordGenerateFailureForAPIKey(token, 0, key, requestID, status)
}

// RecordGenerateFailureForAPIKey 记录带 API Key 关联的失败摘要。
func (c *CodexAppServerAttestationCollector) RecordGenerateFailureForAPIKey(token string, apiKeyID int64, key liveattestation.SessionKey, requestID uint64, status liveattestation.AttestationStatus) error {
	if c == nil {
		return errors.New("attestation collector is nil")
	}
	normalized, err := keyForCollector(key)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.sessionLocked(token, time.Now())
	if err != nil {
		return err
	}
	connection, ok := state.connections[normalized.ConnectionID]
	if !ok || (apiKeyID > 0 && connection.apiKeyID > 0 && connection.apiKeyID != apiKeyID) {
		return errors.New("attestation collector connection was not initialized")
	}
	c.bindCollectorConnectionAccountLocked(state, normalized.ConnectionID, normalized.AccountID)
	c.appendRecordLocked(state, &CodexAppServerAttestationCaptureRecord{
		Event: "generate", APIKeyID: apiKeyID, AccountID: normalized.AccountID, ConnectionID: normalized.ConnectionID,
		SessionID: normalized.SessionID, ThreadID: normalized.ThreadID,
		ClientName: connection.clientName, ClientVersion: connection.clientVersion,
		JSONRPCVersion: connection.jsonRPCVersion, CapabilityKeys: append([]string(nil), connection.capabilityKeys...),
		RequestAttestation: connection.requestAttestation, InitializeID: connection.initializeID,
		GenerateRequestID: requestID, Status: collectorStatusName(status),
		HandshakeProtocol: connection.handshake.HTTPProtocol, HandshakeTransport: connection.handshake.Transport,
		HandshakeUserAgent: connection.handshake.UserAgent, HandshakeOriginator: connection.handshake.Originator,
	})
	return nil
}

// bindCollectorConnectionAccountLocked 在首次观察到账号作用域的 generate 请求后，
// 回填该连接选中的账号。initialize 帧早于账号选择到达，因此初始记录的 account_id=0；
// 在此建立关联可补全审计链路，同时不保存额外客户端载荷。
func (c *CodexAppServerAttestationCollector) bindCollectorConnectionAccountLocked(state *codexAttestationCollectorSessionState, connectionID string, accountID int64) {
	if state == nil || accountID <= 0 {
		return
	}
	for _, record := range state.records {
		if record != nil && record.Event == "initialize" && record.ConnectionID == connectionID && record.AccountID == 0 {
			record.AccountID = accountID
		}
	}
}

func (c *CodexAppServerAttestationCollector) statusLocked() CodexAppServerAttestationCollectorStatus {
	return CodexAppServerAttestationCollectorStatus{
		Running: c.running, SessionTTLSeconds: int(c.ttl.Seconds()),
		MaxRecordsPerSession: c.max, ActiveSessions: len(c.sessions), StartedAt: c.startedAt,
	}
}

func (c *CodexAppServerAttestationCollector) sessionLocked(token string, now time.Time) (*codexAttestationCollectorSessionState, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > codexAttestationCollectorTokenMaxBytes {
		return nil, errors.New("invalid attestation collector token")
	}
	c.purgeLocked(now)
	state := c.sessions[token]
	if state == nil {
		return nil, errors.New("attestation collector session not found or expired")
	}
	return state, nil
}

func (c *CodexAppServerAttestationCollector) purgeLocked(now time.Time) {
	for token, state := range c.sessions {
		if !now.Before(state.expiresAt) {
			delete(c.sessions, token)
		}
	}
}

func (c *CodexAppServerAttestationCollector) appendRecordLocked(state *codexAttestationCollectorSessionState, record *CodexAppServerAttestationCaptureRecord) {
	c.nextID++
	record.ID = fmt.Sprintf("%d", c.nextID)
	record.CapturedAt = time.Now()
	state.records = append(state.records, record)
	if len(state.records) > c.max {
		state.records = state.records[len(state.records)-c.max:]
	}
}

func keyForCollector(key liveattestation.SessionKey) (liveattestation.SessionKey, error) {
	return key.NormalizedForCollector()
}

func parseCollectorInitialize(raw []byte) (string, string, bool, string, string, []string, error) {
	capability, capabilityErr := liveattestation.ParseInitializeRequest(raw)
	if capabilityErr != nil {
		return "", "", false, "", "", nil, capabilityErr
	}
	var message struct {
		ID      json.RawMessage `json:"id"`
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  struct {
			ClientInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return "", "", false, "", "", nil, fmt.Errorf("decode initialize: %w", err)
	}
	if message.JSONRPC != "" && message.JSONRPC != "2.0" {
		return "", "", false, "", "", nil, errors.New("unsupported JSON-RPC version")
	}
	if message.Method != "initialize" {
		return "", "", false, "", "", nil, errors.New("collector requires initialize")
	}
	initializeID := strings.TrimSpace(string(message.ID))
	capabilityKeys := make([]string, 0, len(message.Params.Capabilities))
	requestAttestation := capability
	for name, value := range message.Params.Capabilities {
		capabilityKeys = append(capabilityKeys, name)
		if name == "requestAttestation" {
			var declared bool
			if err := json.Unmarshal(value, &declared); err != nil {
				return "", "", false, "", "", nil, fmt.Errorf("decode requestAttestation capability: %w", err)
			}
			requestAttestation = declared
		}
	}
	sort.Strings(capabilityKeys)
	return message.Params.ClientInfo.Name, message.Params.ClientInfo.Version,
		requestAttestation, initializeID, message.JSONRPC, capabilityKeys, nil
}

func normalizeCollectorHandshakeMetadata(metadata CodexAttestationHandshakeMetadata) CodexAttestationHandshakeMetadata {
	metadata.HTTPProtocol = normalizeCollectorMetadataValue(metadata.HTTPProtocol)
	metadata.Transport = normalizeCollectorMetadataValue(metadata.Transport)
	metadata.UserAgent = normalizeCollectorMetadataValue(metadata.UserAgent)
	metadata.Originator = normalizeCollectorMetadataValue(metadata.Originator)
	return metadata
}

func normalizeCollectorMetadataValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 {
		return ""
	}
	for _, r := range value {
		if r == 0 || r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

func rawJSONRPCID(raw []byte) string {
	var message struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(raw, &message) != nil || len(message.ID) == 0 || bytes.Equal(bytes.TrimSpace(message.ID), []byte("null")) {
		return ""
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(message.ID))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func collectorStatusName(status liveattestation.AttestationStatus) string {
	switch status {
	case liveattestation.AttestationStatusTimeout:
		return "timeout"
	case liveattestation.AttestationStatusRequestFailed:
		return "request_failed"
	case liveattestation.AttestationStatusRequestCanceled:
		return "request_canceled"
	case liveattestation.AttestationStatusMalformedResponse:
		return "malformed_response"
	default:
		return "unknown_failure"
	}
}
