# v2 账号保护传输

`openai.oauth.protection_transport.v1` 是可选、同步、失败关闭的 provider 能力，作用域固定为 OpenAI OAuth。它与 `request.preprocess.v1` 独立注册；现有 v1 协议仍可用。插件实现 `pluginv2.ExtensionHandler`、`pluginv2.HostAware` 和可选的 `pluginv2.TransportHandler`。

运行时由 `ProtectionTransport.Forward` 双向流承载，复用 `pluginapi/v1` 的稳定帧类型。新增的 start 字段只发送给已验签、清单与运行时能力一致的 v2 保护传输：

| 字段/权限 | 数据 |
| --- | --- |
| `request.metadata.read` | 账号 ID、平台、账号类型、方法、目标 |
| `request.body.read` | 已准备的出站请求体，插件限制最大 4 MiB |
| `request.credentials.forward` | 该次出站请求已有的认证头和代理参数；不提供账号的其它凭据 |
| `network.outbound` | 在普通进程模式中创建真实出站连接 |
| `account.protection.read` | 类型受限的保护字段、账号已有并发值、影子账号标记、宿主映射后的模型 |
| `request.original.read` | 当前账号尝试对应的语义检查基线，最大 4 MiB |

权限必须全部声明；只可另外声明 `host.log`、`host.metric`、`host.config.read`、`events.publish`。能力要求 `kind=provider`、`synchronous=true`、`fail_closed`，超时最多 120000 ms。普通预处理插件仍不能读取认证头、网络权限或这些新增字段。

原始基线绑定请求上下文和账号 ID，失败切换不得读取其它账号的基线。未知账号字段、token、完整模型映射、任意 `extra` 及旧快照不随账号元数据传递。宿主默认 instructions 只发送 SHA-256，用于比较默认追加内容。

当前普通 Responses、透传和 Chat Completions 路径在出站构造前刷新比较基线，WS 使用准备 HTTP Bridge 请求后的内容。这里的 original 指本次传输的比较基线，不承诺保留最初客户端请求；不能用它证明宿主全部转换均未损失入站语义。

路由使用 v2 的优先级、账号/用户/分组作用域和稳定灰度，选择第一个匹配保护传输。若同时启用旧 v1 传输，保护传输命中的请求优先，未命中继续既有路径。并发计数覆盖完整响应流；路由超时控制等待响应头，收到响应头后保留 SSE 流。进程退出或拒绝不回退到较低优先级传输。WebSocket 命中的账号走宿主 HTTP Bridge。

插件返回 `PROTECTION_DENIED` 或 `PROTECTION_BUSY` 时，宿主作为本地扩展策略错误处理，不标记为账号认证失败。插件发出真实 HTTP 请求后失败必须报告 `request_sent=true`，防止重复请求。插件不解析计费用量，宿主继续执行原有响应处理和计费。

`v2_sandbox.mode=container` 的网络隔离与该能力不兼容，启动明确拒绝。process 模式仍拥有服务账号操作系统权限，安装者必须信任签名发布者。

保护策略保存时宿主读取已持久化配置作为回滚依据，插件验证并归一化新配置后应用；数据库按安装 ID、二进制 SHA-256、原配置密文执行比较更新。竞争或写入失败恢复已持久化快照。持久化快照的重放调用 Apply，不重跑策略命令或递增修订号。

开发者使用公开 `v2/transport.go` 的 `TransportHandler` 实现 `Forward`，并复用 `v1` 的请求/响应帧类型。宿主不分发私有保护策略实现；SDK 与签名打包步骤见[插件开发指南](../../../../docs/PLUGIN_DEVELOPMENT.md)。保护传输实现需要额外验证取消、SSE、凭据透传、证书校验及 `request_sent`，不能直接把预处理示例的业务函数改名为 `Forward`。
