# TokenRouter Continuity 一致性指南

本文说明本分支解决的问题、各类一致性配置之间的关系，以及部署时如何避免生成互相矛盾的客户端特征。

## 目标与边界

TokenRouter Continuity 主要修改自 TokenRouter，目标是在多账号调度和代理转发中保持以下信息连续且可解释：

- 账号对应的 installation/device 身份；
- 账号主 session 与对话 thread、turn、window；
- prompt cache key 与对话边界；
- User-Agent、TLS ClientHello、ALPN 和 HTTP 协议；
- 账号绑定的网络出口；
- 用量、缓存命中、TTFT、错误和 session 日志。

这些功能用于减少身份漂移、缓存失效、重复探针、错误调度和观测偏差。它们不会改变上游服务授予账号的真实额度，也不会保证特定限流、风控或延迟结果。

## Cockpit 身份模型

默认 Cockpit 模式使用以下拓扑：

```text
账号
└─ 稳定 installation/device
   └─ 稳定主 session
      ├─ 对话 A → 稳定 thread/window/cache key → 多个 turn
      ├─ 对话 B → 稳定 thread/window/cache key → 多个 turn
      └─ 对话 C → 稳定 thread/window/cache key → 多个 turn
```

所有稳定值都在账号作用域内派生。同一个客户端对话切换到另一个上游账号时，新账号获得自己的 installation、session、thread 和缓存键，不直接复用旧账号的上游身份。

## TLS、UA 与 HTTP 协议

TLS 模板不是单独的“开关”，而是一组需要互相匹配的信号：

- UA 中声明的系统、CPU 架构和客户端版本；
- TLS 最低/最高版本、cipher suites 和 extensions；
- GREASE、SNI、supported groups、signature algorithms；
- ALPN 中的 `h2`、`http/1.1` 及其实际协商结果；
- 请求最终使用的 HTTP/1.1 或 HTTP/2 行为；
- 直连、HTTP Proxy、HTTPS Proxy 等传输路径。

推荐从实际客户端环境抓取 ClientHello，再将 UA 与抓包时间、客户端版本和操作系统一起记录。不要只复制一份公开 TLS 扩展列表后搭配完全不同的 UA，也不要在声明 `h2` 后强制所有请求继续使用 HTTP/1.1。

模板保存前应检查 TLS 版本范围、扩展重复、必要扩展依赖、ALPN 和数值边界。HTTPS Proxy 路径也应继续使用目标站点的 uTLS 模板，避免代理连接存在时静默退回 Go 标准 TLS。

## 代理出口策略

账号的应用层身份稳定时，网络出口仍可能成为变化来源：

- `select`：管理员明确选择一个节点，最容易保持出口稳定；
- `fallback`：当前节点异常后按顺序切换，适合稳定优先并保留容灾；
- `url-test`：定期选择延迟较低的节点，出口可能随测速结果变化；
- `load-balance`：请求分散到多个节点，出口可能频繁变化。

长期账号建议使用 `select`，或使用节点地域和运营商尽量一致的 `fallback`。`load-balance` 更适合无账号身份依赖的普通流量，不建议将同一个长期账号分散到跨国家或跨运营商节点。

## 额度异常排查

出现“额度消耗不稳定”时，应同时检查：

1. 是否产生重试、协议转换回退或后台额度探针；
2. prompt cache key 和 thread 是否在同一对话内保持稳定；
3. 调度切换账号后是否正确进入新账号作用域；
4. TLS、UA、ALPN、HTTP 协议是否一致；
5. 账号绑定出口是否频繁变化；
6. Usage Log 的 session、模型、reasoning effort 和 token 是否记录最终上游值；
7. 429/529、上游 4xx/5xx、TTFT 和缓存命中率是否同步变化。

多会话看起来“额度更耐用”可能来自缓存命中、上下文长度、模型路由、reasoning effort、重试次数或上游动态策略差异，不能仅根据对话数量判断。

## 上游与参考项目

本分支主要基于 TokenRouter 修改，并持续吸收 Sub2API 上游修复。Cockpit 身份模型参考 cockpit-tools 的实现思路；代理和出口一致性工作参考 LightBridge 的相关设计。具体适配均以本仓库当前代码、测试和发布记录为准。
