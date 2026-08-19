-- 持久化 Clash 策略的期望运行状态，使应用或服务器重启后可自动恢复。
ALTER TABLE clash_proxy_profiles
    ADD COLUMN IF NOT EXISTS auto_start BOOLEAN NOT NULL DEFAULT FALSE;

-- 升级兼容：旧版本没有期望状态字段。运行中的策略，或仍保留启用账号绑定和
-- 本地托管代理的策略，视为用户希望持续运行，避免升级后仍需手动启动。
UPDATE clash_proxy_profiles AS profile
SET auto_start = TRUE,
    updated_at = NOW()
WHERE profile.deleted_at IS NULL
  AND profile.status = 'active'
  AND profile.auto_start = FALSE
  AND (
      EXISTS (
          SELECT 1
          FROM clash_proxy_runtime_instances AS runtime
          WHERE runtime.profile_id = profile.id
            AND runtime.status IN ('starting', 'running')
      )
      OR (
          profile.managed_proxy_id IS NOT NULL
          AND EXISTS (
              SELECT 1
              FROM clash_proxy_account_bindings AS binding
              WHERE binding.profile_id = profile.id
                AND binding.enabled = TRUE
          )
      )
  );

CREATE INDEX IF NOT EXISTS idx_clash_proxy_profiles_autostart
    ON clash_proxy_profiles(auto_start, id)
    WHERE deleted_at IS NULL AND status = 'active' AND auto_start = TRUE;
