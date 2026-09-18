package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	pluginv2 "github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2"
	"github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2/wire"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type PluginHostEvent struct {
	Sequence   uint64    `json:"sequence"`
	Capability string    `json:"capability"`
	Name       string    `json:"name"`
	Value      int64     `json:"value"`
	Time       time.Time `json:"time"`
}
type PluginHostSnapshot struct {
	Logs           uint64             `json:"logs"`
	Metrics        map[string]float64 `json:"metrics"`
	EventsAccepted uint64             `json:"events_accepted"`
	EventsDropped  uint64             `json:"events_dropped"`
	RecentEvents   []PluginHostEvent  `json:"recent_events"`
}
type pluginHostConfig struct{ data []byte }
type pluginHostServices struct {
	wire.UnimplementedHostServicesServer
	pluginID    int64
	permissions map[string]map[pluginv2.Permission]bool
	config      atomic.Pointer[pluginHostConfig]
	readSecret  func(context.Context, string, string) (pluginv2.SecretValue, error)
	mu          sync.Mutex
	window      time.Time
	calls       int
	stats       PluginHostSnapshot
	events      chan PluginHostEvent
	stop        chan struct{}
	done        chan struct{}
	closed      atomic.Bool
}

func newPluginHostServices(i *PluginInstallation, readSecret func(context.Context, string, string) (pluginv2.SecretValue, error)) *pluginHostServices {
	h := &pluginHostServices{pluginID: i.ID, permissions: map[string]map[pluginv2.Permission]bool{}, readSecret: readSecret,
		stats: PluginHostSnapshot{Metrics: map[string]float64{}}, events: make(chan PluginHostEvent, 64), stop: make(chan struct{}), done: make(chan struct{})}
	for _, cap := range i.Manifest.Capabilities {
		h.permissions[cap.ID] = map[pluginv2.Permission]bool{}
		for _, permission := range cap.Permissions {
			h.permissions[cap.ID][permission] = true
		}
	}
	go h.consumeEvents()
	return h
}
func hasHostPermissions(manifest PluginManifest) bool {
	for _, cap := range manifest.Capabilities {
		for _, permission := range cap.Permissions {
			switch permission {
			case pluginv2.PermissionHostLog, pluginv2.PermissionHostMetric, pluginv2.PermissionHostConfig, pluginv2.PermissionSecretBroker, pluginv2.PermissionEventPublish:
				return true
			}
		}
	}
	return false
}
func (h *pluginHostServices) Close() {
	if h.closed.CompareAndSwap(false, true) {
		close(h.stop)
		select {
		case <-h.done:
		case <-time.After(time.Second):
		}
	}
}
func (h *pluginHostServices) authorize(ctx context.Context, capability string, permission pluginv2.Permission) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	if h.closed.Load() {
		return status.Error(codes.Unavailable, "host service stopped")
	}
	if !h.permissions[capability][permission] {
		return status.Error(codes.PermissionDenied, "capability permission not granted")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if now.Sub(h.window) >= time.Second {
		h.window = now
		h.calls = 0
	}
	if h.calls >= 100 {
		return status.Error(codes.ResourceExhausted, "host service rate limit reached")
	}
	h.calls++
	return nil
}
func validHostEventCode(code string) bool {
	switch code {
	case "policy.pass", "policy.deny", "config.applied", "plugin.ready", "plugin.error":
		return true
	}
	return false
}
func (h *pluginHostServices) Log(ctx context.Context, r *wire.HostLogRequest) (*wire.HostAck, error) {
	if err := h.authorize(ctx, r.GetCapability(), pluginv2.PermissionHostLog); err != nil {
		return nil, err
	}
	if !validHostEventCode(r.GetCode()) {
		return nil, status.Error(codes.InvalidArgument, "unknown log code")
	}
	level := slog.LevelInfo
	switch r.GetLevel() {
	case "info":
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown log level")
	}
	// The plugin cannot supply a log format, message body, headers, or fields.
	slog.Log(ctx, level, "plugin_host_log", "plugin_id", h.pluginID, "capability", r.Capability, "code", r.Code)
	h.mu.Lock()
	h.stats.Logs++
	h.mu.Unlock()
	return &wire.HostAck{Accepted: true}, nil
}
func (h *pluginHostServices) Metric(ctx context.Context, r *wire.HostMetricRequest) (*wire.HostAck, error) {
	if err := h.authorize(ctx, r.GetCapability(), pluginv2.PermissionHostMetric); err != nil {
		return nil, err
	}
	switch r.GetName() {
	case "requests", "denied", "errors", "latency_ms":
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown metric")
	}
	if math.IsNaN(r.Value) || math.IsInf(r.Value, 0) || r.Value < 0 || r.Value > 1e6 || (r.Name != "latency_ms" && math.Trunc(r.Value) != r.Value) {
		return nil, status.Error(codes.InvalidArgument, "metric value is invalid")
	}
	h.mu.Lock()
	h.stats.Metrics[r.Name] += r.Value
	h.mu.Unlock()
	return &wire.HostAck{Accepted: true}, nil
}
func (h *pluginHostServices) ReadConfig(ctx context.Context, r *wire.HostCapabilityRequest) (*wire.HostConfigResponse, error) {
	if err := h.authorize(ctx, r.GetCapability(), pluginv2.PermissionHostConfig); err != nil {
		return nil, err
	}
	cfg := h.config.Load()
	if cfg == nil {
		return nil, status.Error(codes.FailedPrecondition, "configuration has not been applied")
	}
	return &wire.HostConfigResponse{ConfigJson: append([]byte(nil), cfg.data...)}, nil
}
func (h *pluginHostServices) setConfig(raw json.RawMessage) {
	h.config.Store(&pluginHostConfig{data: append([]byte(nil), raw...)})
}
func (h *pluginHostServices) ReadSecret(ctx context.Context, r *wire.HostSecretRequest) (*wire.HostSecretResponse, error) {
	if err := h.authorize(ctx, r.GetCapability(), pluginv2.PermissionSecretBroker); err != nil {
		return nil, err
	}
	if !pluginSecretAliasPattern.MatchString(r.GetAlias()) || h.readSecret == nil {
		return nil, status.Error(codes.PermissionDenied, "secret alias not granted")
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	secret, err := h.readSecret(callCtx, r.Capability, r.Alias)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "secret alias not granted or expired")
	}
	if h.closed.Load() || !time.Now().Before(secret.ExpiresAt) || len(secret.Value) == 0 || len(secret.Value) > pluginSecretMaxBytes {
		return nil, status.Error(codes.PermissionDenied, "secret grant unavailable")
	}
	return &wire.HostSecretResponse{Value: secret.Value, ExpiresUnixMillis: secret.ExpiresAt.UnixMilli()}, nil
}
func (h *pluginHostServices) PublishEvent(ctx context.Context, r *wire.HostEventRequest) (*wire.HostAck, error) {
	if err := h.authorize(ctx, r.GetCapability(), pluginv2.PermissionEventPublish); err != nil {
		return nil, err
	}
	if !validHostEventCode(r.GetName()) || r.Value < 0 || r.Value > 1000000 {
		return nil, status.Error(codes.InvalidArgument, "invalid event")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	event := PluginHostEvent{Sequence: h.stats.EventsAccepted + 1, Capability: r.Capability, Name: r.Name, Value: r.Value, Time: time.Now()}
	select {
	case h.events <- event:
		h.stats.EventsAccepted++
		return &wire.HostAck{Accepted: true}, nil
	default:
		h.stats.EventsDropped++
		return nil, status.Error(codes.ResourceExhausted, "host event queue is full")
	}
}
func (h *pluginHostServices) consumeEvents() {
	defer close(h.done)
	for {
		select {
		case <-h.stop:
			return
		case event := <-h.events:
			h.mu.Lock()
			if len(h.stats.RecentEvents) == 64 {
				copy(h.stats.RecentEvents, h.stats.RecentEvents[1:])
				h.stats.RecentEvents = h.stats.RecentEvents[:63]
			}
			h.stats.RecentEvents = append(h.stats.RecentEvents, event)
			h.mu.Unlock()
			slog.Info("plugin_host_event", "plugin_id", h.pluginID, "capability", event.Capability, "event", event.Name, "value", event.Value)
		}
	}
}
func (h *pluginHostServices) Snapshot() PluginHostSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.stats
	out.Metrics = map[string]float64{}
	for name, value := range h.stats.Metrics {
		out.Metrics[name] = value
	}
	out.RecentEvents = append([]PluginHostEvent(nil), h.stats.RecentEvents...)
	return out
}

func (m *PluginManager) HostStats(ctx context.Context, id int64) (PluginHostSnapshot, error) {
	if _, err := m.repo.GetByID(ctx, id); err != nil {
		return PluginHostSnapshot{}, err
	}
	m.mu.Lock()
	process := m.runtimes[id]
	m.mu.Unlock()
	if process == nil || process.host == nil {
		return PluginHostSnapshot{Metrics: map[string]float64{}, RecentEvents: []PluginHostEvent{}}, nil
	}
	return process.host.Snapshot(), nil
}
