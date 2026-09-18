# 插件宿主接口

ConcordRoute 的插件宿主只提供管理员显式管理的扩展能力，不自动安装、启用或把插件接入普通网关请求链。

## 管理接口

所有接口位于 `/api/v1/admin/plugins`，继承管理员认证；上传、配置、测试、启停、升级、回滚、卸载和秘密授权还需要 step-up 验证。

- `GET /`、`GET /:id`：列出或读取安装、兼容性、运行健康和能力绑定。
- `POST /inspect`、`POST /upload`：检查或安装 `.s2plugin` 包。
- `POST /:id/enable`、`POST /:id/disable`、`POST /:id/test`：生命周期操作。
- `GET/PUT /:id/config`、`PUT /:id/routing`：配置和能力路由。
- `GET /:id/versions`、`POST /:id/upgrade`、`POST /:id/rollback`：版本历史。
- `GET /:id/host`、`GET/PUT/DELETE /:id/secret-grants`：Host API 观测与短期秘密授权。
- `POST /:id/ui-session` 与 `/api/v1/plugin-ui/:token/*path`：隔离插件 UI 资源。

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

本次移植没有把插件预处理、UA/TLS、Cockpit/session/thread/window/turn 收敛、Turn-State、292 或自动探活/额度查询接入普通网关路径。插件宿主上线不会改变现有请求行为。
