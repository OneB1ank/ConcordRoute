-- 持久化请求侧 Turn-State 动作，供管理员观测；State 原文不进入数据库。
SET LOCAL lock_timeout = '5s';

ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS codex_turn_state_request_mode TEXT;

COMMENT ON COLUMN usage_logs.codex_turn_state_request_mode IS
    'Request-side Codex Turn-State mode: injected, acquire, disabled, or not_recorded';
