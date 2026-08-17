-- 修复缺失、格式错误或 nil UUID 的 Codex 指纹种子。
-- 接受 google/uuid.Parse 支持的 canonical、compact、URN 和花括号格式，
-- 仅为损坏记录生成新值，避免正常升级轮换已有账号身份。
UPDATE accounts
SET extra = jsonb_set(
    CASE WHEN jsonb_typeof(extra) = 'object' THEN extra ELSE '{}'::jsonb END,
    '{codex_fingerprint_seed}',
    to_jsonb(gen_random_uuid()::text),
    true
)
WHERE platform = 'openai'
  AND type = 'oauth'
  AND parent_account_id IS NULL
  AND NOT (
      (
          COALESCE(extra->>'codex_fingerprint_seed', '') ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
          OR COALESCE(extra->>'codex_fingerprint_seed', '') ~* '^[0-9a-f]{32}$'
          OR COALESCE(extra->>'codex_fingerprint_seed', '') ~* '^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
          OR COALESCE(extra->>'codex_fingerprint_seed', '') ~* '^\{[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\}$'
      )
      AND regexp_replace(
          LOWER(BTRIM(COALESCE(extra->>'codex_fingerprint_seed', ''))),
          '^(urn:uuid:)|[{}-]', '', 'g'
      ) <> '00000000000000000000000000000000'
  );

-- Spark 影子与父账号共享同一凭据，因此强制同步为父账号的有效种子。
UPDATE accounts AS child
SET extra = jsonb_set(
    CASE WHEN jsonb_typeof(child.extra) = 'object' THEN child.extra ELSE '{}'::jsonb END,
    '{codex_fingerprint_seed}',
    to_jsonb(parent.extra->>'codex_fingerprint_seed'),
    true
)
FROM accounts AS parent
WHERE child.platform = 'openai'
  AND child.type = 'oauth'
  AND child.parent_account_id = parent.id
  AND parent.platform = 'openai'
  AND parent.type = 'oauth'
  AND child.extra->>'codex_fingerprint_seed' IS DISTINCT FROM parent.extra->>'codex_fingerprint_seed';

-- 防御性处理没有有效父账号的剩余损坏记录。
UPDATE accounts
SET extra = jsonb_set(
    CASE WHEN jsonb_typeof(extra) = 'object' THEN extra ELSE '{}'::jsonb END,
    '{codex_fingerprint_seed}',
    to_jsonb(gen_random_uuid()::text),
    true
)
WHERE platform = 'openai'
  AND type = 'oauth'
  AND NOT (
      (
          COALESCE(extra->>'codex_fingerprint_seed', '') ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
          OR COALESCE(extra->>'codex_fingerprint_seed', '') ~* '^[0-9a-f]{32}$'
          OR COALESCE(extra->>'codex_fingerprint_seed', '') ~* '^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
          OR COALESCE(extra->>'codex_fingerprint_seed', '') ~* '^\{[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\}$'
      )
      AND regexp_replace(
          LOWER(BTRIM(COALESCE(extra->>'codex_fingerprint_seed', ''))),
          '^(urn:uuid:)|[{}-]', '', 'g'
      ) <> '00000000000000000000000000000000'
  );
