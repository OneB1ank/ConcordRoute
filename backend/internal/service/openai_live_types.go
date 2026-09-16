package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	// LiveControllerPending 表示会话尚未由 observer 或客户端控制。
	LiveControllerPending  = "pending"
	LiveControllerObserver = "observer"
	LiveControllerProxy    = "proxy"
	LiveControllerClosed   = "closed"
)

var (
	// ErrLiveUnavailable 表示运行环境或基础设施无法提供 Live 服务。
	ErrLiveUnavailable        = errors.New("live is unavailable")
	ErrLiveConcurrencyFull    = errors.New("live concurrency is full")
	ErrLiveCallNotFound       = errors.New("live call not found")
	ErrLiveIdentityMismatch   = errors.New("live call identity mismatch")
	ErrLiveControllerChanged  = errors.New("live controller changed")
	ErrLiveClientPolicyDenied = errors.New("live client is denied by account policy")
)

// LiveAttestationUnavailableError 保留无法生成或复用设备证明的具体原因。
type LiveAttestationUnavailableError struct {
	Reason string
}

func (e *LiveAttestationUnavailableError) Error() string {
	if e == nil || e.Reason == "" {
		return "Live attestation is unavailable"
	}
	return "Live attestation is unavailable: " + e.Reason
}

// LiveCallRequest 是两个下游创建协议归一后的请求。Session 不做结构改写。
type LiveCallRequest struct {
	SDP     string          `json:"sdp"`
	Session json.RawMessage `json:"session"`
}

// LiveCallIdentity 保存创建者身份和 fork 路由所需的入站元数据。
type LiveCallIdentity struct {
	APIKeyID       int64
	ActorUserID    int64
	UserID         int64
	TeamID         *int64
	GroupID        *int64
	SubscriptionID *int64
	UserAgent      string
	Originator     string
	// 客户端原始根会话/线程提示仅用于选择证明中继，不参与出站身份映射。
	ClientSessionID string
	ClientThreadID  string
	// ClientAttestationEnvelope 是 Windows Codex/Claude 客户端生成的
	// x-oai-attestation 原始 envelope。服务端只做语法校验与加密保存，
	// 不在 Linux 上伪造或签发设备证明。
	ClientAttestationEnvelope string
	IPAddress                 string
	InboundEndpoint           string
	ModelMapping              map[string]string
}

// LiveCallRecord 保存跨实例接管 Live 控制连接所需的会话状态。
type LiveCallRecord struct {
	CallID             string
	CallHash           string
	AccountID          int64
	APIKeyID           int64
	ActorUserID        int64
	UserID             int64
	TeamID             int64
	GroupID            int64
	SubscriptionID     int64
	LeaseID            string
	Model              string
	RequestedModel     string
	UpstreamModel      string
	ModelMappingChain  string
	APIKeyModelMapping map[string]string
	CreatedAt          time.Time
	ExpiresAt          time.Time
	Controller         string
	ControllerOwner    string
	UserAgent          string
	IPAddress          string
	InboundEndpoint    string
	// AttestationCiphertext 仅用于让同一会话的 Sideband 复用创建时的证明。
	AttestationCiphertext string
}

// LiveCallCreated 表示上游成功创建的 Live 会话。
type LiveCallCreated struct {
	SDP      []byte
	CallID   string
	Location string
	Account  *Account
}

// LiveCallStore 由 GatewayCache 的 Redis 实现可选提供，避免扩大旧缓存接口。
type LiveCallStore interface {
	SaveLiveCall(ctx context.Context, record *LiveCallRecord, ttl time.Duration) error
	GetLiveCall(ctx context.Context, callHash string) (*LiveCallRecord, error)
	ClaimLiveController(ctx context.Context, callHash, controller, owner string) (bool, error)
	ReleaseLiveController(ctx context.Context, callHash, owner string) (bool, error)
	GetLiveController(ctx context.Context, callHash string) (string, error)
	MarkLiveCallClosed(ctx context.Context, callHash string, ttl time.Duration) (bool, error)
}

// LiveConcurrencyCache 管理同时计入账号、用户和 API Key 的长会话租约。
type LiveConcurrencyCache interface {
	AcquireLiveLease(
		ctx context.Context,
		accountID int64,
		accountMax int,
		userID int64,
		userMax int,
		apiKeyID int64,
		leaseID string,
		replacingRegularSlots bool,
	) (bool, error)
	RefreshLiveLease(ctx context.Context, accountID, userID, apiKeyID int64, leaseID string) (bool, error)
	ReleaseLiveLease(ctx context.Context, accountID, userID, apiKeyID int64, leaseID string) error
}
