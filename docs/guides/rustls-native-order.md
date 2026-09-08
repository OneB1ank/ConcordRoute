# TLS 模板的 rustls 原生排序开关

## 使用范围

在「TLS 指纹模板」创建或编辑窗口中，使用「启用 rustls 原生排序」开关。
对应 API / 导入导出 YAML 字段：`rustls_native_order`。

- **开启**：首个 ClientHello 的扩展排列优先使用 rustls **0.23.36** 的算法；
  `extensions` 仍决定扩展集合，但其中手工填写的排列不再决定普通扩展的发送顺序。
- **关闭或字段缺失**：完全保留原有固定排序，不自动修改已有模板、全局 UA 或账号绑定。
- 更新接口省略该字段时，保留模板已有值；显式 `false` 才关闭。
- 新建模板、旧数据库记录、单次抓包创建的模板均默认关闭。
- 单次采集只证明那次握手的排列，不足以判断客户端采用了哪一种动态排序算法。
  不根据 UA 名称、操作系统或“这是 OpenAI 请求”自动开启。

## 算法来源与优先级

参考 rustls 0.23.36：

- `rustls/src/client/hs.rs`：每个新握手的 16 位排序种子。
- `rustls/src/msgs/handshake.rs`：
  `order_insensitive_extensions_in_random_order`、`low_quality_integer_hash`
  与 `used_extensions_in_encoding_order`。
- 官方源码：<https://github.com/rustls/rustls/tree/v/0.23.36>
- 上游设计参考：<https://github.com/Wei-Shaw/sub2api/pull/6380>

这不是 PR 中的通用 `Shuffle`。普通扩展按种子和扩展 ID 的 32 位整数混合值
进行稳定排序，溢出语义与 Rust 的 `wrapping_add` 一致。
排序辅助函数按 outer_extensions、ECH、PSK 的顺序保留特殊后缀。

种子来自 `crypto/rand`，只在创建新连接的 ClientHello 配置时生成一次。
不读取 UA、授权令牌、账号 ID、业务 `session_id`、`prompt_cache_key` 或压缩窗口字段。
连接复用不会每请求重新排序。种子没有写入模板、数据库或连接池缓存键。

连接池键包含开关，避免固定和原生排序模式复用错误的连接。
关闭时 JSON 省略新字段，保留旧模板的原池键。
HTTP、HTTPS CONNECT、SOCKS5 和直连共享排序入口；
WS / HTTP/1.1 的 ALPN 副本保留该开关。

## 已知边界

**这是原生排序算法移植，不是将整个 TLS 后端替换为 rustls。**

- 已验证 TLS 1.2、TLS 1.3 完整握手和普通 key_share HelloRetryRequest。
- 普通 key_share HRR 不重新生成排序种子，第二个 ClientHello 保持扩展顺序。
- **带 Cookie 的 HRR**：有界观察首个 ServerHello，在原始字节交给 uTLS 前，
  按相同种子为新增 Cookie 预留正确位置；由 uTLS 自行校验并填充内容。
  支持 TCP / TLS 记录分片，完成后停止观察。生产代码不重写网络字节或 transcript。
  此适配针对 uTLS 1.8.2 的公开 CookieExtension 行为；升级该依赖时应重跑回归。
- 当前没有真实 ECH inner/outer 压缩块，也没有 PSK binder/恢复状态生成能力。
  排序函数的后缀单测不等于已支持这些完整握手功能。
- 原生模式禁止自动或显式 GREASE 扩展，也禁止手工声明 PSK(41)、
  首包 Cookie(44) 和真实 outer_extensions(64768)。
  PSK 模式扩展(45)不受该限制。
- 不调整密码套件、曲线、签名算法、ALPN、TLS 版本、UA 或业务 ID。
  本功能也不承诺账户状态、模型回答质量或额度会因此改善。

Codex 配置自定义 CA 时选择 rustls 的路径，不代表 Desktop 经本机应用代理后的
出口也是 rustls；后者由实际终止并重建 TLS 的进程决定。已有代理样本应独立保留。

## 验证与回退

- 将 rustls 原始整数混合函数交给 Rust 编译器运行，以 11 个实际扩展和全部
  65,536 个种子生成参考输出；Go 测试逐项累计输出，与独立参考 SHA-256 比对。
- 验证特殊后缀、固定模式不变、模板切片不被修改、连接池键隔离。
- 回环抓包检查四种建连路径；受信任本地证书验证 TLS 1.2 / 1.3、连接复用及
  UA、测试账号头、测试会话正文不变。无生产账号和真实令牌参与。
- Cookie HRR 回环采集验证真实第二个 ClientHello 的位置及 Cookie 回显。
  另用 OpenSSL 3.5.5 stateless 服务端验证 Cookie、证书、Finished 和应用数据。
  其 CLI stateless 分支会拒绝 dummy CCS，因此**仅在该测试夹具**中过滤此兼容记录；
  不修改 ClientHello，不改变生产 CCS 行为，也不宣称未加夹具的该 CLI 互通已通过。
- 模板 CRUD 覆盖数据库回读、服务重建、部分更新保留、显式关闭与冲突拒绝。
- 前端覆盖开关、旧 YAML 导入兼容、保存和 GREASE 冲突提示。

可先关闭模板开关回到原有固定排序，无需删除模板或改变 UA。
数据库迁移 `276_tls_fingerprint_rustls_native_order.sql` 仅新增默认关闭的布尔列。
回退程序版本时可以保留这列，避免破坏已经保存的模板设置。
