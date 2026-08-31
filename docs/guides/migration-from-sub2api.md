# 从 Sub2API 迁移到 ConcordRoute

本文用于把现有 Sub2API 实例迁移到 ConcordRoute。迁移分成两类：

1. **原服务器、原 PostgreSQL 直接升级**：保留现有数据库、Redis 和数据目录，只替换应用版本。
2. **迁移到新服务器**：在旧服务器导出 PostgreSQL，复制配置和数据目录，在新服务器恢复后再启动 ConcordRoute。

示例以 `deploy/docker-compose.local.yml` 为准；它使用宿主机目录保存 `data/`、`postgres_data/` 和 `redis_data/`，便于备份。使用命名卷或 systemd 的实例，按文中的路径替换为实际卷或 `/opt/sub2api`。

## 迁移前的原则

- 先固定维护窗口，停止写入，再做最终备份。
- PostgreSQL 是业务数据的权威来源；不要只复制应用目录就宣布迁移完成。
- `schema_migrations` 由 ConcordRoute 启动时自动推进。不要手工删除、改名或修改迁移记录来绕过错误。
- 目标版本首次启动前，只运行一个应用实例；迁移成功后再扩容。
- 生产环境固定 ConcordRoute 的版本标签或镜像摘要，不要在迁移窗口使用漂移的 `latest`。
- 旧服务器和旧备份在验证完成前保留，回滚通过恢复数据库和配置完成，而不是只回退镜像。

## 必须保留的配置

从旧实例复制 `.env`、`config.yaml` 或 systemd 环境文件中的有效值。至少保留：

| 配置 | 作用 | 迁移要求 |
| --- | --- | --- |
| `JWT_SECRET` / `jwt.secret` | 签发和校验登录令牌 | 必须保持原值，否则现有登录令牌全部失效 |
| `TOTP_ENCRYPTION_KEY` / `totp.encryption_key` | 加密 2FA 密钥及部分敏感配置 | 必须保持原值，否则已登记的 2FA 和加密配置无法解密 |
| `DATABASE_*` / `database.*` | PostgreSQL 连接 | 新服务器按实际主机、端口和凭据更新 |
| `REDIS_*` / `redis.*` | Redis 连接 | 同机升级保持不变；换机按新 Redis 更新 |
| `DATA_DIR`、`CONFIG_FILE` | 配置、安装锁和本地运维数据位置 | 复制目录并保持路径一致，或同步修改启动参数 |
| `ADMIN_EMAIL`、OAuth、SMTP、支付和对象存储配置 | 管理登录及外部集成 | 从旧环境逐项核对，敏感值不要写入仓库 |
| `webauthn.rp_id`、`webauthn.rp_origins` | Passkey 依赖方边界 | 公开域名不变时保持原值；换域名前先安排重新注册 |

环境变量与 YAML 同时存在时，不要保留互相冲突的副本。迁移后用实际启动命令生成的配置进行验证。

## 0. 准备备份目录

在旧服务器执行：

```bash
cd /srv/sub2api-deploy
set -a
. ./.env
set +a
COMPOSE_FILE=${COMPOSE_FILE:-docker-compose.local.yml}
BACKUP_DIR="migration-backup-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR"

# 保存部署输入；这些文件含有密钥，权限保持为仅当前用户可读。
cp .env "$BACKUP_DIR/.env"
test -f data/config.yaml && cp data/config.yaml "$BACKUP_DIR/config.yaml" || true
chmod 600 "$BACKUP_DIR/.env" "$BACKUP_DIR/config.yaml" 2>/dev/null || true

# 保存解析后的 Compose，便于回滚时确认实际镜像、卷和环境变量。
docker compose -f "$COMPOSE_FILE" config > "$BACKUP_DIR/compose.resolved.yaml"
chmod 600 "$BACKUP_DIR/compose.resolved.yaml"
```

记录当前版本和容器状态：

```bash
docker compose -f "$COMPOSE_FILE" ps
docker compose -f "$COMPOSE_FILE" images
docker compose -f "$COMPOSE_FILE" logs --tail=200 sub2api > "$BACKUP_DIR/sub2api-before.log"
```

## 1. 原服务器、原数据库直接升级

此方式适合 PostgreSQL 和 Redis 继续留在原服务器的场景。

### 1.1 停止应用并做最终备份

先停止应用，保留 PostgreSQL 和 Redis 运行：

```bash
docker compose -f "$COMPOSE_FILE" stop sub2api

docker compose -f "$COMPOSE_FILE" exec -T postgres sh -lc \
  'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' \
  > "$BACKUP_DIR/postgres.dump"

docker compose -f "$COMPOSE_FILE" exec -T postgres sh -lc \
  'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -At -F "," -c \
  "SELECT ''users'',count(*) FROM users UNION ALL SELECT ''accounts'',count(*) FROM accounts UNION ALL SELECT ''api_keys'',count(*) FROM api_keys UNION ALL SELECT ''groups'',count(*) FROM groups;"' \
  > "$BACKUP_DIR/row-counts-before.csv"
docker compose -f "$COMPOSE_FILE" exec -T postgres sh -lc \
  'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT filename,checksum,applied_at FROM schema_migrations ORDER BY applied_at DESC LIMIT 20;"' \
  > "$BACKUP_DIR/schema-migrations-before.txt"

docker run --rm -v "$PWD/$BACKUP_DIR:/backup:ro" postgres:18-alpine \
  pg_restore -l /backup/postgres.dump > "$BACKUP_DIR/postgres.toc"
```

如果实例使用本地 Redis 目录，并且希望保留会话、限流和调度状态，再保存一次 Redis 数据：

```bash
docker compose -f "$COMPOSE_FILE" exec -T redis redis-cli SAVE
docker compose -f "$COMPOSE_FILE" stop redis
tar -czf "$BACKUP_DIR/redis-data.tgz" redis_data
docker compose -f "$COMPOSE_FILE" start redis
```

### 1.2 替换应用版本

保留 `.env` 和数据目录，只更新 Compose 中 `sub2api` 服务的镜像。镜像地址和标签以目标版本随附的 `deploy/docker-compose*.yml` 为准，例如：

```yaml
services:
  sub2api:
    image: ghcr.io/oneb1ank/concordroute:TARGET_TAG
```

同时对照目标版本的 `.env.example` 合并新增变量；不要用空模板覆盖旧 `.env`。完成后检查解析结果：

```bash
docker compose -f "$COMPOSE_FILE" config > "$BACKUP_DIR/compose.target.resolved.yaml"
docker compose -f "$COMPOSE_FILE" pull sub2api
```

### 1.3 启动并让迁移自动执行

```bash
docker compose -f "$COMPOSE_FILE" up -d sub2api
docker compose -f "$COMPOSE_FILE" logs -f sub2api
```

看到服务完成初始化后，执行验证：

```bash
curl --fail --silent --show-error http://127.0.0.1:${SERVER_PORT:-8080}/health
docker compose -f "$COMPOSE_FILE" ps
docker compose -f "$COMPOSE_FILE" exec -T postgres sh -lc \
  'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT count(*) AS applied_migrations FROM schema_migrations;"'
```

重点检查：管理员登录、已有 API Key、已启用 2FA 的账号、一个非流请求、一个流式请求、用量记录、余额/订阅扣费、上游账号调度和后台任务。确认通过后再执行：

```bash
docker compose -f "$COMPOSE_FILE" up -d
```

### 1.4 直接升级失败时

如果日志出现迁移 SQL 错误、checksum mismatch 或启动探针持续失败：

```bash
docker compose -f "$COMPOSE_FILE" stop sub2api
docker compose -f "$COMPOSE_FILE" logs --tail=300 sub2api > "$BACKUP_DIR/sub2api-failed.log"
```

不要修改 `schema_migrations`，也不要让旧版本和目标版本同时写同一数据库。按“回滚”章节恢复升级前数据库和配置，先在副本数据库上完成兼容性检查，再安排下一次窗口。

## 2. 迁移到新服务器并恢复 PostgreSQL

此方式把应用、数据库和 Redis 迁移到新主机。PostgreSQL 使用逻辑备份恢复，不直接跨主机复制 `postgres_data/`，这样可避免 PostgreSQL 大版本或底层文件格式差异。

### 2.1 旧服务器导出

停止旧应用，完成一次最终导出：

```bash
cd /srv/sub2api-deploy
set -a; . ./.env; set +a
COMPOSE_FILE=${COMPOSE_FILE:-docker-compose.local.yml}
BACKUP_DIR="migration-backup-final"
mkdir -p "$BACKUP_DIR"
docker compose -f "$COMPOSE_FILE" stop sub2api
docker compose -f "$COMPOSE_FILE" exec -T postgres sh -lc \
  'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' \
  > "$BACKUP_DIR/postgres.dump"
docker run --rm -v "$PWD/$BACKUP_DIR:/backup:ro" postgres:18-alpine \
  pg_restore -l /backup/postgres.dump > "$BACKUP_DIR/postgres.toc"

tar -czf "$BACKUP_DIR/app-data.tgz" .env data
sha256sum "$BACKUP_DIR/postgres.dump" "$BACKUP_DIR/app-data.tgz" \
  > "$BACKUP_DIR/SHA256SUMS"
```

将 `migration-backup-final/` 传到新服务器，并在传输后校验：

```bash
sha256sum -c SHA256SUMS
```

### 2.2 新服务器准备目标配置

在新服务器准备目标版本的 Compose 文件：

```bash
mkdir -p /srv/concordroute && cd /srv/concordroute
cp /path/to/target/deploy/docker-compose.local.yml ./docker-compose.local.yml
tar xzf /path/to/migration-backup-final/app-data.tgz
chmod 600 .env
```

保留旧 `.env` 中的 `JWT_SECRET` 和 `TOTP_ENCRYPTION_KEY` 原值。数据库和 Redis 若使用新主机名，更新 `DATABASE_HOST`、`REDIS_HOST`；若使用 Compose 内置服务，通常分别为 `postgres` 和 `redis`。确认 `POSTGRES_USER`、`POSTGRES_DB` 与恢复命令一致：

```bash
grep -E '^(POSTGRES_USER|POSTGRES_DB|DATABASE_HOST|REDIS_HOST|JWT_SECRET|TOTP_ENCRYPTION_KEY)=' .env \
  | sed -E 's/^(JWT_SECRET|TOTP_ENCRYPTION_KEY)=.*/\1=<preserved>/; s/^(.*)=([^=]*)$/\1=<set>/'
```

### 2.3 只启动依赖并恢复 PostgreSQL

先启动 PostgreSQL 和 Redis，应用保持停止：

```bash
docker compose -f docker-compose.local.yml up -d postgres redis
docker compose -f docker-compose.local.yml ps
```

在空数据库中恢复自定义格式备份：

```bash
set -a; . ./.env; set +a
cat /path/to/migration-backup-final/postgres.dump \
  | docker compose -f docker-compose.local.yml exec -T postgres sh -lc \
  'pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --no-owner --no-privileges --exit-on-error'
```

若目标 Compose 已自动创建了一个初始化数据库，仍以恢复命令的退出码为准。需要清空后重试时，先停止应用，再在 PostgreSQL 容器内重建目标数据库：

```bash
docker compose -f docker-compose.local.yml exec -T postgres sh -lc \
  'dropdb -U "$POSTGRES_USER" --if-exists "$POSTGRES_DB" && createdb -U "$POSTGRES_USER" -O "$POSTGRES_USER" "$POSTGRES_DB"'
```

恢复完成后再次执行 `pg_restore`，不要在应用已经接流量时重建数据库。

### 2.4 恢复 Redis（可选）

Redis 中的缓存可以冷启动；若需要保留会话、限流、调度或后台任务状态，迁移整个 Redis 数据目录，而不是只复制单个 RDB 文件：

```bash
docker compose -f docker-compose.local.yml stop redis
tar xzf /path/to/migration-backup-final/redis-data.tgz
docker compose -f docker-compose.local.yml start redis
```

跨 Redis 大版本时优先冷启动 Redis，并让 ConcordRoute 重建缓存；PostgreSQL 业务数据不受此选择影响。

### 2.5 启动目标应用并验证

```bash
docker compose -f docker-compose.local.yml up -d sub2api
docker compose -f docker-compose.local.yml logs -f sub2api
curl --fail --silent --show-error http://127.0.0.1:${SERVER_PORT:-8080}/health
```

迁移验证至少包括：

```bash
docker compose -f docker-compose.local.yml exec -T postgres sh -lc \
  'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT count(*) AS applied_migrations FROM schema_migrations;"'
docker compose -f docker-compose.local.yml exec -T postgres sh -lc \
  'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -At -F "," -c \
  "SELECT ''users'',count(*) FROM users UNION ALL SELECT ''accounts'',count(*) FROM accounts UNION ALL SELECT ''api_keys'',count(*) FROM api_keys UNION ALL SELECT ''groups'',count(*) FROM groups;"'
```

将新旧 `row-counts-before.csv` 与恢复后的结果对照。然后使用浏览器和真实 API Key 完成登录、2FA、非流/流式请求、额度扣减和后台任务抽样；健康检查通过只代表进程可服务，不代表业务迁移完整。

## systemd / 外置 PostgreSQL 实例

如果旧实例由 systemd 管理，停服和启动命令替换为：

```bash
sudo systemctl stop sub2api
sudo -u postgres pg_dump -Fc -d sub2api > "$BACKUP_DIR/postgres.dump"
sudo systemctl start sub2api
sudo journalctl -u sub2api -f
```

新服务器先使用 `createdb` 创建空库，再用 `pg_restore --no-owner --no-privileges --exit-on-error` 恢复；把 `/opt/sub2api/config.yaml`、systemd EnvironmentFile、`DATA_DIR` 和证书目录作为同一份配置清单迁移。systemd unit 的 `ExecStart` 应指向目标 ConcordRoute 二进制，且只启动一个实例完成首次迁移。

## 回滚

### 同服务器回滚

```bash
docker compose -f "$COMPOSE_FILE" stop sub2api
cp "$BACKUP_DIR/.env" .env

# 应用必须保持停止，再重建并恢复数据库。
docker compose -f "$COMPOSE_FILE" exec -T postgres sh -lc \
  'dropdb -U "$POSTGRES_USER" --if-exists "$POSTGRES_DB" && createdb -U "$POSTGRES_USER" -O "$POSTGRES_USER" "$POSTGRES_DB"'
cat "$BACKUP_DIR/postgres.dump" \
  | docker compose -f "$COMPOSE_FILE" exec -T postgres sh -lc \
  'pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --no-owner --no-privileges --exit-on-error'

docker compose -f "$COMPOSE_FILE" up -d sub2api
curl --fail --silent --show-error http://127.0.0.1:${SERVER_PORT:-8080}/health
```

### 新服务器回滚

停止新服务器上的目标应用，恢复旧版本 Compose、旧 `.env` 和升级前 PostgreSQL dump；确认数据库恢复完成后再启动旧应用。旧服务器在回滚验证完成前保持离线但完整保留，避免两台服务器同时写入同一业务环境。

## 完成标准

迁移完成应同时满足：

- PostgreSQL dump 能被 `pg_restore -l` 读取，且恢复命令退出码为 0。
- `schema_migrations` 已由目标版本推进，没有 checksum mismatch。
- 关键表行数与迁移前基线符合预期，业务抽样数据可读取。
- `JWT_SECRET`、`TOTP_ENCRYPTION_KEY` 与源实例保持一致，登录和 2FA 正常。
- `/health`、管理员登录、API Key、流式/非流式请求、用量结算和后台任务均通过抽样。
- 旧镜像、旧配置、PostgreSQL dump 和回滚步骤在验证完成前继续保留。
