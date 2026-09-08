# Codex 实验上下文管理与中转兼容边界

## 结论

`features.context_management.experimental_mode` 是 Codex 客户端功能配置，
不是需要添加到 Responses 请求体的生成参数，也不是开启服务器缓存的开关。
工具调用能正常转发，不等于已实现完整的原生历史与笔记服务。

本文针对官方 `rust-v0.153.4`，源码提交
`3d2ee51ca2d5db578f328aa75e20aa22c0197c9a`；后续版本需重新核对条件。

## 客户端开关与前提

官方配置形式：

```toml
[features]
context_management.experimental_mode = true
```

该版本在客户端启动会话时检查：

- 认证为 ChatGPT OAuth，套餐为 Plus、Pro 或 ProLite。
- provider 支持 Codex backend 路由并要求 OpenAI 认证。
- 不配置 provider 的 `env_key`、`experimental_bearer_token`、自定义 `auth` 或 AWS 认证。
- provider 名称为 `OpenAI`，自定义 base URL 若存在须以 `/backend-api/codex` 结尾。
- TokenBudget 功能未被客户端其它约束禁用。

因此，客户端使用中转 API Key，并不因为服务器后面配置了 OAuth 账号而满足这些条件。
仅保存配置、修改 UA 或提高客户端版本号，都不代表功能已经启用。
此说明不要求修改客户端登录状态，亦不将中转身份伪装成满足官方套餐条件。

## 功能由两部分组成

### Responses 承载

客户端可向模型暴露 `new_context`、`get_context_remaining` 等工具，
并通过 developer 消息携带上下文窗口信息。工具执行和窗口推进由客户端维护。

原生 history/notes 扩展还会暴露 `history`、`notes` 命名空间工具，
其工具结果可能包含 `encrypted_content`。这些不透明内容与窗口提示文本应保持原样。

ConcordRoute 已有的普通 OAuth Responses、OAuth passthrough、
WS `passthrough` 和 `ctx_pool` 转发路径支持上述承载格式。
回归测试位于 `backend/internal/service/openai_codex_context_management_test.go`。
测试使用合成请求和本地模拟上游，不验证远端功能开通状态。

### 独立的原生历史与笔记服务

完整功能还会请求：

- `alpha/notes/v2/thread_hint`
- `alpha/history/v2/list_windows`、`list_items`、`read_item`、`search_contents`
- `alpha/notes/v2/list_files_by_prefix`、`read_file`、`search_contents`
- `alpha/notes/v2/append_to_file`、`write_file`

这些是独立的 Codex backend POST 接口，不是 `/v1/responses` 的参数。
客户端将 `context.session_id`、`context.current_agent_name` 写入请求，
并使用工具结果截断策略头；部分操作还有加密参数标志。

**当前 ConcordRoute 尚未实现这些专用路由，本次没有宣称完整原生功能已就绪。**
现有 Alpha Search 路由也不等于通用 `alpha/*` 代理。

后续接入应作为独立功能实现并测试：

1. 使用明确的路由白名单，不开放任意 backend 路径。
2. API Key 所有者、分组、根会话、子代理与上游账号之间建立稳定且可验证的绑定。
3. 历史请求使用与生成请求一致的会话身份；不得随机换账号读取或写入笔记。
4. 隔离不同用户的历史与笔记；禁止客户端指定任意他人会话或上游账号。
5. 保留不透明内容和相关协议头，明确超时、结果大小、审计和生命周期策略。
6. 先解决客户端支持与认证条件，再做真实端到端验证；不通过虚构模型响应宣称启用。

## 与缓存窗口修复的关系

`first_window_id`、`previous_window_id`、`window_number` 的元数据收敛，
以及压缩后 `prompt_cache_key` 的相邻窗口继承，不等于历史或笔记存储。
本次不更改已有窗口修复、TLS 模板、UA、账号绑定或服务器配置。

## 来源

- [Sub2API #6741](https://github.com/Wei-Shaw/sub2api/issues/6741)
- [官方客户端启用条件](https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/core/src/session/token_budget.rs)
- [官方 provider 路由条件](https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/model-provider-info/src/lib.rs)
- [原生历史/笔记扩展](https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/ext/history-notes/src/extension.rs)
- [原生历史/笔记请求协议](https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/ext/history-notes/src/backend.rs)
