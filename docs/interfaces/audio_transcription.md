# 听写转录

本文定义非流式录音上传、上游转录、分组准入、时长计费，以及独立 Windows 音频适配组件、分段听写和本机朗读。Live 服务端协议由 [OpenAI 上游](openai_upstream.md#openai_live_runtime) 定义；分段听写不等于官方逐词 partial transcript，不修改桌面登录状态。

<a id="transcription_gateway"></a>
## 入口与准入

- `POST /v1/audio/transcriptions` 与 `/audio/transcriptions`：OpenAI Audio API 的 multipart 兼容子集，必填 `file`、`model`。
- `POST /transcribe` 与 `/backend-api/transcribe`：桌面形状兼容别名，允许省略 model，缺省使用 `gpt-transcribe` 作为本地请求/计费别名。
- 四个入口均要求 ConcordRoute API Key，并执行用户/团队/余额或订阅、分组、渠道、用户与账号并发准入。匿名请求不会进入音频解码和上游上传。
- 嵌入式前端的普通与兼容中间件均放行上述 API 路径；无 `/v1` 前缀的别名同样进入鉴权，而非返回 SPA HTML。该边界有 `embed` 构建回归并纳入 CI。
- 分组必须是 OpenAI，且管理员显式开启 `allow_audio_transcription`。默认关闭，与 `allow_live`、文本协议开关独立；创建、更新、复制、认证缓存与 DTO 均携带该字段。
- 复合 Key 仍需明确的模型前缀选组。无 model 的桌面请求应使用绑定单组的 Key。

请求支持 `language`、`prompt`、`response_format=json|text`、`stream=false`。未知/重复字段、空文件、超长字段与 streaming 请求返回明确错误，不静默截断或假装支持。OAuth 上游未建立 prompt 支持契约，因此携带 prompt 的请求只选择 API Key 账号。不支持时间戳、说话人标注、字幕和实时听写协议。

## 上游与身份

- ChatGPT OAuth：真实 access token 与账号上下文，发送 `file` 和可选 `language` 至 `/backend-api/transcribe`。PAT 与 agent-identity 账号不参与调度。OAuth 服务端选择实际识别模型，客户端 model 不被冒充为实际上游模型。
- OpenAI API Key：转到经校验的账号 base URL 下 `/v1/audio/transcriptions`，应用既有 Key、渠道和账号模型映射，保留支持的 multipart 字段。
- 沿用账号代理、TLS Router/Profile、账号或全局 UA 与配对 identity header。缺失已配置代理时继续 fail-close，不新增 Chrome 客户端或直连回退。
- 转录不绑定 Responses 的 session/thread/window/turn，也不修改 Cockpit、缓存键或收敛策略；不创建或重放设备证明。
- 不自动跨账号重传录音。上游拒绝只返回脱敏错误；听写 401/403 不把原本可推理的账号标记为失效。
- JSON 成功响应必须有字符串 `text`；空字符串可表示静音。HTML、缺失 text、错误类型、超大响应和非 2xx 不作为成功计费。

## 资源、隐私与计费

上传总上限 26 MiB、单文件 25 MiB、单字段 64 KiB、最多 16 个部分，录音最长 10 分钟；响应最多 1 MiB。handler 的上下文预算为两分钟，上游阶段最多 110 秒，并继承取消；慢请求体还受 HTTP 入口读超时约束。

PCM WAV 使用自洽头部和真实数据长度计算时长。其它受支持音频容器通过本机 `ffmpeg` 解码为固定采样率 PCM 并计数：管道协议白名单、容器白名单、单次 15 秒、每实例最多两个解码进程，禁用视频/字幕输出并限制 PCM 总量。不按压缩文件大小或客户端声明 duration 猜测费用。缺少解码器时，PCM WAV 仍可用，其余音频明确返回 503；无效或过长音频返回 422。

成功请求通过既有 mandatory 用量池、幂等账务、分组 `audio_stt_price_per_hour` 与倍率结算，单位是实际音频小时；缺省价格沿用现有 STT 分组默认值，**不是宣称上游官方成本**。输入输出 token 留空/零而非捏造。音频时长与请求耗时是两个概念。录音与转录文本不写入用量正文、数据共享或普通日志；异步结算只持有请求摘要与计量结果。

每次成功转录使用独立的 `openai_audio:` 结算 ID，异步任务重试沿用该 ID；客户端复用请求 ID 不会合并不同录音。用量显示倍率与事务结算倍率均使用音频基础倍率，不叠加文本 token 高峰倍率。

本功能不添加周期探活、额度查询或隐私初始化；成功响应如带额度 Header，沿用被动快照更新。

## 客户端接入与验收边界

客户端应将已录制音频以 multipart 发往上述入口，携带网关 Key，再读取 `text`。标准 API 客户端可显式指定转录 base URL；它与模型 Responses base URL 不应被假设为同一个配置项。

部署后先在管理台的 OpenAI 分组编辑表单勾选“听写转录”，设置 STT 每小时单价，并使用绑定该分组的 Key。以下为 Windows 手动验收调用，环境变量由操作者提供；音频上传会触发实际转录与计费：

```powershell
curl.exe --fail-with-body "$env:CONCORDROUTE_BASE_URL/v1/audio/transcriptions" `
  -H "Authorization: Bearer $env:CONCORDROUTE_API_KEY" `
  -F "model=gpt-transcribe" `
  -F "file=@C:\path\sample.wav" `
  -F "language=zh"
```

Codex Desktop 的原生听写使用独立桌面 HTTP/认证通道。新增网关路由不会自动把 API-key 登录转换为 ChatGPT 登录，也不会让桌面开始向本路由发送 Key。服务端路由测试、模拟上游测试与实际客户端按钮验收必须分开记录；最终验收须关联一次真实录音点击、到达网关的请求、上游接受与文本回填。

原生 `/dictation/stream` 不在本功能范围内；也不把 `/backend-api/codex/transcribe` 宣称为已验证的转录入口。

<a id="desktop_audio_adapter"></a>
## 独立桌面适配组件

`tools/codex_audio/` 提供独立 Node 宿主、可生成的 Codex++ 用户脚本和可卸载桌面 HTTP 适配器。统计脚本的 Live Token Cost 指实时用量统计；音频组件不依附或修改该脚本，不修改安装包、登录状态、Responses、UA/TLS、Cockpit 或缓存键。

```text
当前用户 auth.json 的 OPENAI_API_KEY → Node 宿主（Key 不进入 renderer）
  ├─ 受当前用户 ACL 保护的安装引导能力 → 用户脚本 → /audio/session
  ├─ 按需返回 30 分钟上传能力 → /dictation/transcribe → 固定 HTTPS 网关 /transcribe
  └─ 可选 Live 创建能力 → /audio/live → 固定 HTTPS 网关 /v1/live
原生录音 → 桌面 HTTP 单例适配器 → helper → JSON text → 原生回填逻辑
独立音频面板 → 4 秒 PCM WAV 分段 → 同一个听写 helper → 文字草稿
朗读 → localhost /audio/speech → Windows System.Speech → WAV → 页面播放器
```

- helper 仅绑定 `127.0.0.1`。听写端口动态分配；安装器的控制/Sideband 缺省端口为 19846/19847。网关必须为 HTTPS origin，拒绝路径、userinfo、query、fragment 和跟随重定向，不成为任意目标代理。
- 独立宿主只从当前用户的 `.codex/auth.json` 读取 `OPENAI_API_KEY`；不解析 OAuth refresh token、不模拟 ChatGPT 登录。Key 不写脚本、日志或命令行。安装时必须显式传入可信的 `-GatewayOrigin`，没有网络默认值，缺省会在读取凭据和启动宿主前终止。
- 安装器依赖 Node 24+、PowerShell 7、Python 3.11+。先验证固定桌面资源哈希，再写独立用户脚本；未知构建停用，不猜测混淆导出。安装不重启当前桌面，需重新加载用户脚本后才进入现有 renderer。
- 引导能力是当前安装专用的随机 256-bit 本地凭证，与网关 Key 不同。它存于 `%LOCALAPPDATA%\ConcordRouteAudio\bootstrap.secret` 及当前用户的 `concord-audio.js`，ACL 仅当前用户和 SYSTEM。普通 Stop/Start 保留安装能力，Rollback 删除该能力；下次安装启动重新生成。此边界明确区别于旧 IPC-only 组件：引导能力落受限文件，30 分钟上传能力只在内存传递。不要分享生成的运行文件；同一用户/renderer 内的其他插件并不受此机制强隔离。
- `/audio/session` 要求正确引导能力、精确 Host、POST、空 body、无 Origin，才续租并返回上传能力。仅在音频调用时请求它，无周期网络探测、额度查询、登录刷新或自动重传。宿主重启仍轮换上传能力，但已加载脚本可使用安装能力按需重新获取，无需仅因宿主重启重载脚本；宿主退出释放全部监听和在途请求。首次安装或脚本代码更新仍需加载新脚本。
- `/audio/status` 独立返回宿主就绪、程序文件摘要、Live 开关及桌面最近的加载回执。适配器仅在安装、实际音频调用和卸载时发送经过能力鉴权的空体状态报告，不提交聊天、音频或转录文本。`adapter.state=unconfirmed` 表示该进程尚未收到回执；`ready` 也只代表适配器报告加载，不代表麦克风或真实 Live 通话验收成功。
- 上传要求正确能力和精确 Host；独立宿主使用无 Origin 的桌面主进程 HTTP 服务。底层听写 helper 仍支持显式配置的非 opaque Origin，不配置通配 CORS。
- 听写并发最多两个、上传26 MiB、响应1 MiB、总预算120秒。取消、断开、卸载和停机传递到网关；超过并发返回429。只接受字符串 `text` 的 JSON；401/403/413/422/429等保留错误，拒绝 HTML、坏 JSON 和重定向。
- 听写只匹配 `POST /transcribe` 与经核验的官方绝对地址，保持 multipart 字节。删除官方 Cookie、Authorization、Attach-Auth/Integrity 指令后，使用独立 provider 的能力认证；不改变桌面官方认证边界。其它请求不额外获取配置，保留原参数与同步返回方式。
- 安装禁止重复包装；卸载取消在途请求、移除面板/监听器、释放音轨/音频上下文。若第三方后来替换 fetch，停用自身选路而不覆盖其修改。

### 分段听写与本机朗读

录音只在用户点击后开始，每4秒封装独立 PCM16 WAV，按实际采样率和字节长度上传；不切割 WebM 伪造独立音频。队列最多4段，录音最多10分钟，文本最多20000字；过载停止，不积压无限音频。停止会排空尾段，取消会中止排队/上传并丢弃晚到结果。关闭面板或卸载会释放资源。未发送的音频只在内存，转录文本仅在面板，填入输入框不自动发送对话。

安装版本的朗读使用 Windows `System.Speech`，优先已安装中文声音。文本经受本地引导能力保护的 `/audio/speech` 到宿主，再通过 stdin 交给固定 PowerShell 语音脚本；不拼接到命令行、不落文本/音频文件、不请求网关。一次只允许一个合成任务，每段最多400字、合成20秒、音频8MiB。页面顺序播放 WAV，播放完成/取消后撤销对象 URL；取消或停机终止合成子进程。没有本机声音/引擎时明确报错，不使用远端 TTS。

底层组件另保留仅选择 `localService=true` 声音的浏览器 `LocalSpeaker`，但安装路径不依赖它：部分构建会列出声音而不触发播放完成事件。本机朗读不提供云端 `/audio/speech`，与网关 STT 或 Live 的收费无关。

### 可选 Live 与验收边界

安装缺省只启用听写/面板，`-EnableLive` 才启用创建请求改道和写入 app-server 音频配置。开启前须确认分组许可、账号模型白名单及真实上游资格；只改开关不代表资格成立。适配器精确匹配当前桌面的 `/wham/realtime/calls?intent=quicksilver&architecture=avas`，保持 Session JSON 字节、模型、voice、delegation、真实关联字段及已有证明，不生成证明。

创建只接受201、SDP及同源合法 `rtc_*` Location。本地 Sideband 只接受宿主创建的 callId 和相同网关 Key，保留 callId、不跨账号重新创建。控制帧通过流背压传递，不记录正文；校验上游 Upgrade 的 accept/protocol，错误/关闭/超时释放连接。Live helper 最多同时创建2条、保留4条会话、30分钟 TTL。app-server 的 `experimental_realtime_ws_base_url` 指向本地 `/v1`；只有 helper 成功绑定后安装器才修改该配置。

安装器提供 Start、Stop、Status、Rollback 和显式 EnableStartup/DisableStartup。自动启动使用当前用户 Startup 目录的独立快捷方式，隐藏启动已安装副本；不要求管理员、不改变安全策略、不定期运行、不自动录音或请求上游。维护操作使用用户级互斥锁防止重复启动。异常退出遗留状态只在原 PID 已不存在时清理，不凭旧 PID 终止进程。

Rollback 停止宿主、删除自身启动项、撤销安装能力、移走自身用户脚本，并按原有保护还原自身配置；遇到配置或启动项被后续修改时停止覆盖。实际运行状态、真实听写与 Live 上游接受应分别记录：组件测试、真实 app-server 的 loopback 接线、合成音轨浏览器测试都不替代桌面真人麦克风/双向通话验收。上游403保持拒绝，不改模型、身份或证明来伪造成功。

相关文档：[OpenAI 上游](openai_upstream.md)、[HTTP 接口](http_api.md)、[路由与结算](../domains/routing_and_billing.md)、[部署与迁移](../operations/deployment_and_migrations.md)。
