# 请求预处理插件示例

此示例运行在独立进程中，限制 OpenAI OAuth 的最大输出 token 数，并可拒绝一个指定模型。包自带配置 UI，通过宿主 UI Bridge 保存配置。

## 构建和打包

以下命令在完整仓库或公开 SDK 开发包的 `backend` 目录执行，需要 Go 1.27.0。推荐先按[正式版开发指南](../../../../../docs/PLUGIN_DEVELOPMENT.md)生成自己的发布者密钥并签名。输出文件必须尚不存在，打包器不会覆盖已有包。

```powershell
$keyPrefix = Join-Path $env:LOCALAPPDATA 'Sub2API-PluginKeys/preprocess-demo'
go run ./pkg/pluginapi/tools/keygen -out $keyPrefix
go build -o preprocess.exe ./pkg/pluginapi/examples/preprocess
go run ./pkg/pluginapi/examples/preprocess/pack -binary preprocess.exe -signing-key "$keyPrefix.private" -key-id preprocess-demo -out request-policy.s2plugin
```

Linux/macOS 将二进制输出名改为 `preprocess`。跨平台编译时，给打包器传相应 `-target`（例如 `linux-amd64`）；Windows 包内运行时保留 `.exe` 后缀。

默认作用于 OAuth。API Key 账号的测试包须同时修改二进制和清单的账号类型，宿主会核对两者：

```powershell
go build -ldflags "-X main.accountType=apikey" -o preprocess-apikey.exe ./pkg/pluginapi/examples/preprocess
go run ./pkg/pluginapi/examples/preprocess/pack -binary preprocess-apikey.exe -account-type apikey -signing-key "$keyPrefix.private" -key-id preprocess-demo -out request-policy-apikey.s2plugin
```

以上命令生成已编译、内置公钥的签名包。分享 `.s2plugin` 后，用户在正式桌面 0.3.0 首次导入时确认发布者即可安装，不需要 Go 或修改配置。可额外交付 `.public` 公钥供核对；私钥自己保管且不进入插件包。已有密钥时跳过 keygen。省略签名参数只会生成独立开发环境使用的未签名调试包。

安装后在“插件管理”查看权限、打开“配置”、保存并测试，再按账号灰度启用。示例清单未声明已测试发布版本，启用需要现有的未验证版本确认。

## 配置

| 字段 | 默认值 | 作用 |
| --- | --- | --- |
| `max_output_tokens` | 1024 | 将输出 token 上限限制到 1–1000000 内的配置值 |
| `deny_model` | 空字符串 | 拒绝模型名完全匹配的请求 |
| `delay_ms` | 0 | 开发用故障注入，最大 5000ms；清单调用超时为 200ms |

配置示例：

```json
{"max_output_tokens":512,"deny_model":"","delay_ms":0}
```

## 验证真实进程

```powershell
go test ./internal/service -run '^TestPluginExtensionProcessIntegration$' -count=1
```

此项需要完整宿主仓库，精简 SDK 开发包不包含 `internal/service`。测试会构建插件、生成临时签名包、验签安装、启动独立进程，向本机 HTTP 测试上游发送改写请求，再验证拒绝、超时和进程退出。无需提供外部插件、上游密钥或数据库；`go test -short` 会跳过此进程构建测试。

完整能力边界及失败语义见 [v2 协议说明](../../v2/README.md)。

## 构建一次升级

在 `backend` 目录同时设置运行时版本和包版本：

```powershell
go build -ldflags "-X main.pluginVersion=0.1.1" -o preprocess-0.1.1.exe ./pkg/pluginapi/examples/preprocess
go run ./pkg/pluginapi/examples/preprocess/pack -binary preprocess-0.1.1.exe -version 0.1.1 -signing-key "$keyPrefix.private" -key-id preprocess-demo -out request-policy-0.1.1.s2plugin
```

在管理页点击已安装插件的“升级版本”，选择新包。配置和能力契约验证通过后切换；“版本记录”可恢复旧包及其配置。测试包签名规则与首次安装一致。
