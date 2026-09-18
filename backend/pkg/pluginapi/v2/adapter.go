package pluginv2

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2/wire"
	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MaxRPCBytes includes the bounded body plus protobuf/context overhead.
const MaxRPCBytes = MaxRequestBodyBytes + 128*1024

func capabilityToWire(c Capability) *wire.Capability {
	permissions := make([]string, len(c.Permissions))
	for i, permission := range c.Permissions {
		permissions[i] = string(permission)
	}
	return &wire.Capability{Id: c.ID, Kind: string(c.Kind), Platform: c.Platform, AccountType: c.AccountType,
		Permissions: permissions, TimeoutMs: c.TimeoutMS, FailureMode: string(c.FailureMode), Synchronous: c.Synchronous}
}

func capabilityFromWire(c *wire.Capability) Capability {
	permissions := make([]Permission, len(c.GetPermissions()))
	for i, permission := range c.GetPermissions() {
		permissions[i] = Permission(permission)
	}
	return Capability{ID: c.GetId(), Kind: CapabilityKind(c.GetKind()), Platform: c.GetPlatform(), AccountType: c.GetAccountType(),
		Permissions: permissions, TimeoutMS: c.GetTimeoutMs(), FailureMode: FailureMode(c.GetFailureMode()), Synchronous: c.GetSynchronous()}
}

func headersToWire(h map[string][]string) map[string]*wire.HeaderValues {
	out := make(map[string]*wire.HeaderValues, len(h))
	for k, v := range h {
		out[k] = &wire.HeaderValues{Values: append([]string(nil), v...)}
	}
	return out
}
func headersFromWire(h map[string]*wire.HeaderValues) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v.GetValues()...)
	}
	return out
}
func requestToWire(r PreprocessRequest) *wire.PreprocessRequest {
	c := r.Context
	return &wire.PreprocessRequest{Capability: r.Capability, BodyJson: r.BodyJSON, Context: &wire.RequestContext{
		RequestId: c.RequestID, TraceId: c.TraceID, DeadlineUnixMillis: c.Deadline.UnixMilli(), Platform: c.Platform,
		AccountType: c.AccountType, AccountId: c.AccountID, UserId: c.UserID, GroupId: c.GroupID,
		Method: c.Method, Path: c.Path, Host: c.Host, Model: c.Model, Headers: headersToWire(c.Headers),
	}}
}
func requestFromWire(r *wire.PreprocessRequest) PreprocessRequest {
	c := r.GetContext()
	return PreprocessRequest{Capability: r.GetCapability(), BodyJSON: r.GetBodyJson(), Context: RequestContext{
		RequestID: c.GetRequestId(), TraceID: c.GetTraceId(), Deadline: time.UnixMilli(c.GetDeadlineUnixMillis()),
		Platform: c.GetPlatform(), AccountType: c.GetAccountType(), AccountID: c.GetAccountId(), UserID: c.GetUserId(),
		GroupID: c.GetGroupId(), Method: c.GetMethod(), Path: c.GetPath(), Host: c.GetHost(), Model: c.GetModel(),
		Headers: headersFromWire(c.GetHeaders()),
	}}
}
func responseToWire(r PreprocessResponse) *wire.PreprocessResponse {
	out := &wire.PreprocessResponse{Decision: string(r.Decision), Code: r.Code, Reason: r.Reason}
	if p := r.Patch; p != nil {
		out.Patch = &wire.RequestPatch{Method: p.Method, Path: p.Path, Headers: headersToWire(p.Headers), BodyJson: p.BodyJSON, BodyChanged: p.BodyChanged}
	}
	return out
}
func responseFromWire(r *wire.PreprocessResponse) PreprocessResponse {
	out := PreprocessResponse{Decision: Decision(r.GetDecision()), Code: r.GetCode(), Reason: r.GetReason()}
	if p := r.GetPatch(); p != nil {
		out.Patch = &RequestPatch{Method: p.Method, Path: p.Path, Headers: headersFromWire(p.Headers), BodyJSON: p.BodyJson, BodyChanged: p.BodyChanged}
	}
	return out
}

// NewClient adapts the generated wire client to the validated public contract.
func NewClient(conn grpc.ClientConnInterface) ExtensionHandler {
	return &handlerClient{api: wire.NewExtensionPluginClient(conn)}
}

type handlerClient struct {
	api          wire.ExtensionPluginClient
	transport    wire.ProtectionTransportClient
	broker       *hcplugin.GRPCBroker
	hostAttached atomic.Bool
}

func (c *handlerClient) GetInfo(ctx context.Context) (PluginInfo, error) {
	r, err := c.api.GetInfo(ctx, &wire.GetInfoRequest{})
	if err != nil {
		return PluginInfo{}, err
	}
	out := PluginInfo{PluginID: r.GetPluginId(), PluginVersion: r.GetPluginVersion(), ProtocolVersion: r.GetProtocolVersion()}
	for _, cap := range r.GetCapabilities() {
		out.Capabilities = append(out.Capabilities, capabilityFromWire(cap))
	}
	return out, out.Validate()
}
func (c *handlerClient) Health(ctx context.Context) (HealthStatus, error) {
	r, err := c.api.Health(ctx, &wire.HealthRequest{})
	return HealthStatus{Healthy: r.GetHealthy(), Message: r.GetMessage()}, err
}
func (c *handlerClient) ValidateConfig(ctx context.Context, config json.RawMessage) (json.RawMessage, error) {
	r, err := c.api.ValidateConfig(ctx, &wire.ConfigRequest{ConfigJson: config}, grpc.MaxCallRecvMsgSize(MaxRPCBytes))
	if err != nil {
		return nil, err
	}
	if !r.GetValid() {
		return nil, errors.New(r.GetMessage())
	}
	if len(r.NormalizedConfigJson) == 0 {
		return config, nil
	}
	return r.NormalizedConfigJson, nil
}
func (c *handlerClient) ApplyConfig(ctx context.Context, config json.RawMessage) error {
	r, err := c.api.ApplyConfig(ctx, &wire.ConfigRequest{ConfigJson: config})
	if err != nil {
		return err
	}
	if !r.GetApplied() {
		return errors.New(r.GetMessage())
	}
	return nil
}
func (c *handlerClient) TestConfig(ctx context.Context, config json.RawMessage) (time.Duration, error) {
	r, err := c.api.TestConfig(ctx, &wire.ConfigRequest{ConfigJson: config})
	if err != nil {
		return 0, err
	}
	if !r.GetSuccess() {
		return 0, errors.New(r.GetMessage())
	}
	return time.Duration(r.LatencyMs) * time.Millisecond, nil
}
func (c *handlerClient) Preprocess(ctx context.Context, request PreprocessRequest) (PreprocessResponse, error) {
	if err := request.Validate(); err != nil {
		return PreprocessResponse{}, err
	}
	r, err := c.api.Preprocess(ctx, requestToWire(request), grpc.MaxCallRecvMsgSize(MaxRPCBytes), grpc.MaxCallSendMsgSize(MaxRPCBytes))
	if err != nil {
		return PreprocessResponse{}, err
	}
	out := responseFromWire(r)
	return out, out.Validate()
}

// NewServer exposes an ExtensionHandler through the versioned protobuf service.
func NewServer(handler ExtensionHandler) wire.ExtensionPluginServer {
	return &handlerServer{handler: handler}
}

type handlerServer struct {
	wire.UnimplementedExtensionPluginServer
	hostServerState
	handler ExtensionHandler
}

func (s *handlerServer) GetInfo(ctx context.Context, _ *wire.GetInfoRequest) (*wire.GetInfoResponse, error) {
	info, err := s.handler.GetInfo(ctx)
	if err != nil {
		return nil, err
	}
	if err := info.Validate(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &wire.GetInfoResponse{PluginId: info.PluginID, PluginVersion: info.PluginVersion, ProtocolVersion: info.ProtocolVersion}
	for _, cap := range info.Capabilities {
		out.Capabilities = append(out.Capabilities, capabilityToWire(cap))
	}
	return out, nil
}
func (s *handlerServer) Health(ctx context.Context, _ *wire.HealthRequest) (*wire.HealthResponse, error) {
	r, err := s.handler.Health(ctx)
	return &wire.HealthResponse{Healthy: r.Healthy, Message: r.Message}, err
}
func (s *handlerServer) ValidateConfig(ctx context.Context, r *wire.ConfigRequest) (*wire.ConfigResponse, error) {
	config, err := s.handler.ValidateConfig(ctx, r.GetConfigJson())
	if err != nil {
		return &wire.ConfigResponse{Message: err.Error()}, nil
	}
	return &wire.ConfigResponse{Valid: true, NormalizedConfigJson: config}, nil
}
func (s *handlerServer) ApplyConfig(ctx context.Context, r *wire.ConfigRequest) (*wire.ApplyResponse, error) {
	if err := s.handler.ApplyConfig(ctx, r.GetConfigJson()); err != nil {
		return &wire.ApplyResponse{Message: err.Error()}, nil
	}
	return &wire.ApplyResponse{Applied: true}, nil
}
func (s *handlerServer) TestConfig(ctx context.Context, r *wire.ConfigRequest) (*wire.TestResponse, error) {
	latency, err := s.handler.TestConfig(ctx, r.GetConfigJson())
	if err != nil {
		return &wire.TestResponse{Message: err.Error()}, nil
	}
	return &wire.TestResponse{Success: true, LatencyMs: latency.Milliseconds()}, nil
}
func (s *handlerServer) Preprocess(ctx context.Context, r *wire.PreprocessRequest) (*wire.PreprocessResponse, error) {
	if r.GetContext() == nil || r.GetContext().GetDeadlineUnixMillis() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "missing request context deadline")
	}
	request := requestFromWire(r)
	if err := request.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	callCtx, cancel := context.WithDeadline(ctx, request.Context.Deadline)
	defer cancel()
	if err := callCtx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	result, err := s.handler.Preprocess(callCtx, request)
	if err != nil {
		return nil, err
	}
	if err := result.Validate(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return responseToWire(result), nil
}
