package liveattestation

import (
	"context"
	"runtime"
	"sync/atomic"
)

var appServerAttestationTransport atomic.Bool

// SetAppServerAttestationTransport 由真实 JSON-RPC bridge 在有活动连接时调用。
// 没有活动 bridge 时能力保持 false，避免管理接口虚报可用。
func SetAppServerAttestationTransport(enabled bool) {
	appServerAttestationTransport.Store(enabled)
}

func AppServerAttestationTransportEnabled() bool {
	return appServerAttestationTransport.Load()
}

// CapabilityStatus 描述服务端与客户端证明相关的能力边界。
//
// ClientAttestationRelay 表示网关可以接收并校验客户端 app-server 返回的
// opaque token，再按会话隔离转发；它不表示网关能够自行生成设备证明。
type CapabilityStatus struct {
	ClientAttestationRelay    bool   `json:"client_attestation_relay"`
	ClientAttestationSource   string `json:"client_attestation_source"`
	ServerAttestationProvider string `json:"server_attestation_provider"`
	// AppServerAttestationTransport 表示当前进程是否已经挂接真实的
	// Codex app-server JSON-RPC 传输。适配器存在不等于公共路由会自动
	// 发起 attestation/generate。
	AppServerAttestationTransport bool `json:"app_server_attestation_transport"`
	// 这里只检查已配置提供器或已连接中继；不代表特定账号的证明已获上游认可。
	AttestationSourceAvailable bool `json:"attestation_source_available"`
	// WebRTC 信令和 Sideband 转发不依赖本机生成证明；账号资格仍由上游确认。
	LiveTransportSupported bool `json:"live_transport_supported"`
	// LiveClientSupported 表示受支持的客户端可以把真实证明交给网关中继，
	// 即使网关自身运行平台不能生成 DeviceCheck 证明。
	LiveClientSupported      bool     `json:"live_client_supported"`
	LiveAttestationMode      string   `json:"live_attestation_mode"`
	LiveDeviceCheckServer    bool     `json:"live_devicecheck_server"`
	ServerPlatform           string   `json:"server_platform"`
	SupportedClientPlatforms []string `json:"supported_client_platforms"`
	TLSProfilePlatform       string   `json:"tls_profile_platform"`
	LiveDeviceCheckReason    string   `json:"live_devicecheck_reason,omitempty"`
}

// CurrentCapabilityStatus 返回当前构建的证明能力快照。
// Windows Codex 的真实证明来自客户端；服务器平台和 Windows TLS/UA 模板
// 仅用于传输身份与连接特征，不能被当作 DeviceCheck 证明来源。
// @project-doc docs/interfaces/openai_upstream.md#openai_live_runtime
func CurrentCapabilityStatus(ctx context.Context) CapabilityStatus {
	return currentCapabilityStatus(ctx, NewProvider(), AppServerAttestationTransportEnabled())
}

// 将传输能力与运行态证明来源分开，避免把中继实现等同于已有真实证明。
func currentCapabilityStatus(ctx context.Context, provider Provider, bridgeConnected bool) CapabilityStatus {
	status := CapabilityStatus{
		ClientAttestationRelay:        true,
		ClientAttestationSource:       "windows_codex_app_server",
		ServerAttestationProvider:     "none",
		AppServerAttestationTransport: bridgeConnected,
		AttestationSourceAvailable:    bridgeConnected,
		LiveTransportSupported:        true,
		LiveClientSupported:           true,
		ServerPlatform:                runtime.GOOS,
		SupportedClientPlatforms:      []string{"windows"},
		TLSProfilePlatform:            "windows",
		LiveAttestationMode:           "none",
	}
	if bridgeConnected {
		status.LiveAttestationMode = "client_relay"
	}
	if err := provider.Check(ctx); err != nil {
		status.LiveDeviceCheckReason = err.Error()
		return status
	}
	status.LiveDeviceCheckServer = true
	status.AttestationSourceAvailable = true
	switch runtime.GOOS {
	case "linux":
		status.ServerAttestationProvider = "linux_external_helper"
	case "darwin":
		status.ServerAttestationProvider = "macos_devicecheck"
	default:
		status.ServerAttestationProvider = "platform_provider"
	}
	status.LiveAttestationMode = "server_provider"
	if bridgeConnected {
		status.LiveAttestationMode = "server_and_client_relay"
	}
	return status
}
