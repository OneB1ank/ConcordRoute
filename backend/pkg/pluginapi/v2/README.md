# Sub2API 通用用户态插件接口 v2

`v2` 提供独立进程请求预处理能力，与 `v1` OpenAI OAuth 出站传输插件并行运行。宿主已接入签名安装、配置、健康检查、能力路由与 OpenAI HTTP 出站 Hook。

首个可运行示例见 [请求策略插件](../examples/preprocess/README.md)。普通进程仍具有宿主服务账号的文件和网络权限；以下字段权限限制 RPC 数据传输。需要系统级限制时启用 [v2 容器隔离](../docs/sandbox.md)。

生产路由、计费、真实浏览器和 v1 子进程回归的复现步骤见 [测试指南](../docs/testing.md)。

## 能力模型

插件通过 `Capability` 声明可以提供的能力。每个能力具有独立 ID、类型、权限、超时和失败策略。宿主应先校验声明，再根据能力 ID 和请求作用域建立路由。

首个同步能力是 `request.preprocess.v1`：

1. 宿主发送经过脱敏的 `RequestContext` 和可选 JSON 请求体；
2. 插件返回 `pass`、`modify`、`deny` 或 `error`；
3. `modify` 只能通过 `RequestPatch` 修改允许的请求字段；
4. 认证、账号选择、计费、持久化和最终重试决策仍由宿主负责。

请求预处理插件不得接收 `authorization`、`proxy-authorization`、`cookie` 或 `set-cookie` 请求头。确实需要秘密时，使用已实现的 Host API `secrets.broker` 权限及管理员按实例授权的短期别名，详见 [Host API](../docs/host-api.md)。

另有独立的 [账号保护传输能力](../docs/protection-transport.md) `openai.oauth.protection_transport.v1`，用于身份/TLS 传输插件。它显式声明凭据转发、网络和原请求权限，通过可选流式 RPC 接管当前 OpenAI OAuth 请求；原预处理白名单不变。

## 协议文件

- [`extension.proto`](./extension.proto)：gRPC 服务和线协议定义；
- [`contract.go`](./contract.go)：宿主和插件适配器可以直接依赖的 Go 类型、验证器和接口；
- [`manifest.schema.json`](./manifest.schema.json)：通用能力清单的 v2 Schema；
- [`contract_test.go`](./contract_test.go)：能力、敏感字段和决策语义的契约测试。
- [`host.go`](./host.go)：可选 HostAware/HostClient 接口，边界和授权见 [Host API](../docs/host-api.md)。

生成代码位于 `wire/`，与 `contract.go` 的公共 Go 类型分包，`adapter.go` 负责转换和验证。Go 插件实现 `ExtensionHandler` 后调用 `pluginv2.Serve(handler)`。

生成器锁定为 protoc 28.3、protoc-gen-go v1.36.11、protoc-gen-go-grpc v1.6.2。三者位于 PATH 时，在本目录执行：

```powershell
./generate.ps1
```

Linux/macOS 可在 `backend` 中使用同版本工具：

```bash
protoc --proto_path=pkg/pluginapi/v2 --go_out=pkg/pluginapi/v2/wire --go_opt=paths=source_relative --go-grpc_out=pkg/pluginapi/v2/wire --go-grpc_opt=paths=source_relative pkg/pluginapi/v2/extension.proto
```

## 宿主执行边界

预处理能力 `request.preprocess.v1` 的作用域必须为 `platform=openai`，账号类型为 `oauth` 或 `apikey`，清单单次超时 1–5000ms。每个能力实例默认最多同时调用 32 次，可配置为 1–256；连续 3 次失败后熔断 10 秒。计数是实例内状态，重启后归零。

调用发生在账号选择与 HTTP 请求准备完成后，匹配 POST Responses、Responses Compact 和 Chat Completions 出站请求；WebSocket 命中账号走现有 HTTP Bridge。账号灰度使用稳定分桶。每次上游尝试独立预处理，始终从该次原始请求创建副本，避免重试累计修改。

| 权限 | 传输或修改范围 |
| --- | --- |
| `request.metadata.read`（必需） | request_id、经过校验的宿主 trace_id、截止时间、平台、账号类型/ID、认证用户/分组 ID、方法、路径、主机名；仅 Content-Type/Accept 请求头 |
| `request.body.read` | 最大 4MiB 的 JSON 请求体（可能包含用户输入），以及从请求体提取的模型名 |
| `request.mutate` | `x-sub2api-extension-*` 请求头；配合 body.read 修改指定生成参数 |

请求体可修改 `temperature`、`top_p`、`max_tokens`、`max_output_tokens`、`presence_penalty`、`frequency_penalty`、`seed`、`stop`、`instructions`。宿主验证类型和范围。其他字段，包括模型、stream、service_tier、工具、消息、账号/路由身份，必须保持不变；端点和方法不可改变。模型与路由改写需要后续新增选择前 Hook，不能通过这里绕过计费和权限判断。

`deny` 始终拒绝请求。`error`、超时、非法结果、熔断及进程不可用按清单 `fail_open` / `fail_closed` 决定保留原请求或拒绝；调用方取消始终终止。核心返回固定的扩展错误，不回显插件原因文本、不把拒绝记为上游账号认证失败、不因此切换账号。日志记录插件 ID、能力、宿主 request_id、决策与耗时，不记录提示词、令牌或插件错误文本。

现有管理接口返回清单中的能力属性，以及 `capability_runtime` 的调用/失败/拒绝数、并发数、并发上限和熔断状态。能力清单使用现有 JSONB；热升级增加 `239_plugin_version_history.sql`，细粒度路由增加 `240_plugin_capability_routing.sql`。

## 配置路由策略

v2 管理卡片的“路由策略”支持以下字段。v1 传输仍保持同平台/账号类型单插件独占；v2 允许多个已启用插件匹配同一能力。

| 字段 | 语义 |
| --- | --- |
| `priority` | -1000 至 1000，越大越先匹配；相同优先级按插件安装 ID 升序 |
| `account_ids`、`user_ids`、`group_ids` | 每种最多 1000 个正整数，空列表表示不限；三类条件取交集 |
| `rollout_percent` | 0–100，按账号 ID 稳定分桶；0 保持进程启用但不接收该能力流量 |
| `max_concurrency` | 1–256，默认 32；调低时已在途请求继续，新的调用遵守新上限 |
| `timeout_ms` | 0 跟随清单，否则必须小于等于清单声明的超时 |

用户和分组 ID 在 API Key 认证成功后写入专用上下文，调度 fallback/composite 修改工作分组不会改变插件使用的认证分组。客户端请求体和请求头不能提供或覆盖这些身份。

只调用第一个匹配的插件；它返回 pass/error/deny 后不继续执行低优先级插件。调用失败按该插件清单策略处理。缺少认证用户/分组时，不匹配限制了对应身份的路由。数据库无法读取绑定状态时，未确认的 OpenAI 预处理路径失败关闭，避免启动期间绕过可能已启用的策略。

管理接口 `PUT /api/v1/admin/plugins/:id/routing` 接收 `policies` 数组及最近读取的 `expected_updated_at`。每个策略包含能力 ID 和上述字段，不允许改动清单能力或权限。保存需二次验证；版本戳过期时拒绝覆盖，更新在数据库事务中完成，实例通过对齐循环同步。

## 热升级与回滚

管理页“升级版本”上传相同插件 ID 的新包。新包必须保持清单协议及能力、权限、作用域契约一致。宿主复验签名和哈希、启动候选进程、校验运行时身份、应用配置并检查健康，随后在数据库事务中保存旧版本并切换。旧进程继续完成正在处理的请求，最长排空 10 秒；新请求使用新进程。

“版本记录”列出最近 5 个可回滚快照，回滚窗口 24 小时。快照包括原始签名包和加密配置；回滚复验包并恢复当时的插件配置，保留当前路由策略。过期快照不再可用，数据库清理在后续版本切换时执行。卸载插件会级联删除其版本记录。

| 管理接口 | 请求 |
| --- | --- |
| `GET /api/v1/admin/plugins/:id/versions` | 返回快照版本、保留期限和兼容性，不返回配置或二进制 |
| `POST /api/v1/admin/plugins/:id/upgrade` | Multipart：`plugin` 文件、`accept_untested` 布尔值 |
| `POST /api/v1/admin/plugins/:id/rollback` | JSON：`version_id` 正整数、`accept_untested` 布尔值 |

升级和回滚沿用管理员二次验证。候选验证失败不替换当前版本；并发配置或状态变更使数据库原子切换失败。多实例独立恢复目标版本；若目标包在某个实例启动失败，只在旧版本仍健康且权限、作用域与绑定保持一致时继续运行旧版本。管理接口的 `runtime_version` 和状态信息展示实际运行版本。

## 兼容策略

- `backend/pkg/pluginapi/v1` 继续服务现有 `TransportPlugin` 插件；
- `v2` 能力按能力 ID 单独版本化，例如 `request.preprocess.v1`；
- 宿主遇到未知能力时应标记为不兼容，而不是仅凭清单字段启用；
- 后续 Provider、事件和后台任务能力尚未注册，声明它们的包会显示不兼容。
