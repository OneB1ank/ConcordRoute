// Host-aware sample used to verify the scoped reverse RPC channel.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	pluginv2 "github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2"
)

type sampleConfig struct {
	Alias        string `json:"alias"`
	SecretDigest string `json:"secret_digest"`
}
type sample struct {
	mu     sync.RWMutex
	host   pluginv2.HostClient
	config atomic.Pointer[sampleConfig]
}

func main() { s := &sample{}; s.config.Store(&sampleConfig{}); pluginv2.Serve(s) }
func (s *sample) SetHost(host pluginv2.HostClient) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.host = host
	return nil
}
func (*sample) GetInfo(context.Context) (pluginv2.PluginInfo, error) {
	return pluginv2.PluginInfo{PluginID: "example.host-services", PluginVersion: "0.1.0", ProtocolVersion: 2, Capabilities: []pluginv2.Capability{{
		ID: pluginv2.CapabilityRequestPreprocess, Kind: pluginv2.CapabilityKindHook, Platform: "openai", AccountType: "oauth",
		Permissions: []pluginv2.Permission{pluginv2.PermissionRequestMetadata, pluginv2.PermissionHostLog, pluginv2.PermissionHostMetric,
			pluginv2.PermissionHostConfig, pluginv2.PermissionSecretBroker, pluginv2.PermissionEventPublish},
		TimeoutMS: 1000, FailureMode: pluginv2.FailureModeClosed, Synchronous: true,
	}}}, nil
}
func (*sample) Health(context.Context) (pluginv2.HealthStatus, error) {
	return pluginv2.HealthStatus{Healthy: true}, nil
}
func (*sample) ValidateConfig(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var cfg sampleConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return json.Marshal(cfg)
}
func (s *sample) ApplyConfig(_ context.Context, raw json.RawMessage) error {
	var cfg sampleConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	s.config.Store(&cfg)
	return nil
}
func (*sample) TestConfig(context.Context, json.RawMessage) (time.Duration, error) { return 0, nil }
func (s *sample) Preprocess(ctx context.Context, _ pluginv2.PreprocessRequest) (pluginv2.PreprocessResponse, error) {
	s.mu.RLock()
	host := s.host
	s.mu.RUnlock()
	if host == nil {
		return pluginv2.PreprocessResponse{}, errors.New("host API not attached")
	}
	capability := pluginv2.CapabilityRequestPreprocess
	if err := host.Log(ctx, capability, "info", "plugin.ready"); err != nil {
		return pluginv2.PreprocessResponse{}, err
	}
	if err := host.Metric(ctx, capability, "requests", 1); err != nil {
		return pluginv2.PreprocessResponse{}, err
	}
	raw, err := host.Config(ctx, capability)
	if err != nil {
		return pluginv2.PreprocessResponse{}, err
	}
	var applied sampleConfig
	if json.Unmarshal(raw, &applied) != nil || applied != *s.config.Load() {
		return pluginv2.PreprocessResponse{}, errors.New("configuration mismatch")
	}
	if applied.Alias != "" {
		secret, err := host.Secret(ctx, capability, applied.Alias)
		if err != nil {
			return pluginv2.PreprocessResponse{Decision: pluginv2.DecisionDeny, Reason: "secret grant unavailable"}, nil
		}
		digest := sha256.Sum256(secret.Value)
		if hex.EncodeToString(digest[:]) != applied.SecretDigest || !time.Now().Before(secret.ExpiresAt) {
			return pluginv2.PreprocessResponse{Decision: pluginv2.DecisionDeny, Reason: "secret validation failed"}, nil
		}
	}
	if err := host.Publish(ctx, capability, "policy.pass", 1); err != nil {
		return pluginv2.PreprocessResponse{}, err
	}
	return pluginv2.PreprocessResponse{Decision: pluginv2.DecisionPass}, nil
}
