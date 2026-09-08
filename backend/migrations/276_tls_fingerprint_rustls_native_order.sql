-- 默认关闭原生排序，保留所有已存在模板的固定扩展顺序。
ALTER TABLE tls_fingerprint_profiles
    ADD COLUMN IF NOT EXISTS rustls_native_order BOOLEAN NOT NULL DEFAULT FALSE;
