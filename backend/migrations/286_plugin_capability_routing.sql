-- 插件路由仅增加默认字段，保持旧安装记录兼容。
ALTER TABLE sub2api_plugin_bindings
    ADD COLUMN IF NOT EXISTS priority INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS account_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS user_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS group_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS max_concurrency INTEGER NOT NULL DEFAULT 32,
    ADD COLUMN IF NOT EXISTS timeout_ms INTEGER NOT NULL DEFAULT 0;

-- 在同一迁移事务中重建同名约束，使重复执行也能保持一致。
ALTER TABLE sub2api_plugin_bindings
    DROP CONSTRAINT IF EXISTS sub2api_plugin_bindings_priority_check,
    DROP CONSTRAINT IF EXISTS sub2api_plugin_bindings_concurrency_check,
    DROP CONSTRAINT IF EXISTS sub2api_plugin_bindings_timeout_check,
    DROP CONSTRAINT IF EXISTS sub2api_plugin_bindings_ids_check;

ALTER TABLE sub2api_plugin_bindings
    ADD CONSTRAINT sub2api_plugin_bindings_priority_check CHECK (priority BETWEEN -1000 AND 1000),
    ADD CONSTRAINT sub2api_plugin_bindings_concurrency_check CHECK (max_concurrency BETWEEN 1 AND 256),
    ADD CONSTRAINT sub2api_plugin_bindings_timeout_check CHECK (timeout_ms BETWEEN 0 AND 5000),
    ADD CONSTRAINT sub2api_plugin_bindings_ids_check CHECK
        (jsonb_typeof(account_ids) = 'array' AND jsonb_typeof(user_ids) = 'array' AND jsonb_typeof(group_ids) = 'array');

-- v1 每种平台/账号类型仍只允许一个传输插件；v2 按细粒度范围和优先级选择。
DROP INDEX IF EXISTS idx_sub2api_plugin_bindings_one_enabled_scope;
CREATE UNIQUE INDEX idx_sub2api_plugin_bindings_one_enabled_scope
    ON sub2api_plugin_bindings(capability, platform, account_type)
    WHERE enabled = TRUE AND capability = 'openai.oauth.outbound_transport.v1';
