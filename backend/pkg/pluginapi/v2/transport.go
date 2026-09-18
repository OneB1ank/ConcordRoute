package pluginv2

import (
	"context"
	"encoding/json"

	v1 "github.com/TokenFlux/TokenRouter/pkg/pluginapi/v1"
	"github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2/wire"
	"google.golang.org/grpc"
)

const CapabilityProtectionTransport = "openai.oauth.protection_transport.v1"
const PermissionCredentialsForward Permission = "request.credentials.forward"
const PermissionAccountProtection Permission = "account.protection.read"
const PermissionOriginalRequest Permission = "request.original.read"

// ProtectionAccount exposes only the protection-owned state, never credentials.
type ProtectionAccount struct {
	ID                        int64          `json:"id"`
	Platform                  string         `json:"platform"`
	Type                      string         `json:"type"`
	Concurrency               int            `json:"concurrency"`
	RandomProxy               bool           `json:"random_proxy"`
	Shadow                    bool           `json:"shadow"`
	Extra                     map[string]any `json:"extra"`
	MappedModel               string         `json:"mapped_model,omitempty"`
	DefaultInstructionsDigest string         `json:"default_instructions_digest,omitempty"`
}

func DecodeProtectionAccount(raw []byte) (ProtectionAccount, error) {
	var account ProtectionAccount
	err := json.Unmarshal(raw, &account)
	return account, err
}

// TransportHandler is optional; ordinary preprocess plugins do not implement it.
type TransportHandler interface {
	Forward(grpc.BidiStreamingServer[v1.ForwardRequest, v1.ForwardResponse]) error
}
type TransportClient interface {
	Forward(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[v1.ForwardRequest, v1.ForwardResponse], error)
}
type transportServer struct {
	wire.UnimplementedProtectionTransportServer
	handler TransportHandler
}

func (s *transportServer) Forward(stream grpc.BidiStreamingServer[v1.ForwardRequest, v1.ForwardResponse]) error {
	return s.handler.Forward(stream)
}
func (c *handlerClient) Forward(ctx context.Context, options ...grpc.CallOption) (grpc.BidiStreamingClient[v1.ForwardRequest, v1.ForwardResponse], error) {
	return c.transport.Forward(ctx, options...)
}
