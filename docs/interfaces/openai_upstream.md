# OpenAI 上游

本文描述 OpenAI OAuth/API Key 账号，以及 Responses、Chat、Messages、Embeddings、Images、Realtime 和 Codex 兼容能力的当前契约。它不枚举会随上游变化的完整模型列表，也不把所有 OpenAI 形状的入口都解释为任意平台可用。

## 章节导航

- [账号与凭据](#账号与凭据)：修改 OAuth、API Key、隐私或客户端限制时读取。
- [协议与传输](#协议与传输)：修改 Responses、WebSocket、Realtime 或兼容转换时读取。
- [远程压缩协议](#远程压缩协议)：区分原生 `remote_compaction_v2` 与旧版 `/responses/compact` 时读取。
- [模型与能力](#模型与能力)：修改模型别名、endpoint capability 或推理参数时读取。
- [额度与调度](#额度与调度)：修改窗口配额、评分、粘性或自动暂停时读取。
- [失败与诊断](#失败与诊断)：修改错误分类、刷新、CAS 状态或 failover 时读取。

## 账号与凭据

OpenAI 正式支持 `oauth` 与 `apikey`。OAuth 账号保存 access/refresh token、账号/组织上下文和 Codex 能力元数据，后台与请求路径都可触发刷新；API Key 账号保存 key、base URL 和可探测的 endpoint capability。其它通用导入类型不构成 OpenAI 转发支持，详见[上游账号能力矩阵](upstream_account_matrix.md)。

OAuth 补全账号元数据时，ID token 中的个人 `chatgpt_plan_type` 是个人套餐的权威来源。`accounts/check` 可能按 access token 的 `poid` 命中另一个 workspace；仅当该记录的账号 ID 与个人 `chatgpt_account_id` 一致时，才能把它的 `entitlement.expires_at` 与个人套餐组合。账号不一致时，到期时间必须改从个人 `/backend-api/subscriptions` 的 `active_until` 获取；若套餐本身来自 `accounts/check`，套餐和到期时间仍保持来自同一条记录。

OAuth 账号可受 Codex CLI-only、允许客户端、agent identity、privacy status 和 OAuth passthrough 策略限制。隐私策略不是 OAuth 生命周期副作用：创建、编辑、复制、导入、令牌交换、令牌刷新和定时维护均不自动调用隐私设置接口；只有管理员显式调用账号管理端点才会写入 `privacy_mode`。OAuth 出站的 `originator` 必须与最终 User-Agent 首段配对；客户端未提供可识别官方身份或身份修复失败时统一回退 `codex-tui`，PAT、模型/额度探测、Alpha Search、HTTP 与 WebSocket 走同一默认身份。推理请求的 HTTP `version` 头由最终出站 UA 首段的引擎版本生成；`codex_version` 是元数据字段，不新增为独立 HTTP 头。网关实际收到的 `x-codex-turn-metadata`、`client_metadata` 及其内嵌 turn metadata 中，只有已存在且为字符串的 `codex_version` 才对齐同一引擎版本；缺失字段不补造，非法 JSON、异常字段类型保留原文。工具名、input、tools、工具输出文本、会话标识与缓存键均不因版本替换而改变。客户端直连 MCP 的调用不经过推理网关，因而不在此处理范围。WS 透传在首帧完成最终版本对齐后才保存重试快照；遇到 `previous_response_not_found` 自动恢复时，仅移除失效续链锚点，不重放改写前的旧版本。最终 UA 仍遵循已有的 TLS Router / 账号 / 全局身份选择规则；无效身份候选回退规范身份，低于当前兼容门槛的服务器默认身份会回退到 0.153.0。例如 `Codex Desktop/0.153.3 ... (Codex Desktop; 26.901.41123)` 对应引擎版本 `0.153.3`，而非桌面构建号 `26.901.41123`。客户端或 TLS 路由显式提供且可配对的官方身份继续保留，历史 `codex_cli_rs` 仍只作为兼容识别值。API Key 账号不应借用 OAuth-only 的内部端点或身份元数据。Header override、代理、base URL 和 TLS 配置属于出站安全边界，不能覆盖受保护认证头或绕过目标校验。

### 客户端 Attestation 协商

远程证明桥的两秒预算覆盖互斥排队与完整 RPC，等待写锁也服从剩余预算。
取消请求或预算耗尽按原有可选证明策略退出，业务请求保留原始上下文；
账号不匹配时先跳过，拿到锁后再次校验绑定，避免排队期间串用账号。
中继连接已声明的 session/thread 作用域必须逐项匹配，缺省其中一项不作为通配符；仅未限定作用域的唯一活动连接保留兼容匹配。

Codex app-server 的 `initialize.params.capabilities.requestAttestation=true` 会话由网关短时记录，并按 API Key、连接、账号、session/thread 隔离。客户端通过独立的 `GET /backend-api/codex/app-server` WebSocket 接入 JSON-RPC bridge；首帧必须是 `initialize`，随后上游请求由该 bridge 发出与 Codex 源码一致的 just-in-time `attestation/generate`：`{"id":N,"method":"attestation/generate","params":{}}`（原生省略 `jsonrpc`，入站兼容 `"2.0"`）。响应按 JSON-RPC ID 匹配，读取原生 `result.token`，旧接入端的 `result.headerValue` 仅在 token 字段缺失时兼容；显式空值、null、错误类型和双字段冲突均按 malformed 处理。不透明值只做类型/长度校验，不按 `v1.*` 前缀猜测真实性，再封装为 `{"v":1,"s":0,"t":"..."}`。超时、请求失败、取消和 malformed response 分别形成 `s=1/2/3/4`，不合成 token；Responses HTTP、Responses WebSocket 和 Live 创建都会复用同一条已协商连接。`app_server_attestation_transport` 是运行时字段：没有已完成协商的 bridge 时为 `false`，至少一条活动 bridge 完成 initialize 后为 `true`，全部断开后恢复 `false`。标准转发与 Live 保留真实客户端直接 Header 的兼容路径，检查客户端 UA/originator 配对和 envelope 格式后中继；这不等于设备证明已由网关验证为真。标准转发已绑定 app-server context 时不回退到直传 Header；Live 创建优先使用已协商 context，实际接受情况仍由对应上游请求确定。Linux 服务端不生成伪造 DeviceCheck 证明；配置真实 Linux helper 或接入真实 app-server bridge 后，才会启用对应服务端路径。Windows UA/TLS 模板只表示客户端身份与连接特征，不代表证明已经生成。证明状态仅短 TTL 保存在内存；Live Sideband 只保存加密后的会话 envelope，不写入账号凭据或普通日志。

管理员可显式开启 app-server 证明采集器：`POST /admin/codex-attestation-collector/start` 后创建 `POST /admin/codex-attestation-collector/sessions`，将返回的 `collector_token` 作为 `collector_token` 查询参数或 `X-Codex-Attestation-Collector-Token` 握手头附加到 app-server WebSocket。管理页面会同时显示完整 WebSocket 地址，并提供显式删除当前采集会话的操作。采集器只记录 `initialize` 能力、`attestation/generate` 状态、请求 ID、客户端版本、连接/session/thread 关联以及 proof 长度和 SHA-256；`GET /admin/codex-attestation-collector/sessions/:token/captures` 不返回 opaque proof 原文。会话默认 30 分钟、每会话最多 100 条，停止采集会立即清除内存摘要；采集 token 不是上游认证凭据，也不能写入账号、TLS 模板或全局配置。采集器只覆盖证明层；账号现有 Codex 收敛策略和 TLS 模板/路由继续由各自配置决定，不在采集器内切换。
管理页面入口位于“账号 → 更多操作 → 工具 → Codex app-server Attestation Collector”，采集器采用独立弹窗；TLS 指纹模板、TLS 路由器和 Codex 收敛仍保持独立配置。采集器只在管理员显式启动并创建会话后工作，不增加账号级自动证明开关。

OpenAI OAuth 账号的 `extra.codex_fingerprint_mode` 控制 Codex Responses 的设备指纹收敛。OAuth 导入模板的内置默认值为 `cockpit`，因此新导入账号会显式保存“会话+缓存键”模式；已有账号未配置、保存空值或包含无效值时，运行态统一按 `off` 处理，避免静默改变存量行为。`off` 保留既有转发行为，`device` 只统一 installation ID，`session` 进一步统一 session ID 并按客户端原始 session 稳定派生 thread ID，`cockpit` 在账号内按客户端 session/thread 分别映射对话身份，同时从请求头或请求体的 session、thread、window 和 `prompt_cache_key` 补充识别对话。客户端同时明确提供相等的 `session_id`、`thread_id` 且没有 `parent_thread_id`，并且 session/thread 两侧绑定都不存在时，新的根绑定只生成一次 UUIDv7，并把两个映射键指向该值；子线程继续使用独立 thread 映射并让 parent 指向共享根。只有一侧存在的旧绑定会保留已有值，并在另一命名空间独立建立缺失绑定；双侧已存在的历史绑定也保持原值，不迁移、不旋转既有窗口和缓存作用域。显式 `prompt_cache_key` 原样保留；字段明确存在但值为空时不视为缺省，不进入 carry/fallback；同一 session/thread/window 暂时省略时复用最近绑定；现代客户端压缩推进到相邻窗口且未带新键时，先查本窗口绑定，再单向继承同账号、同 session/thread 的直接前一窗口绑定。每个窗口独立保存，跳代、跨账号、跨线程及过期绑定不猜测继承，旧窗口重试不覆盖新窗口；新的显式 key 到达后切换缓存命名空间，且不反向改变 session/thread。`full` 再把所有客户端收敛到同一 thread。session/full 保留每请求生成 turn ID 和生命周期开始时间的历史行为；Cockpit 的 `turn_id`、`parent_turn_id`、`root_turn_id` 与 `turn_started_at_unix_ms` 使用客户端值，不建立账号级回合映射，也不把缺失字段从旧请求或 WS 前一帧回灌。HTTP 头、`client_metadata` 和内嵌 turn metadata 对同一请求使用同一组客户端回合值；HTTP 内部重试复用已准备的请求快照。普通转换与 OAuth passthrough 都遵守该配置，透传大 body 仅局部读取和改写身份字段，不做整包解码；旧版 `/responses/compact` 保持既有请求体协议，仅收敛 Header 中的设备、会话、线程和窗口字段，回合元数据仍保持客户端值。账号首次持久化时生成随机 `extra.codex_fingerprint_seed`；身份绑定读取或写入失败时请求不继续向上游发送，避免重启/多实例产生分叉映射，升级迁移为已有账号补齐该值，installation、session、thread 等出站身份由该持久化随机种子派生，显式缓存键仍原样保留，不再使用仅在单个数据库内唯一的自增账号 ID；管理员配置的真实 OpenAI device ID 仍具有最高优先级。普通编辑、批量编辑和运行态 Extra 更新不得覆盖种子，复制账号生成新种子，Spark 影子账号在普通 HTTP、OAuth passthrough 和原生 V2 探测中都动态使用父账号的模式、device ID 和稳定种子，不允许分裂同一 OAuth 凭据的上游设备身份。

现代 Codex（扩展回合身份门槛为 `0.151.0`）只处理客户端明确提供的 `root_turn_id`：缺失时始终保持缺失，不根据 `turn_id` 或 `parent_turn_id` 补全。Cockpit 的 `turn_id`、`parent_turn_id`、`root_turn_id` 保持客户端关联图和字面值；客户端 root 与当前 turn 相等时出站仍相等，子回合的 parent/root 仍引用客户端对应祖先。三个字段各自按客户端存在性处理，缺省当前 turn 也保留缺省。session/full 保留原有 root 兼容规则。WS 每帧独立判断 turn/root/parent 是否存在，即使该帧没有新的 `turn_id`，也不回灌上一帧的回合值。root/parent 只进入 `client_metadata`、内嵌 turn metadata 与兼容 Header；Responses 顶层 `root_turn_id`、`parent_turn_id`、`context_window_id`、`window_number`、`first_window_id`、`previous_window_id` 会先提取有效值再移除，避免误作生成参数。device 模式只迁移这些字段的载体，不改变其回合/窗口值；off 模式不增加改写。旧版客户端按版本门控移除扩展字段，剥离的 `window_number` 不参与窗口派生。

官方客户端内部压缩状态维护 `first_window_id`、`previous_window_id` 与 `window_number`：首窗口锚点不变，压缩后记录前一窗口并递增代数。它们不是 Responses 顶层生成参数。核对过的 0.153.4 源码中，`first_window_id` / `previous_window_id` 还用于客户端上下文片段与持久化状态，并非普通 turn metadata 的标准字段；本 fork 的同名元数据属于兼容扩展，不能据此宣称完整模拟官方压缩历史。网关保留账号隔离的内部窗口链；Cockpit 仅在客户端实际携带 `context_window_id`、`first_window_id`、`previous_window_id` 时写出对应映射。首窗口主动删除已携带的残留 `previous_window_id`；客户端上下文文本保持原样。`window_number` 的数值由客户端显式字段或窗口后缀提供，逐请求发送不会自动递增；Cockpit 未收到该字段时仅内部使用代数，不向元数据补造字段。`client_metadata` 的 `window_number` 使用字符串，内嵌/兼容头的 turn metadata 使用 JSON 数字。普通与透传路径统一接受精确范围内的整数、小数/指数整数及兼容数字字符串，超出 JSON 精确整数范围或非整数不参与派生。异常或 `null` 的内嵌 turn metadata 保留原文，不向 nil map 写入。

### 窗口实例与升级边界

Cockpit 在客户端提供 `context_window_id` 时，按账号、映射后的 thread 与原始窗口实例建立独立 UUIDv7 绑定，窗口代数不参与实例键。`0/W0 → 1/W1 → 0/W0 → 1/W2` 的 W1 与 W2 分别映射；回访 W0 恢复原绑定。显式 `first_window_id` / `previous_window_id` 使用同一实例域，而不是按第零/前一代猜测引用。缺省字段仍不补造；session/thread 映射保持不变，回合字段继续透传。WS 只改变 context UUID 时也刷新并持久化；缺省代数仅内部沿用连接位置，不补入当前帧。

旧 `codex-context-window` 绑定只记录代数，没有原始实例对应信息。新实例使用 `codex-context-instance:v2` 独立命名空间，不自动认领旧 UUID；升级后已有客户端实例首次建立新绑定，因此该窗口 UUID 会发生一次切换。没有实例字段的兼容请求继续使用旧代数绑定，旧记录按原 TTL/容量淘汰，不批量清空账号。回退旧二进制会恢复旧窗口策略，并不等于保持新实例语义。

携带实例的请求，其内部缓存 carry 同样按实例隔离；只在显式提供 `previous_window_id` 时继承对应前驱，不从相同代数的另一分支借键。缺少明确前驱且本实例没有绑定时走既有 session 默认键。只有代数的兼容请求维持既有相邻代数继承。显式缓存键（包括空字符串与首尾空白）不受此限制，原样发送；这项保守隔离可能减少缺省键请求的 carry，不能承诺缓存命中率完全不变。

<a id="openai_protocol_dispatch"></a>
## 协议与传输

非流式录音转文字使用独立的 `/v1/audio/transcriptions` 和 `/transcribe` 兼容入口，分组开关、时长计费、上游认证和桌面原生按钮的验收边界见[听写转录](audio_transcription.md)。听写不是 Live，也不参与推理会话收敛。

OpenAI 平台拥有以下正式协议族：

| 协议 | 处理边界 |
| --- | --- |
| Responses HTTP/SSE | 原生 OAuth/API Key 转发；支持允许的 `/responses/*` 子路径 |
| Responses WebSocket | 根据账号 transport capability 选择 WS 或兼容传输；连接建立后遵守流式不可换账号边界 |
| Chat Completions | 可原生转发或转换到 Responses；每次 attempt 重建协议状态；响应兼容 `reasoning` 推理别名 |
| Anthropic Messages | 转换到 OpenAI 请求并把事件、工具、thinking/usage 恢复为 Anthropic 形状 |
| Embeddings | 仅 OpenAI 分组，账号必须声明或探测到相应 endpoint capability |
| Images | OpenAI 图片生成/编辑；当前网关保留同步生命周期，批量图片由 Gemini/Vertex 专题定义 |
| Realtime/Live/sideband、Alpha Search | 仅 OpenAI 分组，并受分组开关、账号类型和 transport capability 限制 |

OpenAI 分组支持 Messages、Responses 和 Chat，新建时默认启用 Responses 与 Chat；三项都可关闭。已有分组迁移时仅在旧 `allow_messages_dispatch` 开启时加入 Messages。该旧字段只作为 Messages 的弃用兼容镜像，专用 `messages_dispatch_model_config` 仍只负责 Claude 到 GPT 模型映射；系列和精确映射都只在目标值非空时生效，全部留空时不执行分组层模型映射。Responses WebSocket 是 OpenAI/Grok 的原生传输能力，不因其它平台启用兼容 Responses 而开放。

OpenAI 兼容非流式响应的 usage 按 `usage`、`response.usage`、`data.usage`、`data.response.usage` 的顺序解析；前两条原生路径优先于 Cline 等兼容上游使用的 `data` envelope。同层的 hosted image usage 必须随对应路径读取，不能把不同 envelope 的 token 与图片用量混合。

`/backend-api/codex` 和无 `/v1` 别名服务特定客户端兼容，但仍经过 ConcordRoute Key 鉴权、分组准入、调度和结算。Responses WebSocket 不支持 Qoder；其它平台是否可进入 OpenAI 兼容处理器由路由和平台专题共同决定，不能仅凭 URL 推断。

<a id="openai_live_runtime"></a>
### Live 语音接入与验证边界

当前 Live 路由支持 `POST /v1/live` 或 `POST /backend-api/codex/realtime/calls` 创建 WebRTC 会话，以 `201 Created`、`application/sdp` 和网关同路径族的 Sideband `Location` 返回结果，以及相应 call ID 的 Sideband 控制连接；它不是默认 `GET /v1/live` 的纯音频 WebSocket 服务。创建响应不得改为200，否则严格遵循原生201契约的客户端会丢弃已创建会话的SDP。客户端验证必须明确选择匹配的 WebRTC transport，不能把 Responses SSE、证明 bridge 或 CLI 默认音频 WebSocket 当作同一种通道。

上游信令创建使用 ChatGPT 的 `/backend-api/codex/realtime/calls`，Frameless Sideband 则使用 `wss://api.openai.com/v1/live/{call_id}`，与核对的 Codex 原生默认路径一致；不得把 call ID 直接追加在 ChatGPT Codex 根路径后。两条连接都继续使用创建会话的账号凭据、代理、UA/TLS 规则；控制连接不重新调度到其它账号。

Codex app-server 的 `initialize.params.capabilities.experimentalApi` 与线程的 `features.realtime_conversation` 是不同开关；在核对的 0.153.4 中，仅启用 experimental API 并不会使线程具备 realtime 能力。该版本 v3 语音使用 audio 输出，text 输出只适用于 v2。排障先在隔离配置和本地假上游核对客户端请求是否真正到达 Live 创建路由，再验证有效账号、真实证明、SDP、控制连接和双向音频。网关的能力状态、单元测试或成功返回 RPC 确认均不替代真实通话验收；旧版本客户端的具体参数需按其自身协议核对。

Live 的证明按来源可选：存在协商客户端、可信直接 Header 或平台提供器时沿用原校验与加密中继；没有来源（Linux 未配置 helper、本机平台不支持或 macOS 未安装对应 App）时不设置 `x-oai-attestation`，由真实上游决定账号资格。已配置 helper 的执行故障、格式错误、客户端异常 envelope 和已有密文解密失败仍明确报错，不静默降级。创建时未携带证明的会话，其 Sideband 同样不设置该头；已有证明的会话继续使用加密快照，不迁移、不混用。平台是否能生成证明不等于 Live 传输是否可用，也不保证所有账号都接受无证明请求。

Live 请求一旦绑定 app-server context，就只使用该请求的协商结果；空 envelope、格式错误或账号归属冲突明确报错，不再转用入站 Header、平台 Provider 或无证明请求。协议定义的 `s=1/2/3/4` 是有效失败状态 envelope，仍原样表达生成失败，不当作成功证明，也不更换来源。未绑定任何通道的请求仍保留原有按来源可选策略。

网关保持 Linux 部署也可接入外部真实证明或客户端证明中继，服务器平台不是最终可用性结论。分组 `allow_live` 仅授予调用权限，仍受账号类型、计费、并发和上游资格限制。能力查询不生成证明、不探测账号、不产生模型调用，传输能力和证明来源分别报告，语义详见 [HTTP 接口边界](http_api.md)。
Live 创建仍尊重账号模型白名单；所请求语音模型必须获准，不能把文字模型白名单视为已包含 Live。调度返回无可用账号时响应 503 与明确的候选账号提示，不再误报为上游请求失败的 502。

Live 创建与 Sideband 的模型链仍执行 Key、渠道和账号的显式映射；最终 `gpt-live-*` 模型保留其语音模型名称，不因名称包含 `codex` 而套用文字模型兜底。测试须使用真实协议中的 `gpt-live-1-codex`，仅使用不含 `codex` 的虚构 Live 模型会遗漏这一边界。

Live 选择证明中继时，优先读取原始 `session-id`、`thread-id` Header；缺省时才兼容旧调用方放在 Session JSON 中的同名下划线字段。原生客户端的 `session.session_id` 可能属于独立语音会话，不应覆盖 Header 中的根会话/线程提示。这些提示只用于 API Key 下的中继连接匹配，不改写 Session JSON、出站 UA/TLS 或 Cockpit 映射，跨线程/跨 Key 仍隔离。

在核对的 0.153.4 源码与隔离客户端测试中，API Key 自定义 provider 即使声明 `requestAttestation=true`，也未发起 `attestation/generate`；其证明能力判定依赖 ChatGPT 认证模式。因此“打开能力位”不等于实际取得证明，独立运行 CLI 也不是设备证明提供器。这与无证明时正常发起 Live 请求是两个不同问题；不要通过伪造登录形状或成功状态完成验收。需要证明时仍须按客户端支持的登录和宿主能力接入，上游拒绝保持原有错误与准入处理。

独立 Windows 音频适配器可将桌面创建请求交给本地 helper，并使原生 app-server 的 ExistingCall Sideband 通过同一网关 Key、同一 callId 连接。它只转发已存在的真实证明，不实现设备证明生成。安装缺省不启用 Live；启用时以严格201/SDP/Location及 WebSocket 握手验证、真实媒体和关闭验收为门槛。客户端接线、短期能力和回滚边界见 [独立桌面适配组件](audio_transcription.md#desktop_audio_adapter)。

<a id="codex_identity_persistence"></a>
### 身份绑定持久化的等待与读取

启用身份映射时，准备等锁、同步生成和持久化共用账号级互斥域及最长 5 秒的 context 预算，
客户端已有更短期限或取消时优先响应。锁所有权仅放在本次同步操作的账号副本上，不进入调度缓存或存储；
退出前在锁内同步 Extra。取消不会释放另一个请求持有的锁，也不会让未确认持久化的请求快照继续向上游发送。
未启用映射的请求保留原有持久化和流式断连收尾生命周期，不因没有身份准备工作而新增取消拦截。
已有账号测试等探测的生成步骤也支持等锁取消，但不新增探测、不改变触发策略或落库策略。
生产仓储通过独立事务能力只加载账号 ID 和最新 Extra，不加载凭据、代理或分组关系；
账号行锁覆盖权威绑定恢复、UUID 派生、裁剪和写回。请求调度快照缺少绑定或本机热缓存为空时，
先用行锁内的持久化集合覆盖旧运行态集合，再生成本次 session/thread/turn/window 图；不同应用实例并发创建
同一逻辑种子的首个绑定时，后进入事务的实例会复用前一个实例已经提交的完整 UUID，而不是各自接受 A/B 两套结果。
数据库值与本次结果完全一致时，事务只读提交并跳过整集合序列化及写入；实际新增、触摸、裁剪或修复时才原子更新顶层绑定字段。
成功提交后再发布进程热缓存和最终出站快照，写后调度快照传播保持原流程。
旧仓储适配器继续走 GetByID/UpdateExtra 兼容路径，但不获得生产仓储的跨进程行锁语义。
这项处理不改变 UUID 算法、父子回合、缓存键、UA/TLS 或绑定有效期。新根共享写入在裁剪前为两个键预留容量，避免绑定集合满载时只保留 session 或 thread 单边。

生成热路径命中有效目标且集合未超容量时，只校验目标绑定；未命中、目标过期、超容量以及提交阶段继续完整裁剪。
因此无关过期项可能暂存到下一次缺省查找或提交，但不扩容、不延长目标有效期。
同一逻辑 turn 开始时间的热命中也只检查自身 TTL；新回合负责过期清理与既有容量控制。

普通 Responses、passthrough、Messages、旧版 Compact 和 WS 已有持久化调用均将本次身份快照交给同一提交步骤。
WS 后续帧使用同一准备预算；窗口变化或实际新增/更新持久身份绑定时执行提交，失败帧不提交连接状态。Cockpit 的 turn/parent/root 不落库，单纯切换客户端回合不会增加逐帧数据库读取或写入；窗口与线程绑定失败时仍保留待写标记，重试成功或核对持久状态一致后才清除。
合并选择的窗口或父线程绑定与准备阶段不同时，成功写入后统一更新本次出站快照；确认与最新持久化状态一致时也执行相同快照提交。
存储失败、绑定被裁剪或结果有歧义时不提交半更新的快照。若冲突改变 session/thread 派生根，则停止本次出站，
由调用方使用刷新后的账号重试或重新建立 WS，避免仅替换根 ID 却沿用旧窗口图。
核对仅追踪本次快照实际引用的绑定，在既有裁剪遍历中、删除依赖之前采集，不增加独立的全集合遍历；
UUID 派生算法保持不变，跨实例原子性边界是单个账号数据库行与一次最长 5 秒的准备事务。
在比较持久化值时顺带收集已有热缓存的差异；成功写入或确认与最新持久化状态一致后，
在同一账号锁内按账号与绑定键同步已存在的进程热缓存，
再提交出站快照；失败分支不发布选中值。否则后续缺少运行态绑定的调度快照可能恢复旧 UUID 并重新落库。
同步只覆盖当前身份集合；旧版回合集合保留在账号数据中但不再读取、合并或写回，不预热完整持久化集合，也不把提交时间当作所有绑定的新活动时间。
生产事务路径同时完成冷启动恢复：热缓存缺失或过期且账号快照也缺少绑定时，仍先读取数据库权威集合再派生。
正常基础调度继续保留数据库复核后的完整账号，避免最终水合回退到缺绑定旧快照；
完整账号缓存的乱序更新保护见[账号调度与缓存一致性](../architecture/account_scheduling_and_cache.md#scheduler_snapshot_consistency)。
直接绕过调度但仍使用生产账号仓储的入口具有相同恢复与行锁语义；只实现旧读写接口的测试或第三方仓储仍属于兼容边界。

session/full 模式继续让同一逻辑 turn 的 `turn_started_at_unix_ms` 在 Header 与内嵌 turn metadata 中使用同一生命周期值。Cockpit 则使用当前请求或当前 WS 帧的客户端有效值，不生成服务端兜底，也不读取先前请求的时间。已有且有效的平铺 `client_metadata.turn_started_at_unix_ms` 会同步到当前客户端值，并保留字符串或数字类别；缺省平铺时间不补造，异常类型保持原文，device/off 模式不增加该项改写。

Cockpit 不向 turn metadata 新增 `prompt_cache_key`；显式键的主要协议载体保持 Responses 顶层，客户端既有兼容元数据字段保留。

缓存键连续性只保证网关没有意外切换缓存命名空间，不代表压缩后的全部上下文都会命中上游缓存。窗口链元数据不是缓存内容；输入中的压缩结果和上下文片段保持原样。暂存的显式缓存键是有容量与 TTL 限制的进程内状态，并非上游缓存或跨进程持久化承诺。只有窗口标记可用时，会话种子取稳定线程前缀，不将压缩代数纳入其中。

旧版 `/responses/compact` 不读取或写入普通 Responses 的暂存缓存键，其 Header-only 兼容键不覆盖普通 Body 键的绑定与载体。客户端显式提供的 `prompt_cache_key` 按原值透传；未提供时保持缺失，不从普通请求的暂存绑定补齐。`client_metadata` 仍不进入该 compact 请求体。

### 远程压缩协议

ConcordRoute 同时兼容原生 Remote Compaction V2 和旧版 Compact 端点。两者共享 compaction 输出语义，但请求路径、传输方式、账号能力设置和模型改写边界不同：

| 边界 | 原生 `remote_compaction_v2` | 旧版 `/responses/compact` |
| --- | --- | --- |
| HTTP 识别 | 裸 `/responses` 请求同时携带 `stream=true` 且 `input` 含 `compaction_trigger`；`x-codex-beta-features` 不是识别门槛，但原生 V2 出站必保证包含 `remote_compaction_v2` | 客户端显式请求 `/responses/compact`，或带 `compaction_trigger` 但不满足原生 V2 条件的裸 `/responses` 请求被网关提升 |
| 上游传输 | 保持普通 Responses 流式链路，由上游直接返回包含 `compaction` item 的 SSE | 走独立 Compact 子路径；body-signal 流式客户端由网关把 unary JSON 结果合成为 Responses SSE，并在长时间等待时发送注释心跳 |
| 模型处理 | 沿用普通 Responses 的模型处理，不应用 `compact_model_mapping`，也不会因此追加 `-openai-compact` | 仅此路径在常规模型处理基础上应用账号 `credentials.compact_model_mapping` |
| 账号设置 | `extra.openai_native_compaction_v2_mode`、`openai_native_compaction_v2_supported` 和对应 `openai_native_compaction_v2_*` 探测信息只控制此路径；不读取旧端点状态 | `extra.openai_compact_mode`、`openai_compact_supported`、`openai_compact_*` 探测信息和 Compact 专属模型映射都只控制此路径 |

账号设置页中的“原生 V2 压缩”和“旧版 Compact 端点”都是各自协议的能力覆盖，不是协议开关。两者均提供 `auto`、`force_on`、`force_off`：自动模式跟随各自独立的探测结果，未探测账号保持可选以兼容历史配置，明确不支持时排除；强制开启始终允许，强制关闭始终排除。V2 模式只筛选原生 V2 请求，旧版模式只筛选 `/responses/compact`；两者都不会把普通 Responses 或另一条压缩协议改写成自己的路径。原生 V2 即使强制开启仍须满足普通 Responses 端点能力，不能把不支持 Responses 的 API Key 上游纳入候选。旧端点的 OpenAI OAuth GPT-5.6 请求还会把 `reasoning.effort=max` 降为 `xhigh`，原生 V2 则保留常规 Responses 推理强度语义。

管理端连接测试的 `compact` 模式是原生 V2 健康检查，使用普通账号模型映射并要求响应实际出现 compaction item；`legacy_compact` 是旧端点兼容性测试，才使用 `compact_model_mapping`。两种测试的可用状态、最后状态、错误和时间戳完全隔离，旧端点 404 不得改变 V2 能力判定。

WebSocket 的握手身份与每个 `response.create` 帧身份分开维护。现代客户端的后续帧保持连接的账号、installation、session 与 thread 绑定，新的显式 `prompt_cache_key` 立即生效；同窗口临时省略时复用当前键，相邻压缩窗口缺省时按与 HTTP 相同的规则继承前一窗口绑定；握手 Header 中的 `session-id`、`thread-id` 也纳入连接初始隔离状态；帧内显式 session/thread 与此连接原始标记不同时要求重连，避免跨会话沿用旧绑定。不同客户端 `turn_id` 以原值推进帧级回合，压缩推进窗口及历史锚点；内部重试复用已准备的帧身份，不原地修改首帧握手快照。`ctx_pool` 和 passthrough 使用同一推进逻辑，帧内 turn metadata 优先于旧握手头；现代后续帧缺省时不回灌首窗口头。Cockpit 的 turn、parent/root、开始时间、窗口代数与可选窗口字段存在性逐帧刷新，前帧携带、当前帧省略时保持省略。旧客户端保留原有连接身份兼容路径。

官方 Codex WebSocket v2 会先发送 `generate=false` 的预热 `response.create`，再以预热响应 ID 作为业务请求的 `previous_response_id`。严格续接比较会忽略逐请求变化的 `client_metadata`、仅用于传输的 `stream_options`，并把 `generate=false` 与后续省略该字段视为等价；`generate=true` 以及 model、instructions、tools、reasoning、store 等上下文字段仍必须保持一致，避免把无关请求错误串接。

WS 准备按身份绑定的实际变更触发持久化，不只检查窗口代数。新增或恢复 session/thread/window 绑定后，提交成功才清除脏标记；提交失败保留待提交状态，重试成功前不推进连接快照。Cockpit 的普通字符串或 UUIDv7 回合都不进入持久化；切换到已有窗口仍保留原有持久化快照复核。

OpenAI OAuth 的 HTTP、passthrough、旧版 Compact 与 WebSocket 出站会在模型映射和本地 fast 策略处理完成后，由网关生成 `x-codex-routing-hint`。提示至少包含最终上游模型；只有有效的 `priority` 或 `flex` 才附带 tier，`fast` 先规范化为 `priority`，`default`、未知值和空值均保持 model-only。旧版 Compact 规范化必须保留 `service_tier`，否则提示会丢失已经生效的路由层级。该头由网关独占控制：所有账号类型都会先删除调用方及账号覆盖提供的任意大小写变体，只有 OpenAI OAuth 路径会重新生成；API Key 路径不得透传伪造提示。OAuth HTTP 也不再自动注入或透传旧版 `responses=experimental` beta 标记，但同一头中的其它独立 beta 项仍保留。

`x-codex-beta-features` 是 Codex 的会话级协商头：OAuth 普通 Responses HTTP 与 WebSocket 握手在客户端未声明时补入 `remote_compaction_v2`，客户端给出的非空值保持原样；原生 V2 请求无论账号类型都保证该 feature 存在。上游响应中的 `x-codex-turn-state` 会在 HTTP/SSE、SSE 转 JSON 与 passthrough 路径显式回传。网关按 API Key 与客户端原始 session 记录最近签发账号；故障转移后，已知由其它账号签发的客户端回带值会被剥离，未知或同账号的值保持透传。

### 实验性 Turn-State 注入

非影子 OpenAI OAuth 账号可显式开启 `extra.codex_292_state_injection_enabled`。`codex_292_*` 是为了兼容既有账号数据保留的配置键名，不代表 HTTP 状态。该能力默认关闭，只覆盖普通 Responses、OAuth passthrough、Chat 转换、Messages 转换和 WS HTTP bridge 等 HTTP 推理路径；原生上游 WebSocket、令牌刷新、账号测试、额度/模型探测及其它账号流量继续使用账号主代理。状态机只检查最终响应头 `x-codex-turn-state` 去除外侧空白后的字符串长度：Pro 的 `292` 表示可复用满血态、`312` 表示降级态；Team 的对应长度为 `332` 与 `356`。四种响应对应的 HTTP 状态通常都是 `200`，HTTP 状态码不参与签发或撤销判定。这不是稳定的上游公开协议，因此实现不增加主动探测、后台轮询或响应体逐块解析。

开启后，账号主 `proxy_id` 不参与上述 HTTP 推理请求。没有有效 state 时使用 `extra.codex_292_state_acquire_proxy_id`；收到 Pro `292` 或 Team `332` 的有效满血 state 后，按账号 ID 与规范化模型在当前实例内存保存 state，并在后续同模型请求中覆盖客户端回带值，同时切换到 `extra.codex_292_state_egress_proxy_id`。任一代理 ID 留空都表示对应阶段由服务器直连，而不是回退到账号主代理。辅助代理不存在、停用、过期或解析失败时请求直接失败，不静默改走主代理或服务器直连。

state 使用 55 分钟保守内存租约；实例重启、租约到期、账号配置改变，或收到 Pro `312` / Team `356` 的降级 state 后，下一笔同模型业务请求重新进入获取阶段。空值与其它未知长度既不签发也不撤销，避免未来协议变化被误判。state 原文不写入账号 Extra、数据库、Usage 或普通日志；既有被动观测只保存最终上游 HTTP 状态码和头长度。管理员 Usage 另记录请求侧模式 `injected`、`acquire` 或 `disabled`，用于确认本次是否真正命中注入；历史或未接入路径保持空值。多实例不共享租约，因此每个实例独立获取；状态机只负责保存或撤销本账号本模型的注入值，普通错误分类、故障转移和客户端响应语义仍由现有网关策略决定。

WebSocket 连接池把 routing hint 视为拨号和普通复用的软亲和：优先复用相同提示建立的连接，池满时仍可在硬兼容连接上排队，显式 continuation 也不会仅因提示变化而断链。握手 beta feature 与本 fork 的 TLS fingerprint profile 仍是硬兼容键，任一变化都禁止复用，并会使尚未完成的旧目标预热拨号失效。路由诊断只记录网关推导的最终模型、规范化 tier、传输类型、账号 ID、是否生成提示和 WS 亲和决策，不记录提示头值、token 或凭据。

Responses WebSocket 与 HTTP 共用真实首内容判断：非空 delta、完整文本或工具参数均可产生首内容样本；`response.completed`、`response.done` 以及 content part/output item 事件若携带实际文本、工具参数等内容，同样以内容到达时刻计时。仅含状态、usage 或空结构的事件不产生真实首内容样本，避免把纯终态耗时误记为首 token 延迟。文本终态缺少响应 ID 时保留活动轮次已观测的耗时与首内容样本，但不补造响应 ID。

Responses HTTP/SSE 同样区分结构进度与可见输出：`response.created`、空 reasoning item 等进度可以提交当前 attempt、解除首输出超时并关闭 pre-output failover 窗口，但不记录真实首内容；非空文本/工具 delta、完整文本或工具参数、图片结果以及终态内实际 output 才开始首内容计时。只携带 usage 的终态保持首内容未观测。

使用记录的“首字”展示采用首响应（首块）口径：OpenAI 原生 Responses HTTP 普通/透传、WS
及 HTTP 桥接从本次转发开始到首个非空上游正文块/应用消息；创建通知算，本地心跳不算。
`FirstResponseMs` 优先于语义样本，真实首内容 `FirstTokenMs` 与调度反馈仍保留原值。
WS passthrough 展示总耗时对齐请求起点，内部旧耗时与协议生命周期不变。未接入的旧路径、
其它平台及历史记录不改写；完整边界见[首字指南](../guides/first-token-latency.md)。

OAuth 普通 Responses 与 OAuth passthrough 保留客户端显式提供的原生 `stream_options.reasoning_summary_delivery="sequential_cutoff"`，不主动补入该字段，也不改变 `reasoning.effort` 或 `reasoning.summary`。同一对象中的 Chat 专用 `include_usage` 及其它未支持成员仍移除；未支持的值、异常类型与旧版 Compact 请求沿用原有过滤规则。仅含原生选项的透传对象保持原始字节。该规则修正参数丢失，不承诺上游更早产生摘要或降低实际推理耗时。

OAuth passthrough 的 Codex 请求可以省略 `instructions`，网关会按请求模型补入内置 Codex 基础指令；显式提供的非空字符串保持不变，空白或非字符串值仍在本地拒绝。该规则同时适用于 Responses SSE 与旧版 Compact 请求。

Responses Lite 通道由 HTTP `X-OpenAI-Internal-Codex-Responses-Lite: true` 或 WebSocket `client_metadata` 中的对应标记识别，不根据模型名称推断。任何向 OpenAI 上游转发该标记的 HTTP、passthrough、旧版 Compact 或 WebSocket 请求都必须强制顶层 `parallel_tool_calls=false`。OAuth 账号还会统一设置 `reasoning.context=all_turns`，并把私有 namespace 工具声明迁入 `input.additional_tools`；API Key 账号保留除此之外的标准 Responses 请求语义。未携带 Lite 标记的普通 Responses、Grok 和专用 Images 请求不应用这些约束。

OpenAI OAuth 账号承接 Anthropic `count_tokens` 时会调用 Responses `input_tokens` 端点；缺少 scope、端点不存在，或上游代理在 API 前返回 HTML 格式的 `403` 时，网关改用本地 token 估算并返回成功结果。这类端点级失败不会冷却、临时踢出或标错账号；其它结构化鉴权与上游错误仍进入正常健康策略。

Codex 原生 `POST /v1/responses/input_tokens` 同时支持 `/responses/input_tokens` 与 `/backend-api/codex/responses/input_tokens` 别名，并在 Responses 协议准入之后、普通生成管线之前独立分流。请求继续经过 ConcordRoute Key 鉴权、计费资格检查、用户提示词替换、渠道/账号模型映射、账号调度与故障转移，但不会占用生成用量记录或产生 token 账单。官方 OpenAI API Key/OAuth 账号调用原生端点并保留上游 `input_tokens` 响应；自定义 OpenAI 中转、Grok 与通用上游账号直接使用本地 tiktoken 估算。官方端点返回 `404`、OAuth scope 不足或 OAuth HTML `403` 时同样本地回退，且不改变账号健康状态；其它 HTTP 错误继续使用现有账号健康和切号策略，代理、DNS、TCP 与 TLS 传输故障也在客户端响应尚未开始时复用普通 Responses 的故障转移逻辑。原生请求与真实推理共享最终 Codex UA、Originator、Version、TLS Router/Profile 和账号代理出口，避免预估与生成形成两套出站身份。

OpenAI OAuth 的普通 Responses 请求默认原样保留 Codex namespace 工具声明，并保留 `function_call`、`tool_call`、`custom_tool_call`、`mcp_tool_call` 历史项上的 `namespace`；普通消息等非调用项上的残留字段仍会清理。旧版 Compact 请求始终摊平 namespace 并移除输入项字段，API Key 出口也按标准 Responses schema 清理。API Key Responses 回放还会校验输入项 ID 前缀：message 使用 `msg`、工具调用使用 `fc`、reasoning 使用 `rs`；不符合类型约束的 ID 直接删除而不改写，避免伪造标识指向另一上游对象。仅当 OAuth 账号的兼容中转不接受 namespace 时，才应启用账号 `extra.openai_responses_flatten_namespaces=true` 恢复平名行为。每次 failover attempt 都会清空上一账号登记的平名映射，避免响应还原状态串到下一账号。

Responses 工具定义在进入 OAuth passthrough、Codex transform、Grok 或 API Key Chat 分流前统一修正显式为 `null` 的 `parameters.type`，将其归一为 `object`；处理范围包括顶层 `tools[]` 和多轮历史 `input[].tools[]` 中的嵌套工具。缺失 `type` 的合法宽松 Schema 保持原样，不能为了兼容而补写并收窄客户端语义。

Responses 请求降级到 Chat Completions 时，工具结果中的 `input_image`、`image_url` 和完整图片 data URL 不能留在只接受文本的 `tool` message。转换器会按 `call_id` 从工具结果中提取图片，把原位置替换为稳定标记，并在对应的一组工具回复后追加用户多模态消息；并行调用按工具声明顺序归属图片，孤儿或未回答调用不携带媒体。没有可识别图片的工具结果必须保留原始字节，避免无关 JSON 重编码改变提示缓存前缀。

OpenAI API Key 账号以 `force_chat_completions` 承接 `/v1/messages` 时，Chat 流中的并行 `tool_calls` 必须按 `tool_calls[].index` 聚合 ID、名称和全部参数分片，在流收尾时再按 index 顺序生成各自连续闭合的 `content_block_start`、`input_json_delta`、`content_block_stop`；参数分片暂存后一次拼接，聚合期间通过 Anthropic `ping` 维持下游活动，文本与 thinking 仍即时流式输出。空工具参数归一为 `{}`，call ID 保持原样，以便下一轮 `tool_result.tool_use_id` 配对。Anthropic `tool_choice.disable_parallel_tool_use=true` 映射为 Chat 顶层 `parallel_tool_calls=false`，字段缺失或为 `false` 时保持默认 `true`；`auto`、`any`、`none` 和具名工具的选择语义不变。

## 模型与能力

客户端模型先经过 Key、渠道和账号层映射。OpenAI 内置别名、reasoning effort 归一化、旧版 Compact 端点支持、图像/embedding 能力和传输能力会影响候选账号；模型列表只公开当前分组可请求的结果。账号级映射对显式 `gpt-5.6-sol`、`gpt-5.6-terra`、`gpt-5.6-luna` 以及会归一到这些变体的 `gpt-5.6` 请求保持变体隔离：同一请求不能在账号切换或故障转移时跨 Sol/Terra/Luna 漂移。`codex-auto-review` 等显式业务别名仍可按配置映射到目标变体；其它代际或自定义模型继续使用原有一跳映射语义。

API Key endpoint capability 可通过探测或配置表达 `responses`、`chat_completions`、`embeddings` 等能力。OAuth/Codex 账号还可能包含 Realtime、WebSocket、旧版 Compact 端点状态和客户端身份限制。未知模型可以在管理员明确配置的兼容上游中透传，但没有定价或能力证据时不能虚构价格与功能。

Images API 的流式与非流式上游请求都脱离客户端请求取消信号继续执行，并由上游响应超时控制最终回收。生图属于长耗时且上游可能已经产生实际成本的媒体任务；客户端中途断开不能取消上游并丢失已完成图片的计费结果。下游写失败不改变图片产出和结算事实。

## 额度与调度

OpenAI 是通用高级调度器的能力适配者之一，而不是该调度器的全局所有者。只有最终目标 Group 的 `scheduler_type=advanced` 时，OpenAI 路径才在共同 active/schedulable、分组、模型、限流和并发硬过滤后使用通用 Top-K 评分；`basic` 保留原有默认选择路径。高级分组可用稀疏 `advanced_scheduler_overrides` 覆盖全局 Top-K、评分权重和粘性开关，未设置字段继续继承网关设置。高级分组还会考虑所需 transport/capability、账号优先级、负载、排队、错误率、近期延迟、配额余量和粘性上下文。previous response、WebSocket 会话和显式 session 可约束账号复用；只有策略允许时才能迁移。

OpenAI 专属能力只在账号和请求具备对应条件时加入候选或分数：Responses transport、WebSocket、旧版 Compact、previous response、订阅优先和 Codex 额度余量都不会排除缺失这类可选信号的普通账号。OAuth 5 小时、7 天等上游窗口和自动暂停仍由 OpenAI 设置及账号运行状态控制，不随高级调度器通用化而迁移到其它平台。

OAuth 账号的 5 小时、7 天等上游窗口和重置时间保存在账号运行状态中，可触发临时限流或自动暂停；API Key endpoint capability 仍可独立探测。OpenAI 不再采集上游站点声明倍率，也不按该值进行低倍率优先或高级评分。账户本地 `rate_multiplier` 和渠道上游计费模型来源继续用于 ConcordRoute 结算，但都不是用户余额、订阅、Key 限额或用户平台额度。

管理 API 的 `GET /admin/openai/accounts/:id/quota` 保持只读；账号列表使用 `POST /admin/openai/accounts/:id/quota/refresh` 查询上游并把重置次数写入 `account.extra.codex_reset_credit_snapshot`。正数次数只有同时取得到期明细时才覆盖快照，前端水合时过滤已过期明细并把次数收敛到仍有效的卡片数量。该 extra 键只用于展示缓存，不触发调度 outbox；Spark 影子账号的查询可解析母账号额度，但快照仍写在被查询的行上，且列表继续只提供查询入口，不提供真实重置按钮。

## 失败与诊断

账号状态更新使用凭据快照/CAS，避免较早请求在 token 已刷新后再次封禁账号。401/403、429、endpoint 不支持、内容策略、网络错误和上游 5xx 分别分类；只有可切换且客户端响应未开始的失败才进入下一账号。OpenAI 上游代理或 CDN 返回的 HTML 403 只证明当前链路或端点被阻断：请求仍可按既有规则 failover，但不得递增连续 403 计数、临时停调或永久禁用账号；结构化 JSON 与纯文本 403 继续按账号级策略处理。API Key passthrough 池模式会把 `pool_mode_retry_status_codes` 命中的 HTTP 错误先转换为未提交响应的 failover，在同账号预算耗尽后才换号；未配置时默认覆盖 401、403、429，显式空列表可关闭这类按状态码重试。原生 Responses 上游返回的确定性 `400` 在现有账号策略、池模式重试和错误透传规则均未要求改写或故障转移时，按真实 400 回写，并保留脱敏后的 `message` 与诊断所需 `type`、`code`、`param`；瞬时处理错误和容量类 400 仍保持可重试或通用网关错误语义。图片模型被 Codex 文本端点以 plan-gated `400` 拒绝时属于端点错配：当前尝试仍切号，但不写模型冷却，避免影响同账号后续通过 `/v1/images/*` 正常生图；专用 Images 端点上的同类拒绝仍按真实账号能力缺失冷却，图片模型的 `404 model_not_found` 也不豁免。Responses HTTP 与 WebSocket v2 首次发送时保留加密 reasoning/compaction；若上游明确返回 `invalid_encrypted_content`，同账号恢复最多重试一次，清理账号绑定的加密状态但保留未加密 compaction。

账号与模型组合的瞬时失败按连续结果累计：首次失败只记录，第二次短冷却，第三次及以后长冷却。请求间隔较长不能把持续故障误当成恢复，只要未超过状态回收 TTL，稀疏流量中的失败仍继续累计；任一成功结果立即清零该组合。TTL 只负责回收长期不再使用的条目，不能兼作短窗口的连续失败重置条件。

流式错误要保持 SSE/WebSocket 协议完整；Responses 可产生 `response.failed`，非流接口返回相应 OpenAI envelope。入站 WebSocket 的下行写不得继承独立的 ingress 租约取消信号：旧 ingress 路径绑定客户端请求生命周期并叠加 write timeout，v2 relay 只受 write timeout 限制，退出路径再通过显式 Close/CloseNow 回收连接；这样租约丢失不会在终态事件写入期间抢先硬关 TCP，客户端可先收到终态事件，再收到 1013 关闭帧。上行写继续继承控制面取消，以便快速回收上游连接。HTTP 200 SSE 中的 `rate_limit_exceeded` 按语义状态 429 进入故障转移与池模式重试，但不使用该 200 响应的正常配额快照头写入默认账号冷却。上游容量降载通常先发 `error`、再以 `response.failed` 收尾；`server_is_overloaded` / `slow_down` 的前置错误帧在尚无业务输出时继续留在 attempt 缓冲中，触发有界同账号重试和 pre-output failover，并按请求级瞬时故障处理，不冷却当前账号。已有真实输出或重试耗尽后不能重放请求，SSE 与 WS HTTP bridge 会仅在客户端副本中把这两个致命码改为可重试的 `server_error`，原始事件仍用于账号策略与观测。客户端尚未收到业务输出时，池模式账号的其它瞬态流内处理错误也可在请求级预算内重试同一账号；旧版 Compact 桥接心跳注释不算业务输出，即使已提交 200 响应头，只要没有语义 SSE 载荷，最终失败仍必须追加 `response.failed`。OpenAI Responses 标准流与 passthrough 流若只收到前导事件和完全不含 output、usage、error 的 `response.completed` / `response.done`，会在尚未写出客户端业务内容时按静默拒绝切换账号，而不是记录 0/0 成功；终态含 usage、error、任一输出项，或此前已出现语义输出时均不触发该规则。一旦真实输出开始，网关不得重放请求或切换账号。最终错误还可命中[网关错误响应策略](gateway_error_policy.md)，但规则不会把失败结算成成功。排障应同时检查账号类型、required transport/capability、客户端限制、privacy status、模型映射、quota reset、代理/TLS 和 attempt 记录。

相关文档：[网关请求生命周期](../architecture/gateway_request_lifecycle.md)、[账号调度与缓存一致性](../architecture/account_scheduling_and_cache.md)、[模型目录与市场](model_catalog_and_marketplace.md)。

管理员使用记录的 HTTP 状态与 Codex 状态头长度属于默认被动观测，不代表模型能力或设备证明；字段空值、覆盖路径、迁移与性能边界见[使用记录上游状态观测](../operations/observability_and_data_lifecycle.md#使用记录上游状态观测)。该观测不构造或注入状态，不改变本节既有透传语义。
