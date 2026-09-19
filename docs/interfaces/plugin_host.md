# 插件宿主接口

ConcordRoute 的插件宿主提供管理员显式管理的扩展能力。插件需安装、验签、启用并配置能力路由后，才会接入匹配的请求。

## 管理接口

所有接口位于 `/api/v1/admin/plugins`，继承管理员认证；上传、配置、测试、启停、升级、回滚、卸载和秘密授权还需要 step-up 验证。

管理员可在“系统设置 → 功能开关 → 插件管理”显示或隐藏侧边栏入口。该开关只控制菜单和前端路由可见性，不会停止已经加载的插件进程；插件启停仍由插件卡片上的独立动作控制。

- `GET /`、`GET /:id`：列出或读取安装、兼容性、运行健康和能力绑定。
- `POST /inspect`、`POST /upload`：检查或安装 `.s2plugin` 包。
- `POST /:id/enable`、`POST /:id/disable`、`POST /:id/test`：生命周期操作。
- `GET/PUT /:id/config`、`PUT /:id/routing`：配置和能力路由。
- `GET /:id/versions`、`POST /:id/upgrade`、`POST /:id/rollback`：版本历史。
- `GET /:id/host`、`GET/PUT/DELETE /:id/secret-grants`：Host API 观测与短期秘密授权。
- `GET /:id/status`：只读查询运行中插件的健康状态和插件自定义 `status_json`，不应用配置、不访问上游。
- `POST /:id/ui-session` 与 `/api/v1/plugin-ui/:token/*path`：隔离插件 UI 资源。

隔离 UI 通过只包含父页面 Origin 的 Referrer 完成跨域 `postMessage` 定址，同时继续校验 iframe
窗口、沙箱 `null` origin、Bridge Token、请求 ID 和文档代数。Bridge v1 支持配置读写/测试、
只读 `plugin.status`、尺寸调整和通知；插件资源 URL 仍由短时能力 Token 保护。

v1 传输插件可选择协商 Host Services。宿主通过 go-plugin broker 为每个已验签运行时实例提供隔离的 KV
命名空间（含 TTL、前缀列举和大小上限）；声明 `openai.oauth.outbound_transport.v1` 能力且匹配
OpenAI OAuth 的插件还可读取受限账号目录与出站身份。旧插件返回 `Unimplemented` 时继续按旧协议启动，
不会改变现有签名、发布者信任或 v2 Host API 流程。

配置读取与保存均返回标准 `{code, message, data}` 接口信封，`data` 保留插件配置对象本身；
配置内的 `code`、`message` 或 `data` 不作为宿主状态解释。路由超时覆盖值范围为
`0..min(5000, manifest.timeout_ms)`，其中 `0` 沿用清单默认值，前后端采用相同校验。

## 信任与隔离

- 未签名包默认拒绝；发布者公钥、SHA-256 和包签名在安装前校验。
- 安装、启用和发布者信任均为管理员显式动作；插件初始状态为停用。生产宿主只使用部署配置中的公钥或管理员确认后落库的信任记录，不继承参考项目的固定发布者公钥。包内公钥仅用于验签，首次安装仍需绑定包哈希和发布者指纹的确认。
- v1 使用独立进程运行；v2 根据 `plugins.v2_sandbox` 选择 process 或 container。
- 上传大小、解压大小、启动超时和容器资源有配置上限。
- PluginManager 在应用启动时恢复已启用实例，在关闭时先停止运行时再释放依赖。

## 数据库迁移

插件表使用 `283_plugins.sql` 至 `288_plugin_publishers.sql`。迁移是前向、幂等的，按文件名顺序执行；已有迁移文件不可改名或重编号。
约束重建和迁移记录由既有 runner 在同一事务提交；重复执行不得清空安装、配置、版本或授权数据。

## 当前边界

应用通过 `ProvidePluginManager` 装配 KV、受限账号目录和 OpenAI 网关，Wire 生成结果可由手写 provider 重建。OpenAI OAuth HTTP 出站由统一边界检查插件路由，覆盖 Responses、兼容转换和 WS HTTP Bridge；未命中启用路由时继续使用原有 HTTP/TLS 出站。

卡片上的停用操作关闭能力绑定、移除当前路由，再等待在途请求最多 10 秒并结束进程。隐藏插件管理菜单只改变界面可见性，不等价于停用。插件 UI 的只读状态接口不触发上游模型请求，运行健康也不证明某次业务请求已被插件处理或被上游接受。
