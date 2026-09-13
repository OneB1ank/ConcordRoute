package liveattestation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultAppServerAttestationTimeout mirrors Codex app-server's just-in-time
// client request deadline.
const DefaultAppServerAttestationTimeout = 100 * time.Millisecond

var (
	ErrInvalidSessionKey             = errors.New("attestation session key is incomplete")
	ErrAttestationNotNegotiated      = errors.New("client did not negotiate attestation")
	ErrAttestationRequestExpired     = errors.New("attestation request expired or was not pending")
	ErrAttestationResponseIDMismatch = errors.New("attestation response id does not match pending request")
	ErrAttestationStoreFull          = errors.New("attestation store capacity reached")
)

const (
	maxAttestationKeyComponentBytes = 256
	maxAttestationSessions          = 4096
)

// AttestationStatus matches the app-server envelope status codes used by Codex.
// 0 is success; 1..4 describe failures local to the app-server/client bridge.
type AttestationStatus uint8

const (
	AttestationStatusOK                AttestationStatus = 0
	AttestationStatusTimeout           AttestationStatus = 1
	AttestationStatusRequestFailed     AttestationStatus = 2
	AttestationStatusRequestCanceled   AttestationStatus = 3
	AttestationStatusMalformedResponse AttestationStatus = 4
)

// SessionKey 把客户端证明绑定到账号、连接和客户端会话，避免 token 在账号或
// WebSocket 连接之间复用。SessionID/ThreadID 可以为空，但 ConnectionID 必须有值。
type SessionKey struct {
	AccountID    int64
	ConnectionID string
	SessionID    string
	ThreadID     string
}

func (k SessionKey) normalized() (SessionKey, error) {
	k.ConnectionID = strings.TrimSpace(k.ConnectionID)
	k.SessionID = strings.TrimSpace(k.SessionID)
	k.ThreadID = strings.TrimSpace(k.ThreadID)
	if k.AccountID <= 0 || k.ConnectionID == "" {
		return SessionKey{}, ErrInvalidSessionKey
	}
	for _, component := range []string{k.ConnectionID, k.SessionID, k.ThreadID} {
		for _, r := range component {
			if r == 0 || r < 0x20 || r == 0x7f {
				return SessionKey{}, fmt.Errorf("%w: session key contains a control character", ErrInvalidSessionKey)
			}
		}
	}
	if len(k.ConnectionID) > maxAttestationKeyComponentBytes ||
		len(k.SessionID) > maxAttestationKeyComponentBytes ||
		len(k.ThreadID) > maxAttestationKeyComponentBytes {
		return SessionKey{}, fmt.Errorf("%w: session key component exceeds %d bytes", ErrInvalidSessionKey, maxAttestationKeyComponentBytes)
	}
	return k, nil
}

// NormalizedForCollector 校验并规范化采集器使用的会话键。采集器与证明
// 存储共享同一组长度和控制字符边界，避免两条路径对连接身份的判定不一致。
func (k SessionKey) NormalizedForCollector() (SessionKey, error) {
	return k.normalized()
}

func (k SessionKey) mapKey() string {
	return strconv.FormatInt(k.AccountID, 10) + "\x00" + k.ConnectionID + "\x00" + k.SessionID + "\x00" + k.ThreadID
}

type sessionEntry struct {
	requestAttestation bool
	envelope           string
	expiresAt          time.Time
	pendingID          string
	pendingUntil       time.Time
}

// AppServerAttestationStore 保存短时客户端证明状态。
// 该存储只保存客户端成功返回的 opaque token，不执行 DeviceCheck 生成或签名。
type AppServerAttestationStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time
	nextID  atomic.Uint64
	items   map[string]sessionEntry
}

func NewAppServerAttestationStore(ttl time.Duration) *AppServerAttestationStore {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &AppServerAttestationStore{
		ttl:     ttl,
		timeout: DefaultAppServerAttestationTimeout,
		now:     time.Now,
		items:   make(map[string]sessionEntry),
	}
}

// ObserveInitialize 记录 initialize 中的 requestAttestation 能力。
// 未声明能力时清除该连接的旧 token，防止连接重用造成隐式继承。
func (s *AppServerAttestationStore) ObserveInitialize(key SessionKey, raw []byte) (bool, error) {
	if s == nil {
		return false, errors.New("attestation store is nil")
	}
	normalized, err := key.normalized()
	if err != nil {
		return false, err
	}
	capability, err := ParseInitializeRequest(raw)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeLocked(now)
	mapKey := normalized.mapKey()
	// A client that did not opt into requestAttestation has no state to keep.
	// Do not retain a negative capability entry: it would consume global store
	// capacity and could leave a stale token reachable after re-initialization.
	if !capability {
		delete(s.items, mapKey)
		return false, nil
	}
	if _, exists := s.items[mapKey]; !exists && len(s.items) >= maxAttestationSessions {
		return false, ErrAttestationStoreFull
	}
	entry := s.items[mapKey]
	entry.requestAttestation = capability
	entry.envelope = ""
	entry.pendingID = ""
	entry.pendingUntil = time.Time{}
	entry.expiresAt = now.Add(s.ttl)
	s.items[mapKey] = entry
	return capability, nil
}

// BeginGenerate 生成一个待匹配的 attestation/generate JSON-RPC 请求。
func (s *AppServerAttestationStore) BeginGenerate(key SessionKey) ([]byte, uint64, error) {
	return s.BeginGenerateWithTimeout(key, s.timeout)
}

// BeginGenerateWithTimeout starts a pending request using the transport's
// actual round-trip budget. Local app-server IPC keeps the official 100 ms
// default; an authenticated remote bridge can supply a larger network budget.
func (s *AppServerAttestationStore) BeginGenerateWithTimeout(key SessionKey, timeout time.Duration) ([]byte, uint64, error) {
	if s == nil {
		return nil, 0, errors.New("attestation store is nil")
	}
	if timeout <= 0 {
		timeout = DefaultAppServerAttestationTimeout
	}
	normalized, err := key.normalized()
	if err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeLocked(now)
	mapKey := normalized.mapKey()
	entry, ok := s.items[mapKey]
	if !ok || !entry.requestAttestation {
		return nil, 0, ErrAttestationNotNegotiated
	}
	id := s.nextID.Add(1)
	entry.pendingID = "n:" + strconv.FormatUint(id, 10)
	entry.pendingUntil = now.Add(timeout)
	entry.expiresAt = now.Add(s.ttl)
	s.items[mapKey] = entry
	payload, err := BuildAttestationGenerateRequest(id)
	if err != nil {
		return nil, 0, err
	}
	return payload, id, nil
}

// AcceptGenerateResponse 校验 pending request ID，并保存客户端返回的真实 token。
func (s *AppServerAttestationStore) AcceptGenerateResponse(key SessionKey, raw []byte) error {
	_, err := s.AcceptGenerateResponseWithHeader(key, raw)
	return err
}

// AcceptGenerateResponseWithHeader validates and stores the client token,
// returning the exact envelope committed under the same lock. Callers that
// attach the result to a single upstream request should use this method so a
// concurrent generate cannot replace the value between validation and use.
func (s *AppServerAttestationStore) AcceptGenerateResponseWithHeader(key SessionKey, raw []byte) (string, error) {
	if s == nil {
		return "", errors.New("attestation store is nil")
	}
	normalized, err := key.normalized()
	if err != nil {
		return "", err
	}
	idRaw, token, parseErr := ParseAttestationGenerateResponse(raw)
	var responseID string
	if len(idRaw) > 0 {
		// ParseAttestationGenerateResponse returns the raw id for responses that
		// carry a valid JSON-RPC error or a malformed result. Canonicalize it
		// before looking at parseErr so a response with the wrong id is always
		// classified as a malformed response (s=4), not a client request failure
		// (s=2).
		responseID, err = canonicalRequestID(idRaw)
		if err != nil {
			return "", fmt.Errorf("decode attestation response id: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeLocked(now)
	mapKey := normalized.mapKey()
	entry, ok := s.items[mapKey]
	// Treat the deadline as an exclusive upper bound.  A response observed at
	// the exact deadline is already outside the app-server request budget and
	// must be reported as expired rather than being accepted nondeterministically.
	if !ok || !entry.requestAttestation || entry.pendingID == "" ||
		(!now.Before(entry.pendingUntil)) {
		return "", ErrAttestationRequestExpired
	}
	if responseID != "" && entry.pendingID != responseID {
		return "", ErrAttestationResponseIDMismatch
	}
	if parseErr != nil {
		return "", parseErr
	}
	tokenJSON, err := json.Marshal(token)
	if err != nil {
		return "", fmt.Errorf("encode attestation token: %w", err)
	}
	envelope, err := NormalizeClientEnvelope(fmt.Sprintf(`{"v":1,"s":0,"t":%s}`, tokenJSON))
	if err != nil {
		return "", err
	}
	entry.envelope = envelope
	entry.pendingID = ""
	entry.pendingUntil = time.Time{}
	entry.expiresAt = now.Add(s.ttl)
	s.items[mapKey] = entry
	return envelope, nil
}

// RecordFailure stores the same status envelope that Codex app-server emits
// when client attestation generation times out, fails, is canceled, or returns
// malformed data. The pending request must exist and is consumed exactly once.
func (s *AppServerAttestationStore) RecordFailure(key SessionKey, status AttestationStatus) error {
	_, err := s.RecordFailureWithHeader(key, status)
	return err
}

// RecordFailureWithHeader stores and returns the status envelope atomically
// with consuming the pending request.
func (s *AppServerAttestationStore) RecordFailureWithHeader(key SessionKey, status AttestationStatus) (string, error) {
	return s.recordFailureWithHeader(key, "", status)
}

// RecordFailureWithHeaderForRequest consumes the pending request identified by
// requestID, even when its short response deadline has just elapsed. This is
// needed to turn a transport timeout into the official s=1 envelope without
// allowing a late failure from an older request to overwrite a newer pending
// generate operation.
func (s *AppServerAttestationStore) RecordFailureWithHeaderForRequest(key SessionKey, requestID uint64, status AttestationStatus) (string, error) {
	if requestID == 0 {
		return "", errors.New("attestation request id must be positive")
	}
	return s.recordFailureWithHeader(key, "n:"+strconv.FormatUint(requestID, 10), status)
}

func (s *AppServerAttestationStore) recordFailureWithHeader(key SessionKey, expectedPendingID string, status AttestationStatus) (string, error) {
	if s == nil {
		return "", errors.New("attestation store is nil")
	}
	if status == AttestationStatusOK || status > AttestationStatusMalformedResponse {
		return "", errors.New("invalid attestation failure status")
	}
	normalized, err := key.normalized()
	if err != nil {
		return "", err
	}
	envelope, err := NormalizeClientEnvelope(fmt.Sprintf(`{"v":1,"s":%d}`, status))
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeLocked(now)
	mapKey := normalized.mapKey()
	entry, ok := s.items[mapKey]
	if !ok || !entry.requestAttestation || entry.pendingID == "" ||
		(expectedPendingID != "" && entry.pendingID != expectedPendingID) ||
		(expectedPendingID == "" && !now.Before(entry.pendingUntil)) {
		return "", ErrAttestationRequestExpired
	}
	entry.envelope = envelope
	entry.pendingID = ""
	entry.pendingUntil = time.Time{}
	entry.expiresAt = now.Add(s.ttl)
	s.items[mapKey] = entry
	return envelope, nil
}

// HeaderForRequest 返回已协商并成功获取的 upstream header 值。
func (s *AppServerAttestationStore) HeaderForRequest(key SessionKey) (string, bool) {
	if s == nil {
		return "", false
	}
	normalized, err := key.normalized()
	if err != nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(s.now())
	entry, ok := s.items[normalized.mapKey()]
	if !ok || !entry.requestAttestation || entry.envelope == "" {
		return "", false
	}
	return entry.envelope, true
}

// Clear 删除一个连接的全部证明状态。
func (s *AppServerAttestationStore) Clear(key SessionKey) {
	if s == nil {
		return
	}
	if normalized, err := key.normalized(); err == nil {
		s.mu.Lock()
		delete(s.items, normalized.mapKey())
		s.mu.Unlock()
	}
}

func (s *AppServerAttestationStore) purgeLocked(now time.Time) {
	for key, entry := range s.items {
		if !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt) {
			delete(s.items, key)
		}
	}
}

func canonicalRequestID(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", errors.New("request id is empty")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return "", errors.New("request id is empty")
		}
		return "s:" + typed, nil
	case json.Number:
		// The app-server protocol models numeric RequestId values as integers;
		// reject decimal/exponent spellings instead of treating them as a
		// separate correlation namespace.
		integer, err := strconv.ParseInt(typed.String(), 10, 64)
		if err != nil {
			return "", errors.New("request id must be an integer or string")
		}
		return "n:" + strconv.FormatInt(integer, 10), nil
	default:
		return "", errors.New("request id must be a string or number")
	}
}
