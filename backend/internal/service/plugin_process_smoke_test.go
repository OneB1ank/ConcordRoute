package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	pluginv2 "github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2"
	"github.com/stretchr/testify/require"
)

// 编译并启动仓库内的最小插件，验证真实 gRPC 握手、配置、健康检查和停止。
// 仅操作临时目录和本地进程，不连接业务数据库或真实上游。
func TestPluginProcessSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("真实插件进程测试")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	binary := filepath.Join(root, "preprocess")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./pkg/pluginapi/examples/preprocess")
	build.Dir = filepath.Join("..", "..")
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	data, err := os.ReadFile(binary)
	require.NoError(t, err)
	hash := sha256.Sum256(data)
	manifest := testPluginManifest(nil)
	manifest.SchemaVersion = 2
	manifest.ID = "example.request-policy"
	manifest.Requires.PluginProtocol = int(pluginv2.ProtocolVersion)
	manifest.Requires.TransportAPI = 0
	manifest.Requires.ExtensionAPI = 1
	manifest.Capabilities = []PluginCapability{{
		ID: pluginv2.CapabilityRequestPreprocess, Kind: pluginv2.CapabilityKindHook,
		Platform: PlatformOpenAI, AccountType: AccountTypeOAuth,
		Permissions: []pluginv2.Permission{pluginv2.PermissionRequestMetadata, pluginv2.PermissionRequestBody, pluginv2.PermissionRequestMutate},
		TimeoutMS:   200, FailureMode: pluginv2.FailureModeClosed, Synchronous: true,
	}}
	installation := &PluginInstallation{
		ID: 1, PluginKey: manifest.ID, Version: manifest.Version, Manifest: manifest,
		BinaryPath: binary, BinarySHA256: hex.EncodeToString(hash[:]),
	}
	socketDir := filepath.Join(root, "sockets")
	require.NoError(t, os.MkdirAll(socketDir, 0o700))
	process, err := startPluginRuntime(ctx, installation, 10*time.Second, socketDir)
	require.NoError(t, err)
	t.Cleanup(process.kill)
	require.NoError(t, process.checkHealth(ctx))
	require.NoError(t, process.validateAndApplyConfig(ctx, []byte(`{"max_output_tokens":25}`)))
	result, err := process.testConfig(ctx, []byte(`{"max_output_tokens":25}`))
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Error(t, process.validateAndApplyConfig(ctx, []byte(`{"max_output_tokens":0}`)))
	require.NoError(t, process.checkHealth(ctx))
	process.kill()
	require.True(t, process.client.Exited())
	require.Error(t, process.checkHealth(ctx))
}
