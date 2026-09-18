# v2 受控宿主服务

Host API 通过 go-plugin multiplex broker 复用已认证的进程连接。在容器模式中不需要开放网络接口。每个服务实例绑定一个已验证的插件安装 ID；请求中没有安装 ID、环境变量名或数据库查询参数。

## 插件如何接入

账号保护插件使用新增的 `openai.oauth.protection_transport.v1` 能力，详见[保护传输接口](protection-transport.md)。该能力显式授权当前出站请求的凭据转发和网络访问；普通 `request.preprocess.v1` 的脱敏边界不变。它只能在 process 模式运行，无网络容器不会自动放开网络。

插件在对应能力的 `permissions` 中声明需要的权限，并实现 `pluginv2.HostAware.SetHost(HostClient)`。宿主验证运行时能力与签名清单一致后连接 Host API。配置尚未成功 Apply 时，读取配置返回 FailedPrecondition。

完整可执行代码位于 [host-aware 示例](../examples/host-aware/main.go)。它依次发布固定日志代码、累计请求指标、读取已应用配置、按别名读取秘密、发布事件；从不将秘密值放入响应或日志。

| 权限 | SDK 方法 | 边界 |
| --- | --- | --- |
| `host.log` | `Log(ctx, capability, level, code)` | level 仅 info/warn/error；固定代码，没有任意日志格式或正文 |
| `host.metric` | `Metric(ctx, capability, name, value)` | 仅 requests/denied/errors/latency_ms；无自定义标签；数值必须有限且在 0–1000000 |
| `host.config.read` | `Config(ctx, capability)` | 只返回本实例已通过 Validate/Apply 的配置副本 |
| `secrets.broker` | `Secret(ctx, capability, alias)` | 精确匹配安装 ID、能力和管理员授权别名，且未过期 |
| `events.publish` | `Publish(ctx, capability, name, value)` | 固定事件代码和 0–1000000 整数，不接受任意 JSON 负载 |

日志/事件固定代码：`policy.pass`、`policy.deny`、`config.applied`、`plugin.ready`、`plugin.error`。

宿主不将插件提供的错误正文写入宿主日志或管理员错误；v2 错误只保留宿主操作名及标准 RPC 状态。Host API 日志由宿主构造结构化字段，以防秘密或格式注入。

## 限额与事件语义

每个运行实例每秒最多 100 次授权 Host API 调用，gRPC 最多 16 个并发流，输入消息上限 8 KiB。SDK 调用截止时间不超过 2 秒；秘密存储查询不超过 1 秒。

事件使用 64 槽有界队列，满时立即返回 ResourceExhausted，不等待主请求释放。宿主异步写固定结构事件日志，并保留最近 64 条供管理界面查看。事件和统计是实例内、非持久化遥测；不能用作计费账本或可靠任务队列。容量拒绝可以重试，网络结果不明确时重试可能产生重复事件。

## 秘密授权

迁移 `241_plugin_host_secret_grants.sql` 保存单独的秘密授权表。值通过现有 AES-256-GCM 加密器保存；加密载荷还绑定用途、安装 ID、能力、别名和到期时间，不能跨别名或用途搬用密文。

管理页“宿主服务”只显示授权别名和有效期。保存、替换和撤销均经过管理员二次验证；保存后清空浏览器输入。秘密值不会由列表接口回显，秘密写入和插件配置请求体整体不进入操作审计日志。

每个安装实例最多 32 个有效授权，值最多 16 KiB，可读取窗口为 1–3600 秒。过期后读取立即拒绝，管理器周期性删除过期密文。撤销立即阻止之后的读取。

**读取窗口不是外部凭据的撤销机制。** 插件已经收到的值无法收回；应提供由签发方保证实际寿命的短期凭据，必要时向签发方撤销。Host API 不读取任意环境变量，不提供 Repository、数据库连接或任意账号 Token。

## 管理接口

| 路径 | 用途 |
| --- | --- |
| `GET /api/v1/admin/plugins/:id/host` | 当前实例的日志数、指标、事件计数和最近事件 |
| `GET /api/v1/admin/plugins/:id/secret-grants` | 别名、能力和有效期，不包含值或密文 |
| `PUT /api/v1/admin/plugins/:id/secret-grants` | JSON：capability、alias、value、ttl_seconds；需要二次验证 |
| `DELETE /api/v1/admin/plugins/:id/secret-grants` | JSON：capability、alias；需要二次验证 |

别名使用小写字母开头，后续为小写字母、数字、下划线或连字符，最长 64 字符。

## 验证命令

在 `backend` 目录执行：

```powershell
go test ./internal/service ./internal/server/middleware -run 'TestPluginHostServices|TestPluginSecret|TestPluginExtensionRPCError' -count=1
go test -tags integration ./internal/repository -run '^TestPluginRepositorySecret' -count=1 -v -timeout=10m
$env:SUB2API_TEST_SANDBOX_CONTAINER = '1'
go test ./internal/service -run 'TestPluginHostAPI(Process|Container)Integration' -count=1 -v -timeout=5m
```

真实进程测试覆盖普通进程和容器内的双向 RPC、配置一致性、指标/事件发布、获授权秘密读取与撤销。PostgreSQL 测试使用实际 AES 加密器验证无明文存储、权限范围、过期清理、撤销及 32 个授权上限。
