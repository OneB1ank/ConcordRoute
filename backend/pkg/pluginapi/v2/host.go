package pluginv2

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2/wire"
	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const HostAPIVersion = 1

type SecretValue struct {
	Value     []byte
	ExpiresAt time.Time
}
type HostClient interface {
	Log(context.Context, string, string, string) error
	Metric(context.Context, string, string, float64) error
	Config(context.Context, string) (json.RawMessage, error)
	Secret(context.Context, string, string) (SecretValue, error)
	Publish(context.Context, string, string, int64) error
}

// HostAware is optional. Plugins requesting Host API permissions implement it.
type HostAware interface{ SetHost(HostClient) error }

// HostConnector is implemented by the host-side go-plugin client adapter.
type HostConnector interface {
	AttachHost(context.Context, wire.HostServicesServer) error
}

func (c *handlerClient) AttachHost(ctx context.Context, server wire.HostServicesServer) error {
	if c.broker == nil {
		return errors.New("host API requires a go-plugin broker")
	}
	if !c.hostAttached.CompareAndSwap(false, true) {
		return errors.New("host API already attached")
	}
	id := c.broker.NextId()
	go c.broker.AcceptAndServe(id, func(options []grpc.ServerOption) *grpc.Server {
		options = append(options, grpc.MaxRecvMsgSize(8192), grpc.MaxSendMsgSize(MaxRPCBytes), grpc.MaxConcurrentStreams(16))
		hostServer := grpc.NewServer(options...)
		wire.RegisterHostServicesServer(hostServer, server)
		return hostServer
	})
	result, err := c.api.AttachHost(ctx, &wire.AttachHostRequest{BrokerId: id, HostApiVersion: HostAPIVersion})
	if err != nil {
		return err
	}
	if !result.GetApplied() {
		return errors.New("plugin rejected Host API")
	}
	return nil
}

func (s *handlerServer) AttachHost(ctx context.Context, r *wire.AttachHostRequest) (*wire.ApplyResponse, error) {
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	if s.hostConnection != nil {
		return nil, status.Error(codes.AlreadyExists, "Host API already attached")
	}
	if r.GetHostApiVersion() != HostAPIVersion || r.GetBrokerId() == 0 || s.broker == nil {
		return nil, status.Error(codes.FailedPrecondition, "unsupported Host API attachment")
	}
	aware, ok := s.handler.(HostAware)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "plugin does not implement HostAware")
	}
	connection, err := s.broker.Dial(r.BrokerId)
	if err != nil {
		return nil, err
	}
	client := &hostClient{api: wire.NewHostServicesClient(connection)}
	if err = aware.SetHost(client); err != nil {
		_ = connection.Close()
		return nil, status.Error(codes.FailedPrecondition, "plugin rejected Host API")
	}
	s.hostConnection = connection
	return &wire.ApplyResponse{Applied: true}, nil
}

type hostClient struct{ api wire.HostServicesClient }

func boundedHostCall(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 2*time.Second)
}
func (h *hostClient) Log(ctx context.Context, capability, level, code string) error {
	ctx, cancel := boundedHostCall(ctx)
	defer cancel()
	_, err := h.api.Log(ctx, &wire.HostLogRequest{Capability: capability, Level: level, Code: code})
	return err
}
func (h *hostClient) Metric(ctx context.Context, capability, name string, value float64) error {
	ctx, cancel := boundedHostCall(ctx)
	defer cancel()
	_, err := h.api.Metric(ctx, &wire.HostMetricRequest{Capability: capability, Name: name, Value: value})
	return err
}
func (h *hostClient) Config(ctx context.Context, capability string) (json.RawMessage, error) {
	ctx, cancel := boundedHostCall(ctx)
	defer cancel()
	result, err := h.api.ReadConfig(ctx, &wire.HostCapabilityRequest{Capability: capability}, grpc.MaxCallRecvMsgSize(MaxRPCBytes))
	if err != nil {
		return nil, err
	}
	return result.ConfigJson, nil
}
func (h *hostClient) Secret(ctx context.Context, capability, alias string) (SecretValue, error) {
	ctx, cancel := boundedHostCall(ctx)
	defer cancel()
	result, err := h.api.ReadSecret(ctx, &wire.HostSecretRequest{Capability: capability, Alias: alias})
	if err != nil {
		return SecretValue{}, err
	}
	return SecretValue{Value: result.Value, ExpiresAt: time.UnixMilli(result.ExpiresUnixMillis)}, nil
}
func (h *hostClient) Publish(ctx context.Context, capability, name string, value int64) error {
	ctx, cancel := boundedHostCall(ctx)
	defer cancel()
	_, err := h.api.PublishEvent(ctx, &wire.HostEventRequest{Capability: capability, Name: name, Value: value})
	return err
}

// Broker references stay internal to the SDK; the plugin sees only HostClient.
type hostServerState struct {
	broker         *hcplugin.GRPCBroker
	hostMu         sync.Mutex
	hostConnection *grpc.ClientConn
}
