# Host API 示例运行时

本目录实现 `ExtensionHandler` 与 `HostAware`，演示按能力授权的宿主日志、指标、配置、秘密及事件调用。它用于真实进程和容器集成测试，完整接入说明见 [Host API](../../docs/host-api.md)。

在 `backend` 中构建：

```powershell
go build -o host-aware.exe ./pkg/pluginapi/examples/host-aware
go test ./internal/service -run '^TestPluginHostAPIProcessIntegration$' -count=1 -v
```

运行时由宿主启动，不应直接双击运行。集成测试会设置必要的握手、能力、Host API 和短期测试授权；无需真实外部凭据。

配置字段：

| 字段 | 用途 |
| --- | --- |
| `alias` | 为空则不读秘密；非空时通过 Host API 读取该别名 |
| `secret_digest` | 读取值的 SHA-256 十六进制，用于测试校验；示例不会输出秘密值 |

此示例的权限集合与普通请求策略示例不同，因此不能作为后者的热升级包。开发自己的包时，应把权限准确写入签名清单。
