#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"

# 统一输出失败原因，方便从 CI 日志定位配置漂移。
fail() {
  printf 'docker compose variants test failed: %s\n' "$1" >&2
  exit 1
}

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/concordroute-compose-variants.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

# 只提取应用服务的 KEY=value 环境变量，忽略注释和其他服务。
extract_application_environment() {
  file=$1
  awk '
    $0 == "  sub2api:" {
      in_application = 1
      next
    }
    in_application && $0 ~ /^  [A-Za-z0-9_-]+:$/ {
      exit
    }
    in_application && $0 == "    environment:" {
      in_environment = 1
      next
    }
    in_environment && $0 ~ /^    [A-Za-z0-9_-]+:/ {
      exit
    }
    in_environment && $0 ~ /^      - [A-Z][A-Z0-9_]*=/ {
      sub(/^      - /, "")
      print
    }
  ' "$file" | LC_ALL=C sort
}

# 保存环境变量和值，并单独保存键集合用于拓扑变体比较。
save_environment() {
  name=$1
  file=$2
  extract_application_environment "$file" > "$tmp_dir/$name.env"
  cut -d= -f1 "$tmp_dir/$name.env" > "$tmp_dir/$name.keys"
}

save_environment standard deploy/docker-compose.yml
save_environment local deploy/docker-compose.local.yml
save_environment standalone deploy/docker-compose.standalone.yml
save_environment dev deploy/docker-compose.dev.yml

# 两个生产内置服务变体除持久化方式外，应用环境必须完全一致。
cmp -s "$tmp_dir/standard.env" "$tmp_dir/local.env" || \
  fail 'standard and local application environments differ'

# standalone 只允许连接地址表达式不同，不能遗漏公共配置键。
cmp -s "$tmp_dir/standard.keys" "$tmp_dir/standalone.keys" || \
  fail 'standalone application environment keys differ from standard'

# 开发版可增加开发专用变量，但必须包含全部公共配置键。
while IFS= read -r key
do
  grep -Fqx "$key" "$tmp_dir/dev.keys" || \
    fail "dev application environment is missing $key"
done < "$tmp_dir/standard.keys"

# 除部署拓扑和调试模式外，公共环境变量的表达式也必须一致。
for variant in standalone dev
do
  while IFS= read -r entry
  do
    key=${entry%%=*}
    case "$key" in
      SERVER_MODE|DATABASE_HOST|DATABASE_PORT|DATABASE_USER|DATABASE_PASSWORD|DATABASE_DBNAME|DATABASE_SSLMODE|REDIS_HOST|REDIS_PORT)
        continue
        ;;
    esac
    grep -Fqx "$entry" "$tmp_dir/$variant.env" || \
      fail "$variant has a different value expression for $key"
  done < "$tmp_dir/standard.env"
done

# 内置三服务变体都应为应用、PostgreSQL 和 Redis 设置文件描述符上限。
for compose_file in \
  deploy/docker-compose.yml \
  deploy/docker-compose.local.yml \
  deploy/docker-compose.dev.yml
do
  soft_count=$(grep -Fxc '        soft: 100000' "$compose_file" || true)
  hard_count=$(grep -Fxc '        hard: 100000' "$compose_file" || true)
  [ "$soft_count" -eq 3 ] || fail "$compose_file must contain three soft nofile limits"
  [ "$hard_count" -eq 3 ] || fail "$compose_file must contain three hard nofile limits"
done

# standalone 只有应用容器，因此只需要一组文件描述符上限。
soft_count=$(grep -Fxc '        soft: 100000' deploy/docker-compose.standalone.yml || true)
hard_count=$(grep -Fxc '        hard: 100000' deploy/docker-compose.standalone.yml || true)
[ "$soft_count" -eq 1 ] || fail 'standalone must contain one soft nofile limit'
[ "$hard_count" -eq 1 ] || fail 'standalone must contain one hard nofile limit'

printf 'docker compose variants test passed\n'
