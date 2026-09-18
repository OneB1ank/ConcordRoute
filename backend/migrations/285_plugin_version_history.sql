-- Keep verified package/config snapshots for an explicit 24-hour rollback window.
-- Existing installation and binding columns are unchanged for older managers.
CREATE TABLE IF NOT EXISTS sub2api_plugin_versions (
    id BIGSERIAL PRIMARY KEY,
    plugin_id BIGINT NOT NULL REFERENCES sub2api_plugin_installations(id) ON DELETE CASCADE,
    plugin_key VARCHAR(160) NOT NULL,
    name VARCHAR(160) NOT NULL,
    version VARCHAR(64) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    author TEXT NOT NULL DEFAULT '',
    manifest JSONB NOT NULL,
    artifact_data BYTEA NOT NULL,
    binary_sha256 VARCHAR(64) NOT NULL,
    signature_status VARCHAR(32) NOT NULL,
    config_encrypted TEXT NOT NULL DEFAULT '',
    saved_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '24 hours')
);
CREATE INDEX IF NOT EXISTS idx_sub2api_plugin_versions_history
    ON sub2api_plugin_versions(plugin_id, saved_at DESC, id DESC);
