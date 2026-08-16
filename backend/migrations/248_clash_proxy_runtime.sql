-- Clash/mihomo 节点、策略、运行时和账号绑定。
CREATE TABLE IF NOT EXISTS clash_proxy_nodes (
    id          BIGSERIAL PRIMARY KEY,
    name        VARCHAR(255) NOT NULL,
    node_type   VARCHAR(32) NOT NULL,
    source_type VARCHAR(32) NOT NULL DEFAULT 'manual',
    config_json JSONB NOT NULL DEFAULT '{}',
    secret_json JSONB NOT NULL DEFAULT '{}',
    status      VARCHAR(20) NOT NULL DEFAULT 'active',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_clash_proxy_nodes_status
    ON clash_proxy_nodes(status) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS clash_proxy_profiles (
    id               BIGSERIAL PRIMARY KEY,
    name             VARCHAR(255) NOT NULL,
    strategy         VARCHAR(32) NOT NULL DEFAULT 'select',
    test_url         TEXT NOT NULL DEFAULT 'https://www.gstatic.com/generate_204',
    interval_seconds INT NOT NULL DEFAULT 300,
    status           VARCHAR(20) NOT NULL DEFAULT 'active',
    config_json      JSONB NOT NULL DEFAULT '{}',
    managed_proxy_id BIGINT UNIQUE REFERENCES proxies(id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at       TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_clash_proxy_profiles_status
    ON clash_proxy_profiles(status) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS clash_proxy_profile_nodes (
    id         BIGSERIAL PRIMARY KEY,
    profile_id BIGINT NOT NULL REFERENCES clash_proxy_profiles(id) ON DELETE CASCADE,
    node_id    BIGINT NOT NULL REFERENCES clash_proxy_nodes(id) ON DELETE RESTRICT,
    sort_order INT NOT NULL DEFAULT 0,
    weight     INT NOT NULL DEFAULT 1,
    enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(profile_id, node_id)
);

CREATE INDEX IF NOT EXISTS idx_clash_proxy_profile_nodes_profile
    ON clash_proxy_profile_nodes(profile_id, enabled, sort_order);

CREATE TABLE IF NOT EXISTS clash_proxy_runtime_instances (
    id                    BIGSERIAL PRIMARY KEY,
    profile_id            BIGINT NOT NULL UNIQUE REFERENCES clash_proxy_profiles(id) ON DELETE CASCADE,
    runtime_type          VARCHAR(32) NOT NULL DEFAULT 'mihomo',
    pid                   INT,
    mixed_port            INT NOT NULL,
    controller_port       INT NOT NULL,
    controller_secret_ref TEXT NOT NULL,
    config_path           TEXT NOT NULL,
    work_dir              TEXT NOT NULL,
    status                VARCHAR(20) NOT NULL DEFAULT 'stopped',
    last_error            TEXT,
    started_at            TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_clash_proxy_runtime_status
    ON clash_proxy_runtime_instances(status);

CREATE TABLE IF NOT EXISTS clash_proxy_account_bindings (
    id                BIGSERIAL PRIMARY KEY,
    account_id        BIGINT NOT NULL UNIQUE REFERENCES accounts(id) ON DELETE CASCADE,
    profile_id        BIGINT NOT NULL REFERENCES clash_proxy_profiles(id) ON DELETE CASCADE,
    previous_proxy_id BIGINT REFERENCES proxies(id) ON DELETE SET NULL,
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_clash_proxy_account_bindings_profile
    ON clash_proxy_account_bindings(profile_id, enabled);
