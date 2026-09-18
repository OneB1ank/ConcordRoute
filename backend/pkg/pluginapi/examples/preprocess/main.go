// Example request.preprocess.v1 plugin. Build from the backend module:
// go build -o preprocess.exe ./pkg/pluginapi/examples/preprocess
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"time"

	pluginv2 "github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2"
)

type config struct {
	MaxOutputTokens int64  `json:"max_output_tokens"`
	DenyModel       string `json:"deny_model"`
	// DelayMS exercises deadlines in a local development installation.
	DelayMS int64 `json:"delay_ms"`
}
type policy struct{ config atomic.Pointer[config] }

// Override with -ldflags "-X main.pluginVersion=0.1.1" when building an upgrade.
var pluginVersion = "0.1.0"

// Keep this equal to the package's -account-type (oauth or apikey).
var accountType = "oauth"

func main() {
	p := &policy{}
	p.config.Store(&config{MaxOutputTokens: 1024})
	pluginv2.Serve(p)
}
func (p *policy) GetInfo(context.Context) (pluginv2.PluginInfo, error) {
	return pluginv2.PluginInfo{PluginID: "example.request-policy", PluginVersion: pluginVersion, ProtocolVersion: pluginv2.ProtocolVersion,
		Capabilities: []pluginv2.Capability{{
			ID: pluginv2.CapabilityRequestPreprocess, Kind: pluginv2.CapabilityKindHook, Platform: "openai", AccountType: accountType,
			Permissions: []pluginv2.Permission{pluginv2.PermissionRequestMetadata, pluginv2.PermissionRequestBody, pluginv2.PermissionRequestMutate},
			TimeoutMS:   200, FailureMode: pluginv2.FailureModeClosed, Synchronous: true,
		}}}, nil
}
func (p *policy) Health(context.Context) (pluginv2.HealthStatus, error) {
	return pluginv2.HealthStatus{Healthy: true}, nil
}
func parseConfig(raw json.RawMessage) (*config, error) {
	cfg := &config{MaxOutputTokens: 1024}
	if len(raw) == 0 || raw[0] != '{' {
		return nil, errors.New("configuration must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("expected one JSON object")
	}
	if cfg.MaxOutputTokens < 1 || cfg.MaxOutputTokens > 1000000 {
		return nil, errors.New("max_output_tokens must be between 1 and 1000000")
	}
	if cfg.DelayMS < 0 || cfg.DelayMS > 5000 {
		return nil, errors.New("delay_ms must be between 0 and 5000")
	}
	if len(cfg.DenyModel) > 128 {
		return nil, errors.New("deny_model is too long")
	}
	return cfg, nil
}
func (p *policy) ValidateConfig(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cfg)
}
func (p *policy) ApplyConfig(_ context.Context, raw json.RawMessage) error {
	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}
	p.config.Store(cfg)
	return nil
}
func (p *policy) TestConfig(_ context.Context, raw json.RawMessage) (time.Duration, error) {
	start := time.Now()
	_, err := parseConfig(raw)
	return time.Since(start), err
}
func (p *policy) Preprocess(ctx context.Context, request pluginv2.PreprocessRequest) (pluginv2.PreprocessResponse, error) {
	cfg := p.config.Load()
	if cfg.DelayMS > 0 {
		timer := time.NewTimer(time.Duration(cfg.DelayMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return pluginv2.PreprocessResponse{}, ctx.Err()
		case <-timer.C:
		}
	}
	if cfg.DenyModel != "" && cfg.DenyModel == request.Context.Model {
		return pluginv2.PreprocessResponse{Decision: pluginv2.DecisionDeny, Reason: "Model blocked by the configured policy"}, nil
	}
	if len(request.BodyJSON) == 0 {
		return pluginv2.PreprocessResponse{Decision: pluginv2.DecisionPass}, nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(request.BodyJSON, &body); err != nil {
		return pluginv2.PreprocessResponse{}, err
	}
	field := "max_output_tokens"
	if request.Context.Path == "/v1/chat/completions" {
		field = "max_tokens"
	}
	var requested int64
	if raw, exists := body[field]; exists {
		if err := json.Unmarshal(raw, &requested); err != nil {
			return pluginv2.PreprocessResponse{}, err
		}
		if requested > 0 && requested <= cfg.MaxOutputTokens {
			return pluginv2.PreprocessResponse{Decision: pluginv2.DecisionPass}, nil
		}
	}
	body[field], _ = json.Marshal(cfg.MaxOutputTokens)
	updated, err := json.Marshal(body)
	if err != nil {
		return pluginv2.PreprocessResponse{}, err
	}
	return pluginv2.PreprocessResponse{Decision: pluginv2.DecisionModify, Patch: &pluginv2.RequestPatch{BodyChanged: true, BodyJSON: updated}}, nil
}
