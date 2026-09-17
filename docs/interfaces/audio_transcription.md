# 听写转录

本文定义非流式录音上传、上游转录、分组准入与时长计费。不包含 Live WebRTC、实时 partial transcript 或桌面程序认证修改。

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

相关文档：[OpenAI 上游](openai_upstream.md)、[HTTP 接口](http_api.md)、[路由与结算](../domains/routing_and_billing.md)、[部署与迁移](../operations/deployment_and_migrations.md)。
