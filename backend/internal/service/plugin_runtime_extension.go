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
	api, ok := dispensed.(pluginv1.TransportPluginClient)
	if !ok {
		return errors.New("插件未实现传输 gRPC 客户端")
	}
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
	return &pluginv1.HealthResponse{Healthy: result.Healthy, Message: "扩展健康检查未通过"}, safeExtensionRPCError("扩展健康检查失败", err)
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
