-- Pins created only by explicit administrator approval of a verified package.
-- Pin and installation/version writes share one transaction.
CREATE TABLE IF NOT EXISTS sub2api_plugin_publishers (
    key_id VARCHAR(128) PRIMARY KEY,
    public_key VARCHAR(44) NOT NULL,
    fingerprint VARCHAR(71) NOT NULL,
    trusted_by BIGINT,
    trusted_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
