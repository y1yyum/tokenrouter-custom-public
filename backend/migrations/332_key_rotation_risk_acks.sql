-- 仅成功替换非删除记录的凭据时记下轮换时间。历史记录以创建时间为准。
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS key_rotated_at TIMESTAMPTZ;

CREATE OR REPLACE FUNCTION record_api_key_rotation_time() RETURNS TRIGGER AS $$
BEGIN
    NEW.key_rotated_at := clock_timestamp();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS api_key_rotation_time ON api_keys;
CREATE TRIGGER api_key_rotation_time
    BEFORE UPDATE OF key ON api_keys
    FOR EACH ROW
    WHEN (OLD.deleted_at IS NULL AND NEW.deleted_at IS NULL AND OLD.key IS DISTINCT FROM NEW.key)
    EXECUTE FUNCTION record_api_key_rotation_time();

-- 风险确认按追加方式留存，截止时间变化后不再采用先前的确认。
CREATE TABLE IF NOT EXISTS key_rotation_risk_acks (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    acknowledged_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    client_ip TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    cutoff_snapshot TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_key_rotation_risk_acks_user
    ON key_rotation_risk_acks (user_id, cutoff_snapshot, expires_at DESC);
