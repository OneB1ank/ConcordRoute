-- 部分升级历史中195晚于218执行，将约束重新收窄为0..4，导致Live用量落库失败。
-- 使用新的前向迁移恢复0..5，不修改旧迁移校验和；NOT VALID避免扫描历史大表，
-- 同时立即校验所有新增/更新记录。只放宽枚举，不改写用量或计费数据。
SET LOCAL lock_timeout = '5s';

ALTER TABLE usage_logs
    DROP CONSTRAINT IF EXISTS usage_logs_request_type_check;

ALTER TABLE usage_logs
    ADD CONSTRAINT usage_logs_request_type_check
    CHECK (request_type IN (0, 1, 2, 3, 4, 5)) NOT VALID;
