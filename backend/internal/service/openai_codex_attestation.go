package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
	"github.com/TokenFlux/TokenRouter/internal/platform/liveattestation"
	"github.com/gin-gonic/gin"
	"golang.org/x/net/http/httpguts"
)

type codexAttestationContextKey struct{}

// codexClientAttestationCandidate 保存客户端原始证明及其配套身份头。
// 证明头不加入通用白名单，而是在最终账号与客户端身份确认后单独处理。
type codexClientAttestationCandidate struct {
	Envelope   string
	UserAgent  string
	Originator string
}

// CodexAttestationRequestContext 描述已经由 app-server 协商完成的客户端连接。
// 普通公网请求不应自行构造该值；app-server 适配器在完成 initialize 后注入。
type CodexAttestationRequestContext struct {
	Key      liveattestation.SessionKey
	Envelope string
}

// CodexAppServerAttestationAdapter 是 JSON-RPC app-server 与 Responses/WS
// 请求之间的窄适配层。它只负责把客户端真实返回的 headerValue 交给短 TTL
// 存储，不在 Linux 网关上生成或签名设备证明。
type CodexAppServerAttestationAdapter struct {
	service           *OpenAIGatewayService
	key               liveattestation.SessionKey
	mu                sync.RWMutex
	envelope          string
	collector         *CodexAppServerAttestationCollector
	collectorToken    string
	collectorAPIKeyID int64
}

// SetCollector 将一次已建立的 app-server 采集会话绑定到适配器。
// 采集器只接收摘要，证明原文仍只在当前请求的短生命周期内存在。
func (a *CodexAppServerAttestationAdapter) SetCollector(collector *CodexAppServerAttestationCollector, token string, apiKeyID int64) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.collector = collector
	a.collectorToken = strings.TrimSpace(token)
	a.collectorAPIKeyID = apiKeyID
	a.mu.Unlock()
}

func (a *CodexAppServerAttestationAdapter) recordCollectorResponse(requestID uint64, raw []byte) {
	if a == nil {
		return
	}
	a.mu.RLock()
	collector, token, apiKeyID := a.collector, a.collectorToken, a.collectorAPIKeyID
	a.mu.RUnlock()
	if collector != nil && token != "" {
		_ = collector.RecordGenerateResponseForAPIKey(token, apiKeyID, a.key, requestID, raw)
	}
}

func (a *CodexAppServerAttestationAdapter) recordCollectorFailure(requestID uint64, status liveattestation.AttestationStatus) {
	if a == nil {
		return
	}
	a.mu.RLock()
	collector, token, apiKeyID := a.collector, a.collectorToken, a.collectorAPIKeyID
	a.mu.RUnlock()
	if collector != nil && token != "" {
		_ = collector.RecordGenerateFailureForAPIKey(token, apiKeyID, a.key, requestID, status)
	}
}

// accountSupportsCodexAppServerAttestation mirrors Codex's provider gate:
// only ChatGPT-authenticated OpenAI OAuth accounts may receive the desktop
// attestation envelope. PAT and agent-identity accounts use different auth
// paths and must never consume a proof generated for the ChatGPT app-server.
// A blank platform is retained for narrow unit-test fixtures; persisted
// accounts always carry PlatformOpenAI here.
func accountSupportsCodexAppServerAttestation(account *Account) bool {
	if account == nil || account.Type != AccountTypeOAuth {
		return false
	}
	if account.Platform != "" && account.Platform != PlatformOpenAI {
		return false
	}
	return !account.IsOpenAIPersonalAccessToken() && !account.IsOpenAIAgentIdentity()
}

// AppServerAttestationRoundTrip is the transport boundary between ConcordRoute
// and a Codex app-server JSON-RPC connection. The callback must send the JSON
// request to the negotiated client connection and return the matching response.
// Keeping this boundary transport-neutral allows stdio, WebSocket, or a host
// bridge to use the same attestation policy and session isolation.
type AppServerAttestationRoundTrip func(context.Context, []byte) ([]byte, error)

// NewCodexAppServerAttestationAdapter 为一条客户端连接创建隔离的适配器。
func (s *OpenAIGatewayService) NewCodexAppServerAttestationAdapter(key liveattestation.SessionKey) (*CodexAppServerAttestationAdapter, error) {
	if s == nil || s.codexAttestationStore == nil {
		return nil, errors.New("codex attestation store is unavailable")
	}
	if _, err := NewCodexAttestationRequestContext(key.AccountID, key.ConnectionID, key.SessionID, key.ThreadID); err != nil {
		return nil, err
	}
	return &CodexAppServerAttestationAdapter{service: s, key: key}, nil
}

// ObserveInitialize 处理客户端 initialize，并记录 requestAttestation 能力。
func (a *CodexAppServerAttestationAdapter) ObserveInitialize(raw []byte) (bool, error) {
	if a == nil || a.service == nil {
		return false, errors.New("codex attestation adapter is nil")
	}
	return a.service.ObserveCodexAppServerInitialize(a.key, raw)
}

// BeginGenerate 返回应发送给客户端的 attestation/generate JSON-RPC 请求。
func (a *CodexAppServerAttestationAdapter) BeginGenerate() ([]byte, uint64, error) {
	if a == nil || a.service == nil {
		return nil, 0, errors.New("codex attestation adapter is nil")
	}
	return a.service.BeginCodexAttestationGenerate(a.key)
}

// AcceptGenerateResponse 校验 JSON-RPC ID，并保存客户端返回的 opaque headerValue。
func (a *CodexAppServerAttestationAdapter) AcceptGenerateResponse(raw []byte) error {
	if a == nil || a.service == nil {
		return errors.New("codex attestation adapter is nil")
	}
	envelope, err := a.service.AcceptCodexAttestationGenerateResponseWithHeader(a.key, raw)
	if err != nil {
		return err
	}
	a.setEnvelope(envelope)
	return nil
}

// GenerateForRequest performs the Codex just-in-time attestation exchange.
// 成功响应使用客户端持有的不透明 headerValue 生成 s=0；传输层
// and client failures are converted to the official s=1..4 envelopes instead of
// synthesizing a token. The resulting header can be attached to exactly one
// upstream request by the caller.
func (a *CodexAppServerAttestationAdapter) GenerateForRequest(
	ctx context.Context,
	roundTrip AppServerAttestationRoundTrip,
) (string, error) {
	return a.GenerateForRequestWithTimeout(ctx, roundTrip, liveattestation.DefaultAppServerAttestationTimeout)
}

// GenerateForRequestWithTimeout keeps the local IPC default intact while
// allowing a remote bridge to include network latency in the pending lifetime.
func (a *CodexAppServerAttestationAdapter) GenerateForRequestWithTimeout(
	ctx context.Context,
	roundTrip AppServerAttestationRoundTrip,
	timeout time.Duration,
) (string, error) {
	if a == nil || a.service == nil {
		return "", errors.New("codex attestation adapter is nil")
	}
	if roundTrip == nil {
		return "", errors.New("app-server attestation round trip is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = liveattestation.DefaultAppServerAttestationTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	payload, requestID, err := a.service.BeginCodexAttestationGenerateWithTimeout(a.key, timeout)
	if err != nil {
		return "", err
	}
	recordFailure := func(status liveattestation.AttestationStatus) (string, error) {
		a.recordCollectorFailure(requestID, status)
		envelope, recordErr := a.service.RecordCodexAttestationFailureWithHeaderForRequest(a.key, requestID, status)
		if recordErr != nil {
			return "", recordErr
		}
		a.setEnvelope(envelope)
		return envelope, nil
	}
	response, err := roundTrip(requestCtx, payload)
	if err != nil {
		status := liveattestation.AttestationStatusRequestFailed
		switch {
		case errors.Is(err, context.Canceled) || errors.Is(requestCtx.Err(), context.Canceled):
			status = liveattestation.AttestationStatusRequestCanceled
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(requestCtx.Err(), context.DeadlineExceeded):
			status = liveattestation.AttestationStatusTimeout
		}
		return recordFailure(status)
	}
	if requestCtx.Err() != nil {
		if errors.Is(requestCtx.Err(), context.Canceled) {
			return recordFailure(liveattestation.AttestationStatusRequestCanceled)
		}
		return recordFailure(liveattestation.AttestationStatusTimeout)
	}
	envelope, err := a.service.AcceptCodexAttestationGenerateResponseWithHeader(a.key, response)
	if err != nil {
		// A response that arrives at the pending deadline is a timeout at the
		// app-server boundary, not a malformed client response. Preserve the
		// status semantics from PR #20619 while still treating every other
		// validation failure as malformed.
		if errors.Is(err, liveattestation.ErrAttestationRequestExpired) {
			return recordFailure(liveattestation.AttestationStatusTimeout)
		}
		if errors.Is(err, liveattestation.ErrAttestationResponseIDMismatch) {
			return recordFailure(liveattestation.AttestationStatusMalformedResponse)
		}
		if errors.Is(err, liveattestation.ErrAttestationClientRequestFailed) {
			return recordFailure(liveattestation.AttestationStatusRequestFailed)
		}
		return recordFailure(liveattestation.AttestationStatusMalformedResponse)
	}
	a.recordCollectorResponse(requestID, response)
	a.setEnvelope(envelope)
	return envelope, nil
}

func (a *CodexAppServerAttestationAdapter) setEnvelope(envelope string) {
	a.mu.Lock()
	a.envelope = envelope
	a.mu.Unlock()
}

// RequestContext 返回已完成协商后应绑定到上游请求的 context 值。
func (a *CodexAppServerAttestationAdapter) RequestContext() (CodexAttestationRequestContext, error) {
	if a == nil {
		return CodexAttestationRequestContext{}, errors.New("codex attestation adapter is nil")
	}
	value, err := NewCodexAttestationRequestContext(a.key.AccountID, a.key.ConnectionID, a.key.SessionID, a.key.ThreadID)
	if err != nil {
		return CodexAttestationRequestContext{}, err
	}
	a.mu.RLock()
	value.Envelope = a.envelope
	a.mu.RUnlock()
	return value, nil
}

// Clear 删除该连接的短时证明状态。
func (a *CodexAppServerAttestationAdapter) Clear() {
	if a == nil || a.service == nil || a.service.codexAttestationStore == nil {
		return
	}
	a.service.codexAttestationStore.Clear(a.key)
}

// WithCodexAttestationRequestContext 将内部协商会话绑定到上游请求 context。
func WithCodexAttestationRequestContext(ctx context.Context, value CodexAttestationRequestContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexAttestationContextKey{}, value)
}

func codexAttestationContextFrom(ctx context.Context) (CodexAttestationRequestContext, bool) {
	if ctx == nil {
		return CodexAttestationRequestContext{}, false
	}
	value, ok := ctx.Value(codexAttestationContextKey{}).(CodexAttestationRequestContext)
	if !ok || value.Key.AccountID <= 0 || strings.TrimSpace(value.Key.ConnectionID) == "" {
		return CodexAttestationRequestContext{}, false
	}
	return value, true
}

// ObserveCodexAppServerInitialize 记录客户端 initialize 的能力声明。
func (s *OpenAIGatewayService) ObserveCodexAppServerInitialize(key liveattestation.SessionKey, raw []byte) (bool, error) {
	if s == nil || s.codexAttestationStore == nil {
		return false, errors.New("codex attestation store is unavailable")
	}
	return s.codexAttestationStore.ObserveInitialize(key, raw)
}

// BeginCodexAttestationGenerate 发起一次短时 attestation/generate 请求。
func (s *OpenAIGatewayService) BeginCodexAttestationGenerate(key liveattestation.SessionKey) ([]byte, uint64, error) {
	if s == nil || s.codexAttestationStore == nil {
		return nil, 0, errors.New("codex attestation store is unavailable")
	}
	return s.codexAttestationStore.BeginGenerate(key)
}

func (s *OpenAIGatewayService) BeginCodexAttestationGenerateWithTimeout(key liveattestation.SessionKey, timeout time.Duration) ([]byte, uint64, error) {
	if s == nil || s.codexAttestationStore == nil {
		return nil, 0, errors.New("codex attestation store is unavailable")
	}
	return s.codexAttestationStore.BeginGenerateWithTimeout(key, timeout)
}

// AcceptCodexAttestationGenerateResponse 接收客户端响应并保存其 opaque headerValue。
func (s *OpenAIGatewayService) AcceptCodexAttestationGenerateResponse(key liveattestation.SessionKey, raw []byte) error {
	_, err := s.AcceptCodexAttestationGenerateResponseWithHeader(key, raw)
	return err
}

// AcceptCodexAttestationGenerateResponseWithHeader returns the envelope that
// was committed for this response, avoiding a second store lookup that could
// observe a later concurrent generate operation.
func (s *OpenAIGatewayService) AcceptCodexAttestationGenerateResponseWithHeader(key liveattestation.SessionKey, raw []byte) (string, error) {
	if s == nil || s.codexAttestationStore == nil {
		return "", errors.New("codex attestation store is unavailable")
	}
	return s.codexAttestationStore.AcceptGenerateResponseWithHeader(key, raw)
}

func (s *OpenAIGatewayService) RecordCodexAttestationFailureWithHeaderForRequest(key liveattestation.SessionKey, requestID uint64, status liveattestation.AttestationStatus) (string, error) {
	if s == nil || s.codexAttestationStore == nil {
		return "", errors.New("codex attestation store is unavailable")
	}
	return s.codexAttestationStore.RecordFailureWithHeaderForRequest(key, requestID, status)
}

func (s *OpenAIGatewayService) resolveCodexClientAttestation(ctx context.Context, account *Account) (string, bool) {
	if s == nil || !accountSupportsCodexAppServerAttestation(account) {
		return "", false
	}
	value, ok := codexAttestationContextFrom(ctx)
	if !ok || value.Key.AccountID != account.ID {
		return "", false
	}
	if strings.TrimSpace(value.Envelope) == "" {
		return "", false
	}
	envelope, err := liveattestation.NormalizeClientEnvelope(value.Envelope)
	if err != nil {
		return "", false
	}
	return envelope, true
}

// normalizeTrustedCodexClientAttestation validates a client-supplied envelope
// before it is relayed on a standard Responses/WS request.  The gateway never
// creates a token here: it only accepts an envelope that the official client
// already produced and whose identity headers are paired consistently.
func normalizeTrustedCodexClientAttestation(candidate codexClientAttestationCandidate) (string, bool) {
	raw := candidate.Envelope
	if strings.TrimSpace(raw) == "" || !httpguts.ValidHeaderFieldValue(raw) ||
		strings.ContainsAny(raw, "\r\n") ||
		!codexAttestationClientIdentityTrusted(candidate.UserAgent, candidate.Originator) {
		return "", false
	}
	raw = strings.TrimSpace(raw)
	envelope, err := liveattestation.NormalizeClientEnvelope(raw)
	if err != nil {
		return "", false
	}
	return envelope, true
}

// applyCodexClientAttestation 优先使用已经完成 app-server 协商的内部会话。
// 没有内部 context 时，仅中继经过身份配对和 envelope 语法校验的官方客户端
// 证明；缺少证明时保持缺省行为，不在网关生成 token。
func (s *OpenAIGatewayService) applyCodexClientAttestation(ctx context.Context, account *Account, headers http.Header, candidate ...codexClientAttestationCandidate) {
	if headers == nil {
		return
	}
	// 先清除所有已有值，避免账号切换或连接池复用把旧证明带到新请求。
	headers.Del(liveAttestationHeader)
	// Once a request has been bound to an app-server connection for this same
	// OAuth account, that connection is the sole attestation authority for the
	// request.  Do not fall back to a direct inbound header if the negotiated
	// context is present but its envelope is empty/invalid; doing so could mix a
	// proof from another transport or session into an app-server request.
	if bound, ok := codexAttestationContextFrom(ctx); ok && account != nil && bound.Key.AccountID == account.ID {
		if value, valid := s.resolveCodexClientAttestation(ctx, account); valid {
			headers.Set(liveAttestationHeader, value)
		}
		return
	}
	if value, ok := s.resolveCodexClientAttestation(ctx, account); ok {
		headers.Set(liveAttestationHeader, value)
		return
	}
	if !accountSupportsCodexAppServerAttestation(account) {
		return
	}
	for _, item := range candidate {
		if value, ok := normalizeTrustedCodexClientAttestation(item); ok {
			headers.Set(liveAttestationHeader, value)
			return
		}
	}
}

// codexAttestationClientIdentityTrusted 统一证明入口的客户端身份判定。
// UA/TLS 只能作为客户端家族线索；只有官方身份头配对通过后，才允许把客户端
// 自带的 opaque envelope 交给上游校验。该函数不生成、签名或验证设备证明本身。
func codexAttestationClientIdentityTrusted(userAgent, originator string) bool {
	if !httpguts.ValidHeaderFieldValue(userAgent) ||
		!httpguts.ValidHeaderFieldValue(originator) ||
		strings.ContainsAny(userAgent, "\r\n") ||
		strings.ContainsAny(originator, "\r\n") {
		return false
	}
	codexOfficial := openai.IsCodexOfficialClientRequestStrict(userAgent) ||
		openai.IsCodexOfficialClientOriginator(originator)
	// UA 与 originator 同时存在时，Codex 必须满足同一配对规则；否则一个
	// 合法 originator 不能掩盖伪造的中间前缀或不匹配客户端身份。
	if codexOfficial && strings.TrimSpace(userAgent) != "" && strings.TrimSpace(originator) != "" {
		pairedOriginator, _, paired := openai.PairCodexClientIdentity(userAgent)
		if !paired || !strings.EqualFold(strings.TrimSpace(originator), pairedOriginator) {
			codexOfficial = false
		}
	}
	claudeOfficial := openai.MatchAllowedClients(userAgent, originator, []string{openai.AllowedClientClaudeCode})
	return codexOfficial || claudeOfficial
}

// codexClientAttestationFromRequest 读取客户端原始证明；它只在 OAuth
// Responses/WS/Live 入口被显式调用，不改变通用请求头白名单。
func codexClientAttestationFromRequest(c *gin.Context) codexClientAttestationCandidate {
	if c == nil || c.Request == nil {
		return codexClientAttestationCandidate{}
	}
	return codexClientAttestationCandidate{
		Envelope:   c.Request.Header.Get(liveAttestationHeader),
		UserAgent:  c.Request.Header.Get("User-Agent"),
		Originator: c.Request.Header.Get("originator"),
	}
}

// NewCodexAttestationRequestContext 为 app-server 适配器提供便捷构造函数。
func NewCodexAttestationRequestContext(accountID int64, connectionID, sessionID, threadID string) (CodexAttestationRequestContext, error) {
	key := liveattestation.SessionKey{AccountID: accountID, ConnectionID: connectionID, SessionID: sessionID, ThreadID: threadID}
	if key.AccountID <= 0 || strings.TrimSpace(key.ConnectionID) == "" {
		return CodexAttestationRequestContext{}, fmt.Errorf("invalid Codex attestation context: %w", liveattestation.ErrInvalidSessionKey)
	}
	return CodexAttestationRequestContext{Key: key}, nil
}

// codexAttestationStoreTTL 与连接池 TTL 解耦；证明 token 只做短期会话缓存。
const codexAttestationStoreTTL = 5 * time.Minute
