-- 默认被动观测最终上游 HTTP 响应；历史、非 HTTP 和未接入的路径保持 NULL。
-- 不加默认值、索引或回填，避免使用记录大表重写；原文凭据不进入数据库。
SET LOCAL lock_timeout = '5s';
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS upstream_status_code SMALLINT,
    ADD COLUMN IF NOT EXISTS codex_turn_state_bytes INTEGER;

COMMENT ON COLUMN usage_logs.upstream_status_code IS '最终上游 HTTP 状态；不是模型能力或会话等级';
COMMENT ON COLUMN usage_logs.codex_turn_state_bytes IS 'x-codex-turn-state 去除外侧空白后的字节数；0=未返回，NULL=未采集';
