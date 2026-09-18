// Package pluginv2 defines the versioned contract for user-space Sub2API
// extensions. The contract is intentionally independent from internal service
// types so plugins can be implemented and upgraded outside the host process.
package pluginv2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// ProtocolVersion identifies this extension contract. Existing transport
	// plugins continue to use pluginapi/v1 and are not affected by this value.
	ProtocolVersion uint32 = 2

	CapabilityRequestPreprocess = "request.preprocess.v1"
	CapabilityProviderAdapter   = "provider.adapter.v1"
	CapabilityEventSink         = "event.sink.v1"
	CapabilityBackgroundWorker  = "background.worker.v1"

	CapabilityKindHook      CapabilityKind = "hook"
	CapabilityKindProvider  CapabilityKind = "provider"
	CapabilityKindEventSink CapabilityKind = "event_sink"
	CapabilityKindWorker    CapabilityKind = "worker"

	FailureModeClosed FailureMode = "fail_closed"
	FailureModeOpen   FailureMode = "fail_open"
	FailureModeAsync  FailureMode = "async"

	PermissionRequestMetadata Permission = "request.metadata.read"
	PermissionRequestBody     Permission = "request.body.read"
	PermissionRequestMutate   Permission = "request.mutate"
	PermissionNetworkOutbound Permission = "network.outbound"
	PermissionSecretBroker    Permission = "secrets.broker"
	PermissionEventPublish    Permission = "events.publish"
	PermissionHostLog         Permission = "host.log"
	PermissionHostMetric      Permission = "host.metric"
	PermissionHostConfig      Permission = "host.config.read"

	DecisionPass   Decision = "pass"
	DecisionModify Decision = "modify"
	DecisionDeny   Decision = "deny"
	DecisionError  Decision = "error"

	MaxRequestBodyBytes = 4 * 1024 * 1024
	MaxHeaderCount      = 64
	MaxHeaderValues     = 16
	MaxReasonBytes      = 2048
	MaxHeaderBytes      = 16 * 1024
)

var capabilityIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)+\.v[1-9][0-9]*$`)

type CapabilityKind string
type FailureMode string
type Permission string
type Decision string

// Capability describes one independently routable plugin capability.
type Capability struct {
	ID          string         `json:"id"`
	Kind        CapabilityKind `json:"kind"`
	Platform    string         `json:"platform,omitempty"`
	AccountType string         `json:"account_type,omitempty"`
	Permissions []Permission   `json:"permissions"`
	TimeoutMS   int64          `json:"timeout_ms"`
	FailureMode FailureMode    `json:"failure_mode"`
	Synchronous bool           `json:"synchronous"`
}

func (c Capability) Validate() error {
	if !capabilityIDPattern.MatchString(c.ID) {
		return fmt.Errorf("能力 ID 无效: %q", c.ID)
	}
	switch c.Kind {
	case CapabilityKindHook, CapabilityKindProvider, CapabilityKindEventSink, CapabilityKindWorker:
	default:
		return fmt.Errorf("不支持的能力类型: %q", c.Kind)
	}
	if c.TimeoutMS < 1 || c.TimeoutMS > 120000 {
		return fmt.Errorf("能力超时必须在 1ms 到 2m 之间: %dms", c.TimeoutMS)
	}
	switch c.FailureMode {
	case FailureModeClosed, FailureModeOpen, FailureModeAsync:
	default:
		return fmt.Errorf("不支持的能力失败策略: %q", c.FailureMode)
	}
	if c.FailureMode == FailureModeAsync && c.Synchronous {
		return errors.New("异步能力不能声明为同步调用")
	}
	if c.Kind == CapabilityKindEventSink && c.FailureMode != FailureModeAsync {
		return errors.New("事件能力必须使用 async 失败策略")
	}
	seen := make(map[Permission]struct{}, len(c.Permissions))
	for _, permission := range c.Permissions {
		if strings.TrimSpace(string(permission)) == "" {
			return errors.New("能力权限不能为空")
		}
		if _, ok := seen[permission]; ok {
			return fmt.Errorf("能力权限重复: %q", permission)
		}
		seen[permission] = struct{}{}
	}
	return nil
}

func (c Capability) TimeoutDuration() time.Duration {
	return time.Duration(c.TimeoutMS) * time.Millisecond
}

// PluginInfo is the information returned during the host handshake.
type PluginInfo struct {
	PluginID        string       `json:"plugin_id"`
	PluginVersion   string       `json:"plugin_version"`
	ProtocolVersion uint32       `json:"protocol_version"`
	Capabilities    []Capability `json:"capabilities"`
}

func (i PluginInfo) Validate() error {
	if strings.TrimSpace(i.PluginID) == "" {
		return errors.New("插件 ID 不能为空")
	}
	if strings.TrimSpace(i.PluginVersion) == "" {
		return errors.New("插件版本不能为空")
	}
	if i.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("插件协议版本不匹配: %d", i.ProtocolVersion)
	}
	if len(i.Capabilities) == 0 {
		return errors.New("插件至少需要声明一个能力")
	}
	seen := make(map[string]struct{}, len(i.Capabilities))
	for _, capability := range i.Capabilities {
		if err := capability.Validate(); err != nil {
			return err
		}
		if _, ok := seen[capability.ID]; ok {
			return fmt.Errorf("插件能力重复: %q", capability.ID)
		}
		seen[capability.ID] = struct{}{}
	}
	return nil
}

// HealthStatus is deliberately small so health checks remain fast and safe.
type HealthStatus struct {
	Healthy bool   `json:"healthy"`
	Message string `json:"message,omitempty"`
}

// RequestContext is the sanitized, minimum context sent to synchronous hooks.
// Sensitive headers must be removed by the host before this structure is built.
type RequestContext struct {
	RequestID   string              `json:"request_id"`
	TraceID     string              `json:"trace_id,omitempty"`
	Deadline    time.Time           `json:"deadline"`
	Platform    string              `json:"platform"`
	AccountType string              `json:"account_type"`
	AccountID   int64               `json:"account_id,omitempty"`
	UserID      int64               `json:"user_id,omitempty"`
	GroupID     int64               `json:"group_id,omitempty"`
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Host        string              `json:"host"`
	Model       string              `json:"model,omitempty"`
	Headers     map[string][]string `json:"headers,omitempty"`
}

func (c RequestContext) Validate() error {
	for name, value := range map[string]string{
		"request_id":   c.RequestID,
		"platform":     c.Platform,
		"account_type": c.AccountType,
		"method":       c.Method,
		"path":         c.Path,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("请求上下文 %s 不能为空", name)
		}
	}
	if c.Deadline.IsZero() {
		return errors.New("请求上下文必须包含截止时间")
	}
	if len(c.Headers) > MaxHeaderCount {
		return fmt.Errorf("请求头数量超过限制: %d", len(c.Headers))
	}
	headerBytes := 0
	for name, values := range c.Headers {
		if !validHeaderName(name) {
			return fmt.Errorf("请求头名称无效: %q", name)
		}
		if isSensitiveHeader(name) {
			return fmt.Errorf("请求上下文不能包含敏感请求头: %q", name)
		}
		if len(values) > MaxHeaderValues {
			return fmt.Errorf("请求头 %q 的值数量超过限制", name)
		}
		headerBytes += len(name)
		for _, value := range values {
			headerBytes += len(value)
			if strings.ContainsAny(value, "\r\n\x00") {
				return errors.New("请求头值包含控制字符")
			}
		}
	}
	if headerBytes > MaxHeaderBytes {
		return errors.New("请求头总大小超过限制")
	}
	return nil
}

// PreprocessRequest is the first synchronous generic hook contract.
type PreprocessRequest struct {
	Capability string         `json:"capability"`
	Context    RequestContext `json:"context"`
	BodyJSON   []byte         `json:"body_json,omitempty"`
}

func (r PreprocessRequest) Validate() error {
	if r.Capability != CapabilityRequestPreprocess {
		return fmt.Errorf("不支持的预处理能力: %q", r.Capability)
	}
	if err := r.Context.Validate(); err != nil {
		return err
	}
	if len(r.BodyJSON) > MaxRequestBodyBytes {
		return fmt.Errorf("请求体超过限制: %d", len(r.BodyJSON))
	}
	if len(r.BodyJSON) > 0 && !json.Valid(r.BodyJSON) {
		return errors.New("请求体不是有效 JSON")
	}
	return nil
}

// RequestPatch contains only fields the plugin explicitly changes. Empty
// fields preserve the host's original value.
type RequestPatch struct {
	Method      string              `json:"method,omitempty"`
	Path        string              `json:"path,omitempty"`
	Headers     map[string][]string `json:"headers,omitempty"`
	BodyJSON    []byte              `json:"body_json,omitempty"`
	BodyChanged bool                `json:"body_changed,omitempty"`
}

func (p RequestPatch) Validate() error {
	if len(p.Headers) > MaxHeaderCount {
		return fmt.Errorf("修改后的请求头数量超过限制: %d", len(p.Headers))
	}
	headerBytes := 0
	for name, values := range p.Headers {
		if !validHeaderName(name) {
			return fmt.Errorf("修改后的请求头名称无效: %q", name)
		}
		if isSensitiveHeader(name) {
			return fmt.Errorf("插件不能修改敏感请求头: %q", name)
		}
		if len(values) > MaxHeaderValues {
			return fmt.Errorf("修改后的请求头 %q 的值数量超过限制", name)
		}
		headerBytes += len(name)
		for _, value := range values {
			headerBytes += len(value)
			if strings.ContainsAny(value, "\r\n\x00") {
				return errors.New("修改后的请求头包含控制字符")
			}
		}
	}
	if headerBytes > MaxHeaderBytes {
		return errors.New("修改后的请求头总大小超过限制")
	}
	if !p.BodyChanged && len(p.BodyJSON) > 0 {
		return errors.New("请求体变更必须声明 body_changed")
	}
	if len(p.BodyJSON) > MaxRequestBodyBytes {
		return fmt.Errorf("修改后的请求体超过限制: %d", len(p.BodyJSON))
	}
	if p.BodyChanged && len(p.BodyJSON) > 0 && !json.Valid(p.BodyJSON) {
		return errors.New("修改后的请求体不是有效 JSON")
	}
	if p.BodyChanged || p.Method != "" || p.Path != "" || len(p.Headers) > 0 {
		return nil
	}
	return errors.New("修改决策没有包含任何请求变更")
}

// PreprocessResponse is the only result a request preprocessing plugin may
// return. The host remains authoritative over authentication, account choice,
// billing and persistence.
type PreprocessResponse struct {
	Decision Decision      `json:"decision"`
	Patch    *RequestPatch `json:"patch,omitempty"`
	Code     string        `json:"code,omitempty"`
	Reason   string        `json:"reason,omitempty"`
}

func (r PreprocessResponse) Validate() error {
	switch r.Decision {
	case DecisionPass:
		if r.Patch != nil || r.Code != "" {
			return errors.New("pass 决策不能包含修改或错误码")
		}
	case DecisionModify:
		if r.Patch == nil {
			return errors.New("modify 决策必须包含 patch")
		}
		if err := r.Patch.Validate(); err != nil {
			return err
		}
	case DecisionDeny:
		if r.Patch != nil || strings.TrimSpace(r.Reason) == "" {
			return errors.New("deny 决策必须只包含原因")
		}
	case DecisionError:
		if r.Patch != nil || strings.TrimSpace(r.Code) == "" || strings.TrimSpace(r.Reason) == "" {
			return errors.New("error 决策必须包含错误码和原因")
		}
	default:
		return fmt.Errorf("不支持的决策: %q", r.Decision)
	}
	if len(r.Code) > MaxReasonBytes || len(r.Reason) > MaxReasonBytes {
		return errors.New("插件决策文本超过限制")
	}
	return nil
}

// ExtensionHandler is the host-independent Go interface implemented by a
// plugin adapter. A transport layer can map these methods to the gRPC service
// declared in extension.proto.
type ExtensionHandler interface {
	GetInfo(context.Context) (PluginInfo, error)
	Health(context.Context) (HealthStatus, error)
	ValidateConfig(context.Context, json.RawMessage) (json.RawMessage, error)
	ApplyConfig(context.Context, json.RawMessage) error
	TestConfig(context.Context, json.RawMessage) (time.Duration, error)
	Preprocess(context.Context, PreprocessRequest) (PreprocessResponse, error)
}

func NormalizeCapabilities(capabilities []Capability) ([]Capability, error) {
	cloned := append([]Capability(nil), capabilities...)
	for i := range cloned {
		if err := cloned[i].Validate(); err != nil {
			return nil, err
		}
		cloned[i].Permissions = append([]Permission(nil), cloned[i].Permissions...)
		sort.Slice(cloned[i].Permissions, func(left, right int) bool {
			return cloned[i].Permissions[left] < cloned[i].Permissions[right]
		})
	}
	sort.Slice(cloned, func(left, right int) bool { return cloned[left].ID < cloned[right].ID })
	for i := 1; i < len(cloned); i++ {
		if cloned[i-1].ID == cloned[i].ID {
			return nil, fmt.Errorf("能力重复: %q", cloned[i].ID)
		}
	}
	return cloned, nil
}

func validHeaderName(name string) bool {
	if name == "" || strings.ToLower(name) != name {
		return false
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func isSensitiveHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key", "x-goog-api-key":
		return true
	default:
		return false
	}
}
