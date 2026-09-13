-- 将渠道监控的工厂默认切换为 V2 被动聚合。
-- 迁移 262 写入的 v1 且从未被管理员改过的记录视为旧工厂默认并切换；
-- 已经显式选择 v1/v2 的管理员设置保持不变，非法值和缺失值归一到 v2。
INSERT INTO settings AS target (key, value)
VALUES ('channel_monitor_mode', 'v2')
ON CONFLICT (key) DO UPDATE
SET value = CASE
    WHEN LOWER(TRIM(target.value)) = 'v2' THEN target.value
    WHEN LOWER(TRIM(target.value)) = 'v1'
         AND EXISTS (
             SELECT 1
             FROM schema_migrations AS legacy
             WHERE legacy.filename = '262_channel_monitor_mode.sql'
               AND target.updated_at = legacy.applied_at
         ) THEN 'v2'
    WHEN LOWER(TRIM(target.value)) NOT IN ('v1', 'v2') THEN 'v2'
    ELSE target.value
END,
updated_at = CASE
    WHEN LOWER(TRIM(target.value)) = 'v2' THEN target.updated_at
    WHEN LOWER(TRIM(target.value)) = 'v1'
         AND EXISTS (
             SELECT 1
             FROM schema_migrations AS legacy
             WHERE legacy.filename = '262_channel_monitor_mode.sql'
               AND target.updated_at = legacy.applied_at
         ) THEN NOW()
    WHEN LOWER(TRIM(target.value)) NOT IN ('v1', 'v2') THEN NOW()
    ELSE target.updated_at
END;
