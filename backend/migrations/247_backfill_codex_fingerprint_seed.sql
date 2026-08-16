-- 为每个 OpenAI OAuth 凭据账号生成持久化随机指纹种子。
-- 旧实现只使用本地自增 account.ID，导致不同部署中的同编号账号产生相同身份。
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
  AND NULLIF(BTRIM(COALESCE(extra->>'codex_fingerprint_seed', '')), '') IS NULL;

-- Spark 影子与父账号共享凭据，也必须共享父账号的持久化种子。
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
  AND NULLIF(BTRIM(COALESCE(child.extra->>'codex_fingerprint_seed', '')), '') IS NULL
  AND NULLIF(BTRIM(COALESCE(parent.extra->>'codex_fingerprint_seed', '')), '') IS NOT NULL;

-- 防御性处理孤立影子记录：缺少有效父账号时仍分配独立随机种子。
UPDATE accounts
SET extra = jsonb_set(
    CASE WHEN jsonb_typeof(extra) = 'object' THEN extra ELSE '{}'::jsonb END,
    '{codex_fingerprint_seed}',
    to_jsonb(gen_random_uuid()::text),
    true
)
WHERE platform = 'openai'
  AND type = 'oauth'
  AND NULLIF(BTRIM(COALESCE(extra->>'codex_fingerprint_seed', '')), '') IS NULL;
