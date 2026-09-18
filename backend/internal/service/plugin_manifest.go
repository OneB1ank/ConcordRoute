package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	pluginv1 "github.com/TokenFlux/TokenRouter/pkg/pluginapi/v1"
	pluginv2 "github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2"
)

const (
	PluginCapabilityOpenAIOAuthOutbound = "openai.oauth.outbound_transport.v1"
	PluginStateDisabled                 = "disabled"
	PluginStateStarting                 = "starting"
	PluginStateEnabled                  = "enabled"
	PluginStateError                    = "error"
	PluginStateIncompatible             = "incompatible"
	PluginSignatureTrusted              = "trusted"
	PluginSignatureUnsigned             = "unsigned"
)

var pluginIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)+$`)

var ErrPluginStateChanged = errors.New("插件状态已在其他实例中变化，请刷新后重试")

// PluginManifest 是 .s2plugin 包中可在执行二进制前检查的声明。
type PluginManifest struct {
	SchemaVersion int                      `json:"schema_version"`
	ID            string                   `json:"id"`
	Name          string                   `json:"name"`
	Version       string                   `json:"version"`
	Description   string                   `json:"description,omitempty"`
	Author        string                   `json:"author,omitempty"`
	Requires      PluginRequirements       `json:"requires"`
	Capabilities  []PluginCapability       `json:"capabilities"`
	Runtimes      map[string]PluginRuntime `json:"runtimes"`
	UI            PluginUIManifest         `json:"ui"`
	Files         map[string]string        `json:"files"`
}

type PluginRequirements struct {
	Sub2API                   string   `json:"sub2api"`
	RecommendedSub2APIVersion string   `json:"recommended_sub2api_version,omitempty"`
	TestedSub2APIVersions     []string `json:"tested_sub2api_versions,omitempty"`
	PluginProtocol            int      `json:"plugin_protocol"`
	TransportAPI              int      `json:"transport_api,omitempty"`
	ExtensionAPI              int      `json:"extension_api,omitempty"`
	UIBridge                  int      `json:"ui_bridge"`
}

type PluginCapability struct {
	ID          string                  `json:"id"`
	Platform    string                  `json:"platform"`
	AccountType string                  `json:"account_type"`
	Kind        pluginv2.CapabilityKind `json:"kind,omitempty"`
	Permissions []pluginv2.Permission   `json:"permissions,omitempty"`
	TimeoutMS   int64                   `json:"timeout_ms,omitempty"`
	FailureMode pluginv2.FailureMode    `json:"failure_mode,omitempty"`
	Synchronous bool                    `json:"synchronous,omitempty"`
}

type PluginRuntime struct {
	Path string `json:"path"`
}

type PluginUIManifest struct {
	Entrypoint string `json:"entrypoint"`
}

type PluginSignature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
	PublicKey string `json:"public_key,omitempty"`
}

// PluginCompatibility 是管理页面展示和启用门禁共同使用的兼容性结论。
type PluginCompatibility struct {
	Compatible         bool   `json:"compatible"`
	Tested             bool   `json:"tested"`
	Status             string `json:"status"`
	Message            string `json:"message"`
	CurrentSub2API     string `json:"current_sub2api_version"`
	RequiredSub2API    string `json:"required_sub2api_version"`
	RecommendedSub2API string `json:"recommended_sub2api_version"`
	PluginProtocol     int    `json:"plugin_protocol"`
	TransportAPI       int    `json:"transport_api"`
	ExtensionAPI       int    `json:"extension_api,omitempty"`
	UIBridge           int    `json:"ui_bridge"`
}

type PluginInstallation struct {
	// Set only after explicit approval of this exact verified upload; repositories
	// persist the publisher pin in the same transaction as install/upgrade.
	PublisherToTrust  *PluginPublisher          `json:"-"`
	ID                int64                     `json:"id"`
	PluginKey         string                    `json:"plugin_key"`
	Name              string                    `json:"name"`
	Version           string                    `json:"version"`
	Description       string                    `json:"description"`
	Author            string                    `json:"author"`
	Manifest          PluginManifest            `json:"manifest"`
	ArtifactData      []byte                    `json:"-"`
	ArtifactPath      string                    `json:"-"`
	InstallPath       string                    `json:"-"`
	BinaryPath        string                    `json:"-"`
	BinarySHA256      string                    `json:"binary_sha256"`
	SignatureStatus   string                    `json:"signature_status"`
	State             string                    `json:"state"`
	ConfigEncrypted   string                    `json:"-"`
	LastError         string                    `json:"last_error"`
	InstalledBy       *int64                    `json:"installed_by"`
	InstalledAt       time.Time                 `json:"installed_at"`
	EnabledAt         *time.Time                `json:"enabled_at"`
	UpdatedAt         time.Time                 `json:"updated_at"`
	Bindings          []PluginBinding           `json:"bindings"`
	Compatibility     PluginCompatibility       `json:"compatibility"`
	RuntimeHealthy    bool                      `json:"runtime_healthy"`
	RuntimeVersion    string                    `json:"runtime_version,omitempty"`
	RuntimeIsolation  string                    `json:"runtime_isolation,omitempty"`
	RuntimeMessage    string                    `json:"runtime_message"`
	CapabilityRuntime []PluginCapabilityRuntime `json:"capability_runtime,omitempty"`
}

type PluginBinding struct {
	ID             int64     `json:"id"`
	PluginID       int64     `json:"plugin_id"`
	Capability     string    `json:"capability"`
	Platform       string    `json:"platform"`
	AccountType    string    `json:"account_type"`
	Enabled        bool      `json:"enabled"`
	RolloutPercent int       `json:"rollout_percent"`
	Priority       int       `json:"priority"`
	AccountIDs     []int64   `json:"account_ids"`
	UserIDs        []int64   `json:"user_ids"`
	GroupIDs       []int64   `json:"group_ids"`
	MaxConcurrency int       `json:"max_concurrency"`
	TimeoutMS      int64     `json:"timeout_ms"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type PluginRepository interface {
	List(ctx context.Context) ([]*PluginInstallation, error)
	GetByID(ctx context.Context, id int64) (*PluginInstallation, error)
	GetByKey(ctx context.Context, key string) (*PluginInstallation, error)
	Install(ctx context.Context, plugin *PluginInstallation, bindings []PluginBinding) (*PluginInstallation, error)
	GetArtifact(ctx context.Context, id int64) ([]byte, error)
	Delete(ctx context.Context, id int64, expectedBinarySHA256 string) error
	BeginEnable(ctx context.Context, id int64, binarySHA256, expectedState string) error
	MarkRuntimeHealthy(ctx context.Context, id int64, binarySHA256, configEncrypted string) error
	UpdateState(ctx context.Context, id int64, state, lastError string, enabledAt *time.Time, expectedBinarySHA256, expectedState string) error
	UpdateConfig(ctx context.Context, id int64, encrypted, expectedBinarySHA256 string) error
	UpdateBindingsAndState(ctx context.Context, pluginID int64, bindings []PluginBinding, state, lastError string, enabledAt *time.Time, expectedState, expectedBinarySHA256 string) error
}

func (m PluginManifest) RuntimeKey() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

func (m PluginManifest) Validate() error {
	return m.ValidateForRuntime(m.RuntimeKey())
}

func (m PluginManifest) ValidateForRuntime(runtimeKey string) error {
	if m.SchemaVersion != 1 && m.SchemaVersion != 2 {
		return fmt.Errorf("不支持的插件清单版本: %d", m.SchemaVersion)
	}
	if !pluginIDPattern.MatchString(m.ID) || len(m.ID) > 160 {
		return errors.New("插件 ID 必须是长度不超过 160 的小写命名空间标识")
	}
	if strings.TrimSpace(m.Name) == "" || len(m.Name) > 160 {
		return errors.New("插件名称不能为空且不能超过 160 个字符")
	}
	if normalizeSemver(m.Version) == "" {
		return errors.New("插件版本必须是有效的语义化版本")
	}
	if strings.TrimSpace(m.Requires.Sub2API) == "" {
		return errors.New("插件必须声明 requires.sub2api")
	}
	if err := m.validateProtocol(); err != nil {
		return err
	}
	if len(m.Capabilities) == 0 {
		return errors.New("插件必须声明至少一个能力")
	}
	seen := make(map[string]bool)
	for _, capability := range m.Capabilities {
		if seen[capability.ID] {
			return fmt.Errorf("插件能力重复: %s", capability.ID)
		}
		seen[capability.ID] = true
		if m.SchemaVersion == 1 {
			if capability.ID != PluginCapabilityOpenAIOAuthOutbound || capability.Platform != PlatformOpenAI || capability.AccountType != AccountTypeOAuth {
				return fmt.Errorf("v1 仅支持能力 %s", PluginCapabilityOpenAIOAuthOutbound)
			}
			if capability.Kind != "" || len(capability.Permissions) > 0 || capability.TimeoutMS != 0 || capability.FailureMode != "" || capability.Synchronous {
				return errors.New("v1 清单不能声明 v2 能力属性")
			}
		} else if err := capability.ExtensionCapability().Validate(); err != nil {
			return err
		}
	}
	runtimeEntry, ok := m.Runtimes[runtimeKey]
	if !ok || !safePluginRelativePath(runtimeEntry.Path) {
		return fmt.Errorf("插件不支持运行平台 %s", runtimeKey)
	}
	if !safePluginRelativePath(m.UI.Entrypoint) || !strings.HasPrefix(m.UI.Entrypoint, "ui/") {
		return errors.New("插件 UI 入口必须位于 ui/ 目录")
	}
	if len(m.Files) == 0 {
		return errors.New("插件清单必须声明文件哈希")
	}
	for path, hash := range m.Files {
		if !safePluginRelativePath(path) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(hash) {
			return fmt.Errorf("插件文件声明无效: %s", path)
		}
	}
	if _, ok := m.Files[runtimeEntry.Path]; !ok {
		return errors.New("运行时二进制未包含在文件哈希声明中")
	}
	if _, ok := m.Files[m.UI.Entrypoint]; !ok {
		return errors.New("UI 入口未包含在文件哈希声明中")
	}
	return nil
}

func (m PluginManifest) validateProtocol() error {
	if m.Requires.UIBridge != pluginv1.UIBridgeVersion {
		return errors.New("插件 UI Bridge 版本与当前宿主不兼容")
	}
	if m.SchemaVersion == 1 && m.Requires.PluginProtocol == pluginv1.ProtocolVersion &&
		m.Requires.TransportAPI == pluginv1.TransportAPIVersion && m.Requires.ExtensionAPI == 0 {
		return nil
	}
	if m.SchemaVersion == 2 && m.Requires.PluginProtocol == int(pluginv2.ProtocolVersion) &&
		m.Requires.ExtensionAPI == 1 && m.Requires.TransportAPI == 0 {
		return nil
	}
	return errors.New("插件清单与协议版本不匹配")
}

func (c PluginCapability) ExtensionCapability() pluginv2.Capability {
	return pluginv2.Capability{ID: c.ID, Kind: c.Kind, Platform: c.Platform, AccountType: c.AccountType,
		Permissions: append([]pluginv2.Permission(nil), c.Permissions...), TimeoutMS: c.TimeoutMS,
		FailureMode: c.FailureMode, Synchronous: c.Synchronous}
}

// supportedExtensionCapability is the host registry for executable v2 hooks.
// Unknown capabilities remain installable, but cannot be enabled.
func supportedExtensionCapability(c PluginCapability) error {
	if c.ID == pluginv2.CapabilityProtectionTransport {
		return validateProtectionTransportCapability(c)
	}
	if c.ID != pluginv2.CapabilityRequestPreprocess {
		return fmt.Errorf("宿主尚未实现能力 %s", c.ID)
	}
	if err := c.ExtensionCapability().Validate(); err != nil {
		return err
	}
	if c.Kind != pluginv2.CapabilityKindHook || !c.Synchronous ||
		(c.FailureMode != pluginv2.FailureModeClosed && c.FailureMode != pluginv2.FailureModeOpen) {
		return errors.New("请求预处理必须是同步 hook，失败策略为 fail_closed 或 fail_open")
	}
	if c.Platform != PlatformOpenAI || (c.AccountType != AccountTypeOAuth && c.AccountType != AccountTypeAPIKey) {
		return errors.New("请求预处理当前支持 openai 平台的 oauth 或 apikey 账号")
	}
	metadata := false
	for _, permission := range c.Permissions {
		switch permission {
		case pluginv2.PermissionRequestMetadata:
			metadata = true
		case pluginv2.PermissionRequestBody, pluginv2.PermissionRequestMutate,
			pluginv2.PermissionHostLog, pluginv2.PermissionHostMetric, pluginv2.PermissionHostConfig,
			pluginv2.PermissionSecretBroker, pluginv2.PermissionEventPublish:
		default:
			return fmt.Errorf("请求预处理不支持权限 %s", permission)
		}
	}
	if !metadata {
		return errors.New("请求预处理必须声明 request.metadata.read")
	}
	if c.TimeoutMS > 5000 {
		return errors.New("同步请求预处理超时不能超过 5000ms")
	}
	return nil
}

func safePluginRelativePath(path string) bool {
	cleaned := strings.ReplaceAll(strings.TrimSpace(path), "\\", "/")
	return cleaned != "" && cleaned != "." && !strings.HasPrefix(cleaned, "/") &&
		!strings.HasPrefix(cleaned, "../") && !strings.Contains(cleaned, "/../") && cleaned == strings.TrimPrefix(cleaned, "./")
}

func (m PluginManifest) MarshalJSONBytes() ([]byte, error) {
	return json.Marshal(m)
}

func (m PluginManifest) SortedCapabilities() []PluginCapability {
	out := append([]PluginCapability(nil), m.Capabilities...)
	for i := range out {
		out[i].Permissions = append([]pluginv2.Permission(nil), out[i].Permissions...)
		sort.Slice(out[i].Permissions, func(a, b int) bool { return out[i].Permissions[a] < out[i].Permissions[b] })
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
