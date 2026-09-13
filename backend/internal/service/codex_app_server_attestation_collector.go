package service

import (
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

// CodexAppServerAttestationCaptureRecord 只保存证明的可审计摘要，不保存
// opaque proof 原文。这样管理员仍能确认真实客户端完成了协商与生成，
// 但采集器不会变成长期凭据存储。
type CodexAppServerAttestationCaptureRecord struct {
	ID                 string    `json:"id"`
	CapturedAt         time.Time `json:"captured_at"`
	Event              string    `json:"event"`
	APIKeyID           int64     `json:"api_key_id"`
	AccountID          int64     `json:"account_id"`
	ConnectionID       string    `json:"connection_id"`
	SessionID          string    `json:"session_id,omitempty"`
	ThreadID           string    `json:"thread_id,omitempty"`
	ClientName         string    `json:"client_name,omitempty"`
	ClientVersion      string    `json:"client_version,omitempty"`
	RequestAttestation bool      `json:"request_attestation"`
	InitializeID       string    `json:"initialize_id,omitempty"`
	GenerateRequestID  uint64    `json:"generate_request_id,omitempty"`
	Status             string    `json:"status"`
	ProofLength        int       `json:"proof_length,omitempty"`
	ProofSHA256        string    `json:"proof_sha256,omitempty"`
}

type codexAttestationCollectorConnection struct {
	apiKeyID           int64
	key                liveattestation.SessionKey
	clientName         string
	clientVersion      string
	requestAttestation bool
	initializeID       string
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
	if c == nil {
		return errors.New("attestation collector is nil")
	}
	normalized, err := keyForCollector(key)
	if err != nil {
		return err
	}
	clientName, clientVersion, capability, initializeID, err := parseCollectorInitialize(raw)
	if err != nil {
		return err
	}
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
	}
	c.appendRecordLocked(state, &CodexAppServerAttestationCaptureRecord{
		Event: "initialize", APIKeyID: apiKeyID, AccountID: 0,
		ConnectionID: normalized.ConnectionID, SessionID: normalized.SessionID,
		ThreadID: normalized.ThreadID, ClientName: clientName, ClientVersion: clientVersion,
		RequestAttestation: capability, InitializeID: initializeID,
		Status: map[bool]string{true: "negotiated", false: "not_requested"}[capability],
	})
	return nil
}

// RecordGenerateResponse 记录成功或 malformed 的 attestation/generate 响应。
// 成功时仅写入 token 长度和 SHA-256，不把 token 原文落入内存记录。
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
	record := &CodexAppServerAttestationCaptureRecord{
		Event: "generate", APIKeyID: apiKeyID, AccountID: normalized.AccountID, ConnectionID: normalized.ConnectionID,
		SessionID: normalized.SessionID, ThreadID: normalized.ThreadID,
		ClientName: connection.clientName, ClientVersion: connection.clientVersion,
		RequestAttestation: connection.requestAttestation, InitializeID: connection.initializeID,
		GenerateRequestID: requestID,
	}
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
	c.appendRecordLocked(state, &CodexAppServerAttestationCaptureRecord{
		Event: "generate", APIKeyID: apiKeyID, AccountID: normalized.AccountID, ConnectionID: normalized.ConnectionID,
		SessionID: normalized.SessionID, ThreadID: normalized.ThreadID,
		ClientName: connection.clientName, ClientVersion: connection.clientVersion,
		RequestAttestation: connection.requestAttestation, InitializeID: connection.initializeID,
		GenerateRequestID: requestID, Status: collectorStatusName(status),
	})
	return nil
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

func parseCollectorInitialize(raw []byte) (string, string, bool, string, error) {
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ClientInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
			Capabilities struct {
				RequestAttestation bool `json:"requestAttestation"`
			} `json:"capabilities"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return "", "", false, "", fmt.Errorf("decode initialize: %w", err)
	}
	if message.Method != "initialize" {
		return "", "", false, "", errors.New("collector requires initialize")
	}
	initializeID := strings.TrimSpace(string(message.ID))
	return message.Params.ClientInfo.Name, message.Params.ClientInfo.Version,
		message.Params.Capabilities.RequestAttestation, initializeID, nil
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
