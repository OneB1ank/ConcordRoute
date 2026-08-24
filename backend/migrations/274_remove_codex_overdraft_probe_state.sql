-- 清理已停用的 Codex 主动透支探针状态；保留账号级策略开关和真实上游限额字段。
WITH cleaned AS (
    UPDATE accounts
    SET extra = COALESCE(extra, '{}'::jsonb)
            - 'codex_quota_overdraft_probe'
            - 'codex_5h_overdraft_started_at'
            - 'codex_5h_overdraft_reset_at'
            - 'codex_7d_overdraft_started_at'
            - 'codex_7d_overdraft_reset_at',
        temp_unschedulable_until = CASE
            WHEN COALESCE(temp_unschedulable_reason, '') ~ '"source"[[:space:]]*:[[:space:]]*"codex_quota_overdraft"'
                THEN NULL
            ELSE temp_unschedulable_until
        END,
        temp_unschedulable_reason = CASE
            WHEN COALESCE(temp_unschedulable_reason, '') ~ '"source"[[:space:]]*:[[:space:]]*"codex_quota_overdraft"'
                THEN NULL
            ELSE temp_unschedulable_reason
        END,
        updated_at = NOW()
    WHERE deleted_at IS NULL
      AND (
          COALESCE(extra, '{}'::jsonb) ?| ARRAY[
              'codex_quota_overdraft_probe',
              'codex_5h_overdraft_started_at',
              'codex_5h_overdraft_reset_at',
              'codex_7d_overdraft_started_at',
              'codex_7d_overdraft_reset_at'
          ]
          OR COALESCE(temp_unschedulable_reason, '') ~ '"source"[[:space:]]*:[[:space:]]*"codex_quota_overdraft"'
      )
    RETURNING id
)
INSERT INTO scheduler_outbox (event_type, account_id)
SELECT 'account_changed', id
FROM cleaned;
