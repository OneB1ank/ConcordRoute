package repository

import (
	"time"

	"github.com/redis/go-redis/v9"
)

// 账号行的 updated_at 由数据库触发器保证逐次递增；UTC 定长表示避免 Lua 浮点精度丢失。
// v2 命名空间隔离滚动升级期间仍使用普通 SET 的旧进程。
// @project-doc docs/architecture/account_scheduling_and_cache.md#scheduler_snapshot_consistency
const schedulerAccountRevisionPrefix = "sched:acc:revision:v2:"

func schedulerAccountRevisionKey(id string) string {
	return schedulerAccountRevisionPrefix + id
}

func schedulerAccountRevision(updatedAt time.Time) string {
	if updatedAt.IsZero() {
		// 保留未带版本的测试/旧适配器兼容；空版本不会覆盖已有真实数据库版本。
		return ""
	}
	return updatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

// 单账号的完整值、筛选元数据及版本原子发布，旧桶 writer 也必须经过同一边界。
var writeSchedulerAccountScript = redis.NewScript(`
local current = redis.call('GET', KEYS[3])
if current ~= false then
    if current == 'deleted' or ARGV[1] < current then
        return 0
    end
end
redis.call('SET', KEYS[1], ARGV[2])
redis.call('SET', KEYS[2], ARGV[3])
redis.call('SET', KEYS[3], ARGV[1])
return 1
`)

// 删除保留账号级墓碑；数据库账号 ID 不复用，延迟的旧快照不能复活已删除账号。
var deleteSchedulerAccountScript = redis.NewScript(`
redis.call('SET', KEYS[4], 'deleted')
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3])
return 1
`)

// 无效快照只可驱逐不比自己新的缓存，避免旧的编码失败路径绕过版本保护。
var evictSchedulerAccountScript = redis.NewScript(`
local current = redis.call('GET', KEYS[4])
if current ~= false and (current == 'deleted' or ARGV[1] < current) then
    return 0
end
redis.call('SET', KEYS[4], ARGV[1])
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3])
return 1
`)
