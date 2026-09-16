# 首字延迟：统计口径与热路径排查

## 结论

“鉴权、刷新令牌、读取配置可能增加延迟”是需要分路径验证的判断，并非每次请求都执行所有慢操作。
本项目已有 API Key 两级认证缓存、OpenAI access token 缓存、OAuth 刷新互斥与后台刷新，以及部分运行配置缓存。
缓存失效、凭据接近过期、存储变慢、并发排队或故障重试仍可能增加等待；直接跳过鉴权或刷新不是本轮方案。

已有的真实首内容测量与热路径优化：

1. Responses 转 Chat Completions、Responses 转 Messages 以及 Anthropic 透传/桥接路径把首个 `data:` 当作首 token；原生 Chat 还会把角色帧、空增量或结束帧算入首 token。
2. WS 的部分计时分支仅看事件类型，空 `.delta` / `.done` 也会产生首 token 样本。
3. OpenAI Fast 策略热路径缺少跨请求缓存，携带相应 `service_tier` 的连续策略评估重复读取设置表。

## 四种时间不要混用

| 指标 | 实际含义 |
| --- | --- |
| HTTP TTFB / 首响应头 | 首个响应头字节；可能早于响应正文，并非本次展示终点 |
| 使用记录中的“首字” `first_token_ms` | OpenAI 原生 Responses HTTP/WS 新记录显示**首响应（首块）**：本次转发开始到首个非空上游正文块/WS 应用消息 |
| 内部真实首内容 `OpenAIForwardResult.FirstTokenMs` | 保留各路径原计时点；HTTP 在有内容事件完成网关 Flush 后采样，供调度反馈使用 |
| 用户端首内容耗时 | 客户端发起至收到并展示内容，额外包括入口、排队、回程、解析等 |

### 首响应的边界

- HTTP 普通、透传和 WS→HTTP 桥接在 `Body.Read` 返回 `n>0` 时采样，不等待完整 SSE 行、JSON、正文或下游 Flush；同时返回数据和 EOF 也算。
- WS 在收到非空应用消息时采样；创建通知、空结构和上游发来的保活数据均算首块。本地心跳、仅 HTTP 响应头、WS ping/pong 控制帧不算。
- HTTP 起点沿用 `Forward()`/当前桥接请求的起点。采样状态属于当前尝试，失败请求的样本不带到下一次；同次 Forward 内之前的等待仍属于原起点区间。
- 入站 WS 连接池路径沿用逐轮 `turnStart`：取得租约后、写上游请求前开始，不包含此前的连接获取与准备时间；各路径指标均不是客户端端到端等待时间。
- WS passthrough 新展示值从首轮转发开始（包括调用方进入 Relay 前的等待）、后续轮请求写出前开始，不再从 `response.created` 自己开始得到 0ms。按 response ID / 当前活动轮次隔离，无 ID 终态仍保留本轮样本。
- WS passthrough 的展示总耗时使用与首响应相同的请求起点，避免首响应大于总耗时；内部旧 `Duration`、真实首内容、生命周期、调度及转发不变。其它路径总耗时不变。
- 被原地恢复逻辑丢弃的 WS 错误帧不污染成功轮次样本。没有非空上游数据不伪造 0ms，不用总耗时补值。
- 首块到得晚，显示仍然会高：16 秒才到第一块，仍显示约 16 秒。没有固定扣减、封顶或缩放。

`FirstResponseMs` 只在 OpenAI 流式/WS 的 `RecordUsage` 展示边界优先使用；没有新样本的旧调用方先沿用 `SemanticFirstTokenMs`，再沿用原内容值，其它平台/协议不变。
内部语义和真实内容样本继续保留，与新展示值互不覆盖；诊断 `first_content_received`/Flush 与账号调度不因展示提前而改变。
历史记录不回填，没有数据库迁移或额外数据库读取。使用记录、导出和相关预聚合读取展示字段；跨版本对比是口径对比，不代表模型生成提速。

例如转发开始后 0.2 秒收到创建通知、0.5 秒收到空 reasoning item、8 秒收到推理摘要，展示约 0.2 秒；内部语义约 0.5 秒、首内容约 8 秒。HTTP Flush 完成也不代表反代/客户端已经展示。
界面保留“首字”标签，悬浮提示明确解释首响应定义；缺省仍显示“未记录”，明确非流式无首内容样本时显示“不适用”。

### 实现与验证

- 只增加内存计时，不改 SSE/WS 原帧、模型选择、首输出超时、故障转移、计费、UA/TLS、Cockpit、turn/session/UUID 或缓存键。
- HTTP reader 使用只发布一次的原子快照，覆盖异步扫描与超时收尾并发；不复制数据块、不逐块记录日志。
- 真实首内容继续用 `internal/pkg/openai/stream_output.go` 分类，终态正文、拒绝内容、工具参数等原规则不变。
- 测试覆盖半行首块、晚到首块、EOF 同时带数据、无数据、跨尝试隔离、快照并发、创建通知延迟、WS 多轮与无 ID、原帧不变及入库/调度分离。

```bash
go test -tags unit ./internal/service ./internal/service/openai_ws_v2 \
  -run 'TestOpenAIFirstResponse|TestOpenAISemanticTTFT|TestRelay.*(TTFT|FirstResponse)'
```

## Fast 策略缓存

入口：`SettingService.getOpenAIFastPolicySettingsCached`。

- 每个服务实例独立缓存，TTL 为 5 秒；仅用于运行态。
- 冷缓存与过期缓存通过 singleflight 合并回源；DB 回源最多等待 5 秒。
- 请求取消后立即停止自身等待，共享回源不因其中一个请求取消而被破坏。
- 管理读取 `GetOpenAIFastPolicySettings` 仍直接读存储。
- `SetOpenAIFastPolicySettings` 成功提交后立即发布当前实例的新快照；保存失败不污染缓存。
- generation 防止慢旧查询覆盖已保存的新值；串行保存保证 DB 提交顺序与快照发布顺序一致。
- 其它实例或直接 SQL 更新的传播窗口为当前 TTL，随后第一次读取触发回源。新缓存命中不延长该 TTL。
- 返回深拷贝，调用方修改规则列表不会污染其它请求。
- DB 错误不缓存为长期默认值，调用方维持原有错误处理语义；未新增授权放行规则。
- 已存在的 WS ctx_pool 会话级策略快照机制保持不变：已建立会话不因这次优化而逐帧重读设置，新策略在其下一次策略取值/新会话时使用。
- 未携带有效服务档位且原来就跳过策略求值的请求，不会因本轮新增一次设置查询。

## 本地验证方式

测试使用合成内容与模拟存储，不读取真实令牌或真实会话。

```bash
go test -tags unit ./internal/service ./internal/service/openai_ws_v2 ./internal/pkg/openai \
  -run 'TestOpenAITTFT|TestOpenAIChatTTFT|TestOpenAIFastPolicy|TestRelay.*TTFT|TestOpenAIResponsesTTFT|TestOpenAISemanticTTFT|TestStreamSemanticOutput'

go test -tags unit ./internal/service -run '^$' \
  -bench '^BenchmarkOpenAIFastPolicyHotPath$' -benchtime=100x
```

覆盖：

- 延迟到达的正文、纯角色/结构流、空增量、usage、终止事件；
- Chat 与 Messages 的真实 SSE 转发入口，以及真实 WS Relay 入口；
- WS 转发帧数与输入用量保持不变；
- 并发冷读去重、配置保存、慢旧查询与保存竞态、返回值隔离；
- 请求取消、存储失败、缓存到期、实例隔离；
- 原有原生 Responses 首内容回归与相关鉴权/令牌测试。
- 首语义与首内容分离、0.5 秒/8 秒时序、HTTP 普通/透传原帧一致性、WS 跨轮次隔离，
  以及真实 `RecordUsage` 入库值与调度用结果不互相覆盖。

性能基准为设置查询注入 2ms 延迟，比较预热后每次策略评估的时间和 `db_reads/op`。
它衡量被删除的配置读取开销，不代表上游生成、真实数据库、生产网络或整站 p95 的提升。

## 生产定位建议

### 阶段诊断开关

将 `gateway.ttft_diagnostics_enabled`（或环境变量
`GATEWAY_TTFT_DIAGNOSTICS_ENABLED`）设为 `true` 后，模型网关 access log
会附带 `ttft_stages_ms`。这是从入口收到请求开始的阶段时间点（毫秒），不记录
请求体、令牌或响应内容，默认关闭。常见字段包括：

- `request_received`、`auth_complete`、`routing_complete`、`forward_started`；
- `request_body_read`、`content_moderation_started`、`content_moderation_done`；
- `upstream_do_started`、`upstream_headers_received`、`first_upstream_byte`；
- `first_sse_event`、`first_content_received`；
- `first_content_flush_started`、`first_content_flush_completed`；
- `first_visible_output`、`first_downstream_flush`、`stream_completed`、`request_completed`。

HTTP Responses、透传和原始 Chat 同时输出 `ttft_attempts`：每次实际上游 Do 独立编号，
关联账号 ID 和 `stages_ms`，同账号重试也独立记录。最多保留最近 64 次，编号不重置。
这些毫秒仍相对请求入口，方便比较尝试之间的等待；旧连接迟到的 trace 回调只记到旧尝试。
平面的 `ttft_stages_ms` 保留请求级前置阶段的首次值，上游/流阶段仅来自最后一次尝试，
不把失败尝试的完成时间混入成功尝试。涉及重试的归因应读取 `ttft_attempts`，不要跨尝试做差。

在同一次尝试内，`first_content_received` 表示解析识别到首个可用内容，
在下游写出之前记录；到 `first_content_flush_completed` 的间隔包含网关缓冲、处理和下游写出等待。
`first_content_flush_started`/`completed` 单独覆盖首内容所在的 flush。
`first_visible_output` 保持既有首字口径；断连补样本分支不保证出现 flush-completed。
`first_downstream_flush` 可能只是更早的结构帧，不能当成首内容到达。
若 `first_upstream_byte` 很晚，应继续区分连接、代理和上游响应头等待；
仅凭该字段仍无法单独量出 DNS/TCP/TLS 或模型排队。

### 连接与请求操作明细

同一诊断开关现在还输出两组有界事件，不新增上游探测：

- `ttft_operations.events`：请求级 token 获取、cache read/hit/miss、刷新调用、刷新锁竞争等待、
  用户/账号槽位、账号选择及同账号重试退避；另外区分 `api_key_auth`、`request_body_read`、
  `identity_lock_wait` 和 `identity_binding_read/merge/write`。每次发生都记录，不复用第一次的时间戳。
  认证结束记录在下游 handler 前；请求体读取包含解压、不包含后续 JSON 规范化。
  认证内部的 Body 读取与认证区间可能重叠，不能直接相加。
- `ttft_attempts[].transport.events`：该次 Do 内的主机校验、客户端池条目获取、连接获取与复用、
  本地 DNS/TCP、标准 TLS、自定义 TLS、代理协商、请求头/请求体写完和首响应字节。

每条事件使用固定 `phase` 和相对请求入口的 `at_ms`；完成时 `failed=true` 表示有错误，
但不记录错误原文。`connection_got` 还携带 `reused`、`was_idle` 和 `idle_ms`。
每个事件列表最多 128 条，超量通过 `dropped` 显式标记；上游尝试仍最多保留最近 64 次。
关闭诊断不创建采样状态、不挂新增网络回调。事件不携带主机、IP、凭据、证明、Header 或正文，
不会修改 UA/TLS 模板、连接池键、Cockpit、UUID、缓存键、重试次数、等待预算和计费。

判读顺序：

| 区间 | 能解释的等待 | 边界 |
| --- | --- | --- |
| `account_selection` / `user_slot` / `account_slot` 起止 | 选择、并发槽位获取及相关处理 | 不等于上游推理，部分阶段早于列表 TTFT 起点 |
| `token_get` / `token_refresh` / `token_lock_wait` 起止 | 凭据获取、刷新调用和锁竞争等待 | 刷新调用可能包含内部锁/存储，区间会嵌套 |
| `host_validation` / `client_acquire` 起止 | 目标校验及客户端池条目获取 | 不等于 Transport 的连接获取 |
| `connection_get` → `connection_got` | 连接池等待或新建连接的总等待 | 新连接可能包含 DNS/TCP/代理/TLS，勿再次相加 |
| `request_headers_written` → `request_written` | Header 回调之后的请求体读取、编码及写出 | Header 回调时仍可能在缓冲区，不是纯网络上传 |
| 成功的 `request_written` → 最终响应头/`first_content_received` | 请求发完后的等待 | 混合代理、网络、上游排队/处理，不能单独归因于模型 |
| `first_content_received` → `first_content_flush_completed` | 网关缓冲/处理和下游写出 | 客户端收到及渲染仍需客户端侧时间 |

HTTP/HTTPS 自定义代理分别记录 CONNECT 协商和代理自身 TLS；
SOCKS 的 `proxy_tunnel` 包含到代理的连接及 SOCKS 协商，代理端解析目标 DNS 的耗时也在其中。
自定义 uTLS 使用 `tls_client_handshake`，只观察握手，不改 ClientHello；原生标准 TLS 使用 `tls`。
原生 HTTP CONNECT 没有独立协商回调时只体现于连接获取总区间，不凭缺省字段宣称 0ms。
网络重试/双栈拨号可产生重复或重叠事件，保留顺序但不提供每条底层连接的独立 ID；
不要把首次 start 与另一连接的 done 配成单次耗时。复用连接、IP 字面量及远端 DNS
可能根本不触发本地 DNS/TLS 回调，缺省是“没有该项观测”，不是耗时为零。

这些新增接线覆盖 HTTP Responses、HTTP passthrough 和原始 Chat 出站；
WS 可共享部分请求级操作，但不代表 WS 全帧网络观测已补齐。
已有访问日志输出/落库开关仍生效，诊断开关不强制开启系统日志落库。
排障时短期开启并利用正常流量采样，完成后关闭，避免长期增加日志量。

### 诊断自身的开销边界

- SSE 字符串与字节载体共用可见输出分类规则；字节入口继续使用字节 JSON 读取，避免为了复用逻辑整体复制大帧。
  原生 Responses HTTP 在首内容完成 Flush 并记录 TTFT 后停止重复执行首字分类；结构输出、故障转移、
  用量解析和正文转发仍继续执行。单个巨大 delta 的 JSON 取值仍可能分配内存，这不是零分配承诺。
- 默认关闭；关闭时不分配采样状态或安装新增网络 trace，固定操作的空结束回调无分配。
- 开启后只在当前请求内存中记录时间点。锁属于当前请求/尝试，不使用跨请求的采样全局锁；
  每个事件列表有数量上限，不在事件回调中写文件、访问数据库或向上游发送探测。
- 固定操作名使用常量，不重复拼接起止后缀；已规范化的阶段名直接复用，
  避免流式响应反复标记首 Flush 时持续制造临时字符串。异常名称保留原有裁剪/清洗规则。
- 诊断快照随访问日志在处理器返回后构造，不逐 token 输出日志。Info 关闭且没有可输出的
  Gin Warn 错误时，跳过字段构造和序列化；入口拒绝统计、允许输出的错误日志保持原行为。
- Ops 系统日志落库仍是既有有界异步队列；标准输出/文件日志仍可能同步写入。
  慢磁盘或日志收集端背压可能延迟请求收尾，并在高并发下间接占用资源，因此不承诺零开销。
  “日志在首内容 Flush 后写”也不代表客户端已收到首字或请求已结束。

开关成本与序列化成本分别由 `BenchmarkTraceOperation`、`BenchmarkTraceRequest`、
`BenchmarkTTFTRepeatedFlush`、`BenchmarkTTFTDiagnosticsRequest` 和
`BenchmarkLoggerTTFTOverhead` 覆盖。示例命令：

```bash
go test -tags unit ./internal/pkg/latencytrace ./internal/service ./internal/server/middleware \
  -run '^$' -bench 'BenchmarkTrace|BenchmarkTTFT|BenchmarkLoggerTTFT' -benchmem -count=3
```

基准只衡量本机模拟状态、回调和日志编码（输出到 Discard），不是线上首字、磁盘吞吐或网络
p95 的测量。上线短期诊断仍需观察 CPU、GC、日志速率和丢弃计数，取到样本后关闭。

先把同一个请求的以下时间对齐，再决定调整哪个环节：

1. 客户端：发起时间、收到头、第一条模型事件、第一条非空内容、结束时间；
2. 网关入口：鉴权前后、计费预检、账号调度与槽位等待；
3. 上游：token 缓存命中/刷新锁等待、连接复用、DNS/TCP/TLS、收到头、首内容；
4. 重试：每次失败的账号尝试、退避时间、最终成功尝试；
5. 回程：网关 Flush、入口反代是否缓冲、客户端何时读到数据。

`OpsUpstreamLatencyMsKey` 在 HTTP 路径测的是客户端 `Do` 至响应头返回的一段等待，通常混合连接与上游等待，
它不是独立的模型推理计时。`OpsAuthLatencyMsKey` 在 handler 内赋值，也不等于完整 middleware 鉴权耗时。
这些现有分段有排查价值，但不宜直接相加当作完整端到端链路。

按冷/热连接、缓存命中、模型、思考档位、上下文长度和并发量分别比较 p50/p95，
不要用两个系统不同口径的“首字”相减，推断网关固定增加了若干秒。
地理位置会影响路由，但某个欧洲节点测到的 1 秒不是“欧洲固定物理延迟”，也未证明当前部署的瓶颈在地区。

## 上游资料

以下是公开讨论，不等于当前部署已复现了其中所有现象：

- [Sub2API #5617：首字显示异常，与上游不一致](https://github.com/Wei-Shaw/sub2api/issues/5617)：
  评论同时讨论过计算口径与缓冲。区分两者后再测量，不以“看着更低”作为准确性标准。
- [Sub2API #6401：CPA + Sub2API 首字延迟](https://github.com/Wei-Shaw/sub2api/issues/6401)：
  评论指出首帧与非空内容的定义差异，也有人报告 WS 超时；这些是不同层面的问题。
- [Sub2API #5813：高并发 settings 重复查询](https://github.com/Wei-Shaw/sub2api/issues/5813)：
  报告来自其描述的定制版本，提供了存储查询热点线索，不证明所有版本具有相同开销。
- [OpenAI 延迟优化指南](https://developers.openai.com/api/docs/guides/latency-optimization)：
  讨论流式输出、模型与输入输出处理等影响延迟的因素；不保证任意地区或中转的固定首字时间。
