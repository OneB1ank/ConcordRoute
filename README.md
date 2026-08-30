<div align="center">
  <img src="assets/logo.svg" alt="ConcordRoute" width="112" />

  <h1>ConcordRoute</h1>

  <p><strong>基于 TokenRouter 的身份、传输与出口一致性增强分支</strong></p>

  <p>
    <a href="https://github.com/OneB1ank/ConcordRoute/actions/workflows/backend-ci.yml"><img src="https://github.com/OneB1ank/ConcordRoute/actions/workflows/backend-ci.yml/badge.svg" alt="CI" /></a>
    <a href="https://github.com/OneB1ank/ConcordRoute/releases"><img src="https://img.shields.io/github/v/release/OneB1ank/ConcordRoute?display_name=tag" alt="Release" /></a>
    <a href="https://github.com/OneB1ank/ConcordRoute/pkgs/container/concordroute"><img src="https://img.shields.io/badge/container-ghcr.io%2Foneb1ank%2Fconcordroute-2496ED?logo=docker&logoColor=white" alt="Container" /></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-LGPL--3.0--or--later-4c1.svg" alt="License: LGPL-3.0-or-later" /></a>
  </p>

  <p><strong>简体中文</strong> | <a href="README_EN.md">English</a></p>
</div>

## 项目定位

ConcordRoute 主要修改自 [TokenRouter](https://github.com/TokenFlux/TokenRouter)，保留其 AI API 网关、账号调度、用量统计、订阅计费和管理后台能力，并重点增强 Codex/OpenAI 多账号场景下的身份、协议和网络出口一致性。

本分支关注的“额度异常”并不是提高账号额度，而是减少账号切换、会话漂移、缓存键变化、客户端特征不一致、代理出口频繁变化以及日志口径不统一所造成的额外请求、缓存失效、错误探针和用量观测偏差。账号的真实额度、限流和动态策略仍由上游服务决定。

## ConcordRoute 特性

- **Cockpit 身份收敛**：每个账号保持稳定 installation/device 与主 session，不同对话使用独立且稳定的 thread、window 和 prompt cache key。
- **账号切换隔离**：会话标识按账号作用域派生，调度切换账号时不会直接继承另一个账号的上游身份。
- **TLS 与 UA 一致性**：支持自定义 uTLS ClientHello、ALPN、TLS 版本与扩展校验，并让普通 HTTPS、HTTPS Proxy 和相关连接路径复用一致配置。
- **稳定代理出口**：集成 Clash/mihomo 节点、策略和账号绑定，支持为账号固定出口或配置受控故障转移。
- **额度与延迟观测**：统一 session ID、reasoning effort、TTFT、缓存命中和用量记录口径，减少“功能正常但日志为空或误记”的情况。
- **导入与配置一致性**：补齐 Codex Session/PAT、OAuth 默认配置、模型白名单和模型映射的持久化链路。

## 推荐使用方式

1. **自行抓取客户端特征**：优先从自己实际使用的系统和客户端抓取 TLS ClientHello，并记录真实 UA。公开样本适合研究结构，不应直接当作所有环境的固定模板。
2. **保持整组特征一致**：UA 中的系统与客户端版本、TLS cipher/extension、ALPN、HTTP 协议和实际运行环境应互相匹配。只修改其中一个字段，反而会产生矛盾特征。
3. **每个账号使用稳定出口**：长期使用的账号建议绑定稳定节点；需要切换时优先使用受控 `fallback`。会频繁跨国家、运营商或线路轮换的 `load-balance` 不适合作为账号身份出口。
4. **默认使用 Cockpit 模式**：它在账号级稳定设备和主会话，在对话级隔离 thread 与缓存键。已有特殊客户端集成可按实际情况选择 session、full 或 off。
5. **根据观测调整**：结合请求错误、TTFT、缓存命中、429/529、代理健康和账号用量判断问题，不把单一指标直接解释为额度变化。

### Codex 客户端版本与 UA

- **推荐 Codex 0.151.0 及更高版本**。较新的客户端会携带更完整的会话、回合和上下文窗口字段，压缩与重连时的生命周期信息更完整；旧版本仍可走兼容入口，但字段覆盖范围较小。
- **UA 由部署者自行设置**。在管理端的全局 `openai_codex_user_agent`、TLS 指纹路由规则 `upstream_user_agent`，以及 token/reset 专用 UA 字段中，填写与实际客户端、操作系统、架构和版本匹配的值。
- **按客户端族分别设置**。`codex-cli/`、`codex-tui/`、`codex_cli_rs/` 和 `Codex Desktop/` 使用各自的 UA 与 `originator` 配对，不把多个客户端族压成同一字符串。
- **UA 与 TLS 成组配置**。UA 声明的系统、架构、版本应与 ClientHello、ALPN 和实际 HTTP 传输一致；升级客户端时同步复核 UA 和 TLS profile。

示例模板（将占位符替换为实际值）：

```text
codex-cli/<version> (<os> <arch>)
Codex Desktop/<version> (<os> <arch>)
```

管理端留空时沿用账号、入站请求或系统兜底值；显式设置后，仅在匹配的 OpenAI/Codex 路由上使用。

## OpenAI 反代分支的优势

- **缓存命中链路清晰**：显式 `prompt_cache_key` 优先保留；同一 `session/thread/window` 暂时省略 key 时复用最近绑定；收到新的显式 key 后立即切换缓存命名空间，不让缓存键反向改变 session/thread。
- **指纹收敛范围可控**：installation/device 维持账号级稳定，会话与 thread 按对话隔离，turn 按请求更新；HTTP、SSE、passthrough 和 WebSocket 使用同一 fingerprint snapshot。
- **统一出站**：账号绑定的 TLS profile、UA、originator、ALPN、HTTP 协议和代理出口从同一决策快照产生，减少直连、HTTP Proxy、HTTPS Proxy 之间的漂移。
- **故障转移可诊断**：切换账号时重新进入目标账号作用域，保留客户端对话语义并隔离上游身份；日志同时记录最终模型、session、TTFT、缓存命中和传输类型。
- **OpenAI 专用边界**：Codex 身份、Responses、压缩、额度快照和 WebSocket 逻辑只在满足 OAuth/Codex 条件时启用，其他平台继续使用各自的协议路径。

这些设计主要减少反代层产生的身份漂移、缓存冷启动和重复请求；账号真实额度、并发策略和上游动态策略仍由上游服务决定。

更详细的差异和配置原则见 [指纹、身份与出口一致性说明](docs/guides/fingerprint-consistency.md)。

## 上游与致谢

本项目主要基于以下开源项目持续修改、同步和适配：

- [TokenRouter](https://github.com/TokenFlux/TokenRouter)：本项目的主要代码基础和持续同步上游。
- [Sub2API](https://github.com/Wei-Shaw/sub2api)：TokenRouter 的上游基础，提供核心网关与账号管理架构。
- [cockpit-tools](https://github.com/jlcodes99/cockpit-tools)：为 Codex 设备、会话、thread 与缓存键一致性设计提供参考。
- [LightBridge](https://github.com/WilliamWang1721/LightBridge)：为代理接入、网络出口与客户端一致性实现提供参考。

感谢上述项目的维护者、贡献者以及相关 issue、PR 和抓包研究的分享者。本仓库中的适配、修复和新增功能由本分支独立维护；各上游项目名称和商标归其各自权利人所有。

## 基础功能

- 多上游、多账号、API Key、用户、团队和分组管理
- 模型映射、模型白名单、请求路由和故障转移
- 并发、速率、余额、订阅和配额控制
- 用量统计、运行观测、备份及管理后台
- Anthropic、OpenAI、Gemini、Antigravity、Grok / xAI 和 Qoder 适配

## 部署与文档

- [部署指南](docs/guides/deployment/index.md)
- [Docker 镜像说明](deploy/DOCKER.md)
- [使用与运维指南](docs/guides/index.md)
- [接口文档](docs/interfaces/index.md)
- [工程文档](docs/index.md)

## 许可证

本项目依据 [GNU Lesser General Public License v3.0 或更高版本](LICENSE) 发布，并保留上游项目要求的版权和许可证声明。

Copyright (c) 2026 Wesley Liddick & TokenFlux
