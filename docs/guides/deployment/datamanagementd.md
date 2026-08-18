# datamanagementd 部署说明（数据管理）

本文说明如何把可选的 `datamanagementd` 进程接入 ConcordRoute，以启用管理后台的“数据管理”功能。

> 当前仓库不包含 `datamanagement/` 源码目录，也不发布该进程的构建产物。因此，不能直接在本仓库执行 `make build-datamanagementd`，`install-datamanagementd.sh --source` 也会因缺少源码目录而失败。只有在另行取得与当前 ConcordRoute 版本兼容的二进制或完整源码后，才能部署此功能。

## 运行边界

- ConcordRoute 固定探测 `/tmp/sub2api-datamanagement.sock`。
- 只有 Unix Socket 可连接且健康检查成功时，后台才会启用数据管理。
- `datamanagementd` 使用 SQLite 保存自身元数据，不使用 ConcordRoute 的 PostgreSQL 主库。
- 宿主机需要提供 `pg_dump`、`redis-cli`；使用 `source_mode=docker_exec` 时还需要 `docker`。

## 使用现成二进制安装

先确认二进制来源、版本和校验值，再从仓库根目录运行安装脚本：

```bash
sudo ./deploy/install-datamanagementd.sh --binary /absolute/path/to/datamanagementd
```

脚本会把二进制安装到 `/opt/sub2api/datamanagementd`，创建数据目录并安装仓库内的 `deploy/sub2api-datamanagementd.service`。完成后检查服务和 Socket：

```bash
sudo systemctl status sub2api-datamanagementd
sudo journalctl -u sub2api-datamanagementd -f
sudo test -S /tmp/sub2api-datamanagement.sock
```

若你持有另一个包含 `datamanagement/` 目录的完整源码包，可以使用脚本的 `--source` 模式；该模式不适用于当前仓库。

## Docker 联动

ConcordRoute 运行在 Docker 容器中时，需要把宿主机 Socket 映射到容器内的同一路径。应在 `datamanagementd` 已启动并创建 Socket 后再启动应用容器：

```yaml
services:
  sub2api:
    volumes:
      - /tmp/sub2api-datamanagement.sock:/tmp/sub2api-datamanagement.sock
```

建议把挂载放在 `docker-compose.override.yml`，避免修改发布 Compose 文件。若 Docker 在 Socket 创建前把宿主机路径建成目录，先停止容器、删除该空目录、启动 `datamanagementd`，再重新创建应用容器。

## 验证

1. 确认 systemd 服务为 `active`，且 Socket 文件存在。
2. 容器部署时，在应用容器内确认同一路径也是 Unix Socket。
3. 打开管理后台“数据管理”，确认代理状态为已启用。
4. 分别执行一个最小 PostgreSQL 和 Redis 备份任务，确认宿主机依赖与文件权限正常。

工程边界和部署资产现状见 [部署与数据库迁移](../../operations/deployment_and_migrations.md)。
