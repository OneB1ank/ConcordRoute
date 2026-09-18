package pluginv2

import (
	"context"

	"github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2/wire"
	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

const (
	// HandshakeProtocolVersion is separate from the v1 transport handshake.
	HandshakeProtocolVersion = 2
	ExtensionPluginName      = "extension"
)

var HandshakeConfig = hcplugin.HandshakeConfig{
	ProtocolVersion:  HandshakeProtocolVersion,
	MagicCookieKey:   "SUB2API_PLUGIN_MAGIC_COOKIE",
	MagicCookieValue: "sub2api-plugin-v2",
}

type GRPCPlugin struct {
	hcplugin.NetRPCUnsupportedPlugin
	Impl wire.ExtensionPluginServer
}

func (p *GRPCPlugin) GRPCServer(broker *hcplugin.GRPCBroker, server *grpc.Server) error {
	if adapter, ok := p.Impl.(*handlerServer); ok {
		adapter.broker = broker
	}
	wire.RegisterExtensionPluginServer(server, p.Impl)
	if adapter, ok := p.Impl.(*handlerServer); ok {
		if transport, ok := adapter.handler.(TransportHandler); ok {
			wire.RegisterProtectionTransportServer(server, &transportServer{handler: transport})
		}
	}
	return nil
}

func (p *GRPCPlugin) GRPCClient(_ context.Context, broker *hcplugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	return &handlerClient{api: wire.NewExtensionPluginClient(conn), transport: wire.NewProtectionTransportClient(conn), broker: broker}, nil
}

func ClientPluginMap() map[string]hcplugin.Plugin {
	return map[string]hcplugin.Plugin{ExtensionPluginName: &GRPCPlugin{}}
}

func Serve(impl ExtensionHandler) {
	hcplugin.Serve(&hcplugin.ServeConfig{
		HandshakeConfig: HandshakeConfig,
		Plugins:         map[string]hcplugin.Plugin{ExtensionPluginName: &GRPCPlugin{Impl: NewServer(impl)}},
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			return grpc.NewServer(append(opts, grpc.MaxRecvMsgSize(MaxRPCBytes), grpc.MaxSendMsgSize(MaxRPCBytes))...)
		},
	})
}
