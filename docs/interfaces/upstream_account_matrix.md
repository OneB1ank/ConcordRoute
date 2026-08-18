# 上游账号能力矩阵

本文统一记录 ConcordRoute 六个平台、七类账号和公开网关协议的当前支持边界。它是账号能力的路由入口，不替代各平台专题中的认证、转换、限流和诊断细节，也不把数据导入器能够保存的历史组合视为正式支持。

## 章节导航

- [判定口径](#判定口径)：理解矩阵中的支持等级。
- [账号支持矩阵](#账号支持矩阵)：核对平台和账号类型组合。
- [公开网关协议](#公开网关协议)：从入口路由到平台专题。
- [跨层约束](#跨层约束)：判断账号为何创建成功但仍不可调度。
- [已确认冲突](#已确认冲突)：查看尚未形成完整运行契约的组合。

## 判定口径

后端常量定义六个平台 `anthropic`、`openai`、`gemini`、`antigravity`、`grok`、`qoder`，以及七类账号 `oauth`、`setup-token`、`apikey`、`upstream`、`bedrock`、`service_account`、`cosy`。矩阵使用以下等级：

- **正式支持**：管理端有创建或授权流程，平台运行时也有对应凭据、转发和维护契约。
- **兼容保留**：通用创建/导入层可以保存，或旧运行路径仍会识别，但管理端不推荐该组合；不能据此推导完整平台能力。
- **不支持**：创建校验明确拒绝，或该类型被限定给另一个平台。
- **契约冲突**：管理端与运行时对同一组合的类型解释不同；在实现统一前不作为正式支持承诺。

通用数据导入器除 Qoder/COSY 的双向限制外，会接受多种历史组合。这只是迁移兼容性；真正的可调度性仍由平台 token provider、协议处理器、账号状态、模型和 endpoint 能力共同决定。

## 账号支持矩阵

| 平台 | OAuth | Setup Token | API Key | Upstream | Bedrock | Service Account | Cosy |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Anthropic | 正式支持 | 正式支持 | 正式支持 | 兼容导入，无正式转发契约 | 正式支持 | 正式支持（Vertex AI） | 不支持 |
| OpenAI | 正式支持 | 兼容导入，无正式转发契约 | 正式支持 | 兼容导入，无正式转发契约 | 兼容导入，无正式转发契约 | 兼容导入，无正式转发契约 | 不支持 |
| Gemini | 正式支持 | 兼容导入，无正式转发契约 | 正式支持 | 兼容导入，无正式转发契约 | 兼容导入，无正式转发契约 | 正式支持（Vertex AI） | 不支持 |
| Antigravity | 正式支持 | 兼容导入，无正式转发契约 | 契约冲突，见下文 | 兼容保留（旧 Claude 直连） | 兼容导入，无正式转发契约 | 兼容导入，无正式转发契约 | 不支持 |
| Grok | 正式支持 | 兼容导入，无正式转发契约 | 正式支持 | 兼容导入，无正式转发契约 | 兼容导入，无正式转发契约 | 兼容导入，无正式转发契约 | 不支持 |
| Qoder | 不支持 | 不支持 | 不支持 | 不支持 | 不支持 | 不支持 | 正式支持 |

平台专题：

- [Anthropic 上游](anthropic_upstream.md)
- [OpenAI 上游](openai_upstream.md)
- [Gemini 上游](gemini_upstream.md)
- [Antigravity 上游](antigravity_upstream.md)
- [Grok / xAI 上游](grok_upstream.md)
- [Qoder 原生上游](qoder_upstream.md)

<a id="public_gateway_protocols"></a>
## 公开网关协议

文本生成协议是否可进入处理器由分组的 `allowed_client_protocols` 控制。支持集合、新建默认值和迁移值如下；默认值只决定初始选择，所有协议都可关闭：

| 上游平台 | 支持协议 | 新建默认 | 已有分组迁移值 |
| --- | --- | --- | --- |
| Anthropic | Messages、Responses、Chat | Messages | 三项全部启用 |
| OpenAI | Messages、Responses、Chat | Responses、Chat | Responses、Chat，加上旧开关允许的 Messages |
| Gemini | Messages、Responses、Chat、Gemini GenerateContent | Gemini GenerateContent | 四项全部启用 |
| Antigravity | Messages、Responses、Chat、Gemini GenerateContent | Messages、Gemini GenerateContent | 四项全部启用 |
| Qoder | Messages、Responses、Chat | 空集合 | 三项全部启用 |
| Grok | Messages、Responses、Chat | Responses、Chat | 三项全部启用 |

集合顺序固定为 Messages、Responses、Chat、Gemini，空集合对所有平台都合法。准入只控制文本生成协议；Live、WebSocket、Embedding、图片和视频继续使用独立能力规则。

| 协议族或入口 | 当前平台边界 | 专题路由 |
| --- | --- | --- |
| Anthropic Messages：`/v1/messages` | 六个平台均有平台分派；最终分组允许 Messages 时按平台转换或原生转发 | 六个平台专题；共同链路见[网关请求生命周期](../architecture/gateway_request_lifecycle.md) |
| Anthropic token count：`/v1/messages/count_tokens`、`/messages/count_tokens` | Anthropic、OpenAI、Gemini 进入各自统计路径，Grok 使用本地估算；Antigravity、Qoder 明确返回 `404`，Anthropic Bedrock 账号也不支持 | 六个平台专题；客户端应保留本地估算回退 |
| OpenAI Responses：`/v1/responses`、`/responses` 及允许的子路径 | 六个平台在最终分组允许 Responses 时进入平台适配；Qoder 不支持 Responses 子路径和 WebSocket | 六个平台专题；WebSocket/Realtime 重点见 [OpenAI 上游](openai_upstream.md) |
| OpenAI Chat Completions：`/v1/chat/completions`、`/chat/completions` | 最终分组允许 Chat 时，六个平台均按平台转换或原生转发 | 六个平台专题 |
| 模型与用量：`/v1/models`、`/models`、`/v1/usage` | 按 Key、分组、账号和渠道解析可请求模型与本地额度；不是上游模型列表或账单的原样代理 | [模型目录与市场](model_catalog_and_marketplace.md)及各平台专题 |
| Embeddings：`/v1/embeddings`、`/embeddings` | 仅 OpenAI 分组 | [OpenAI 上游](openai_upstream.md) |
| Realtime、Live 与 Alpha Search | Live/sideband、Codex realtime 和 alpha search 仅 OpenAI 平台；是否可用还受分组和账号能力限制 | [OpenAI 上游](openai_upstream.md) |
| 同步图片生成/编辑 | 仅 OpenAI 与 Grok；分组图片开关和账号能力继续收窄范围 | [OpenAI 上游](openai_upstream.md)、[Grok / xAI 上游](grok_upstream.md) |
| 批量图片作业 | Gemini/Vertex 使用独立任务生命周期；供应商范围由批量图片领域契约定义 | [批量图片作业](../domains/batch_image_jobs.md) |
| 视频生成、编辑、扩展、查询和下载 | 新任务仅 Grok；复合 Key 可凭持久任务绑定查询既有任务 | [Grok / xAI 上游](grok_upstream.md) |
| Gemini v1beta：`/v1beta/models/*` | Gemini/Antigravity 分组允许 Gemini 协议时承接生成、流式生成和 token 统计；模型列表 GET 不受开关影响 | [Gemini 上游](gemini_upstream.md)、[Antigravity 上游](antigravity_upstream.md) |
| Antigravity 专用入口：`/antigravity/*` | 强制只选择 Antigravity 账号，不参与混合调度 | [Antigravity 上游](antigravity_upstream.md) |

路由存在不代表任意分组或账号类型都能承接。协议门禁在账号选择前按最终分组执行；通过后，处理器仍会校验平台、模型、transport、endpoint capability、媒体资格和其它分组策略。Gemini Responses 已有正式非流和 SSE 转换，保留 reasoning、工具调用、usage、结束原因与首次 Token 指标；首个客户端字节写出后不再 failover。

公开协议不再包含 Key 账单自省或上游声明倍率入口。`GET /v1/sub2api/billing` 未注册并返回普通 `404`；账户本地 `rate_multiplier` 和渠道的上游计费模型来源仍属于结算配置，不代表从上游探测到的声明倍率。

## 跨层约束

一个账号能够承接请求，需要同时满足：

1. 平台与账号类型有实际 token/签名实现，而不只是导入器接受字段。
2. 账号 active、schedulable、未过期、未处于账号或模型限流期，并属于目标分组。
3. 分组允许对应协议或媒体能力，渠道和账号都能解析最终模型。
4. OAuth-only、隐私状态、客户端限制、transport capability 和站点/区域等平台策略通过。
5. 并发槽、等待队列和粘性约束允许本次选择。

账号选择和快照一致性见[账号调度与缓存一致性](../architecture/account_scheduling_and_cache.md)，分组/渠道策略见[网关策略控制](../domains/gateway_policy_controls.md)，凭据和健康恢复见[账号维护](../operations/account_maintenance.md)。

## 已确认冲突

Antigravity 管理端把“静态上游”表单保存为 `type=apikey`，但当前 Antigravity Claude 直连和 token provider 的静态分支只识别历史 `type=upstream`；OpenAI Chat/Responses 兼容路径又明确要求原生 OAuth。两者不能被描述为等价账号类型。本轮只记录该冲突，不修改运行行为；在代码和契约测试统一前，新的 Antigravity 静态账号不应被视为完整正式支持。

相关文档：[接口目录](index.md)、[账号维护](../operations/account_maintenance.md)、[上游传输安全](../operations/upstream_transport_security.md)。
