-- 用现有 updated_at 作为账号行版本，不新增账号字段或改动身份绑定。
-- NOW() 在同一事务中固定，事务开始顺序也不等于更新顺序；
-- 在行锁内严格推进至少一微秒，避免同时间戳及较早事务晚写造成缓存版本歧义。
CREATE OR REPLACE FUNCTION concordroute_account_updated_at_monotonic()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at := GREATEST(
        OLD.updated_at + INTERVAL '1 microsecond',
        NEW.updated_at,
        clock_timestamp()
    );
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS accounts_updated_at_monotonic ON accounts;
CREATE TRIGGER accounts_updated_at_monotonic
BEFORE UPDATE ON accounts
FOR EACH ROW EXECUTE FUNCTION concordroute_account_updated_at_monotonic();
