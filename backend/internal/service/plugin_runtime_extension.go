package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	pluginv1 "github.com/TokenFlux/TokenRouter/pkg/pluginapi/v1"
	pluginv2 "github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2"
	"google.golang.org/grpc/status"
)

// Keep plugin-supplied error text out of host logs and admin errors.
type extensionRPCError struct {
	operation string
	cause     error
}

func (e *extensionRPCError) Error() string { return e.operation + ": " + status.Code(e.cause).String() }
func (e *extensionRPCError) Unwrap() error { return e.cause }
func safeExtensionRPCError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return &extensionRPCError{operation: operation, cause: err}
}

func (r *pluginRuntime) initializeAPI(ctx context.Context, dispensed any) error {
	i := r.installation
	if i.Manifest.SchemaVersion == 2 {
		api, ok := dispensed.(pluginv2.ExtensionHandler)
		if !ok {
			return errors.New("插件未实现扩展 gRPC 客户端")
		}
		info, err := api.GetInfo(ctx)
		if err != nil {
			return safeExtensionRPCError("读取插件扩展信息失败", err)
		}
		if info.PluginID != i.PluginKey || info.PluginVersion != i.Version || info.ProtocolVersion != pluginv2.ProtocolVersion {
			return errors.New("插件运行时信息与已校验清单不一致")
		}
		expected := make([]pluginv2.Capability, 0, len(i.Manifest.Capabilities))
		for _, capability := range i.Manifest.Capabilities {
			expected = append(expected, capability.ExtensionCapability())
		}
		expected, err = pluginv2.NormalizeCapabilities(expected)
		if err != nil {
			return err
		}
		actual, err := pluginv2.NormalizeCapabilities(info.Capabilities)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(expected, actual) {
			return errors.New("插件能力或权限与已校验清单不一致")
		}
		r.extension = api
		for _, capability := range actual {
			if capability.ID == pluginv2.CapabilityProtectionTransport {
				transport, ok := dispensed.(pluginv2.TransportClient)
				if !ok {
					return errors.New("插件未实现保护传输客户端")
				}
				r.transport = transport
			}
		}
		return nil
	}
	transportClient, ok := dispensed.(*pluginv1.TransportClient)
	if !ok || transportClient.TransportPluginClient == nil {
		return errors.New("插件未实现传输 gRPC 客户端")
	}
	api := transportClient.TransportPluginClient
	info, err := api.GetInfo(ctx, &pluginv1.GetInfoRequest{})
	if err != nil {
		return fmt.Errorf("读取插件信息: %w", err)
	}
	if info == nil || info.PluginId != i.PluginKey || info.PluginVersion != i.Version ||
		info.ProtocolVersion != pluginv1.ProtocolVersion || info.TransportApiVersion != pluginv1.TransportAPIVersion {
		return errors.New("插件运行时信息与已校验清单不一致")
	}
	r.api = api
	return nil
}

func (r *pluginRuntime) health(ctx context.Context) (*pluginv1.HealthResponse, error) {
	if r.extension == nil {
		return r.api.Health(ctx, &pluginv1.HealthRequest{})
	}
	result, err := r.extension.Health(ctx)
	return &pluginv1.HealthResponse{Healthy: result.Healthy, Message: result.Message}, safeExtensionRPCError("扩展健康检查失败", err)
}

// status is the passive runtime status used by the admin read-only endpoint.
// v1 plugins may expose status_json directly; v2 extensions retain their
// validated health/message contract and are adapted without probing upstream.
func (r *pluginRuntime) status(ctx context.Context) (*pluginv1.HealthResponse, error) {
	if r == nil || r.client == nil || r.client.Exited() || (r.api == nil && r.extension == nil) {
		return nil, errors.New("插件进程已退出")
	}
	if r.api != nil {
		result, err := r.api.Health(ctx, &pluginv1.HealthRequest{})
		if err != nil {
			return nil, fmt.Errorf("插件状态查询失败: %w", err)
		}
		if result == nil {
			return nil, errors.New("插件未返回状态")
		}
		return result, nil
	}
	result, err := r.extension.Health(ctx)
	if err != nil {
		return nil, safeExtensionRPCError("扩展状态查询失败", err)
	}
	return &pluginv1.HealthResponse{Healthy: result.Healthy, Message: result.Message}, nil
}
func (r *pluginRuntime) validateConfig(ctx context.Context, config []byte) (*pluginv1.ValidateConfigResponse, error) {
	if r.extension == nil {
		return r.api.ValidateConfig(ctx, &pluginv1.ValidateConfigRequest{ConfigJson: config})
	}
	result, err := r.extension.ValidateConfig(ctx, config)
	return &pluginv1.ValidateConfigResponse{Valid: err == nil, NormalizedConfigJson: result}, safeExtensionRPCError("扩展配置校验失败", err)
}
func (r *pluginRuntime) applyConfig(ctx context.Context, config []byte) (*pluginv1.ApplyConfigResponse, error) {
	if r.extension == nil {
		return r.api.ApplyConfig(ctx, &pluginv1.ApplyConfigRequest{ConfigJson: config})
	}
	err := r.extension.ApplyConfig(ctx, config)
	return &pluginv1.ApplyConfigResponse{Applied: err == nil}, safeExtensionRPCError("扩展配置应用失败", err)
}
func (r *pluginRuntime) testConfig(ctx context.Context, config []byte) (*pluginv1.TestConfigResponse, error) {
	if r.extension == nil {
		return r.api.TestConfig(ctx, &pluginv1.TestConfigRequest{ConfigJson: config})
	}
	latency, err := r.extension.TestConfig(ctx, config)
	return &pluginv1.TestConfigResponse{Success: err == nil, LatencyMs: latency.Milliseconds()}, safeExtensionRPCError("扩展配置测试失败", err)
}
