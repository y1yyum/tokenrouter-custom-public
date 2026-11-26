-- Allocate randomized 4-to-8-digit user IDs on registration with tiered
-- capacity expansion and threshold alerts when tier 8 reaches 50%/75%/90%.

-- 1. Create the tiered ID pool metadata and usage tracking table.
CREATE TABLE IF NOT EXISTS user_id_pool_tiers (
    tier_digit      INT NOT NULL PRIMARY KEY, -- Supported digit lengths: 4, 5, 6, 7, 8
    min_id          BIGINT NOT NULL,
    max_id          BIGINT NOT NULL,
    capacity        BIGINT NOT NULL,
    threshold       BIGINT NOT NULL,          -- Expansion threshold at half capacity
    used_count      BIGINT NOT NULL DEFAULT 0
);

-- 2. Seed each digit tier and backfill current counts from existing users.
INSERT INTO user_id_pool_tiers (tier_digit, min_id, max_id, capacity, threshold, used_count)
VALUES
    (4, 1000, 9999, 9000, 4500, (SELECT COUNT(*) FROM users WHERE id >= 1000 AND id <= 9999)),
    (5, 10000, 99999, 90000, 45000, (SELECT COUNT(*) FROM users WHERE id >= 10000 AND id <= 99999)),
    (6, 100000, 999999, 900000, 450000, (SELECT COUNT(*) FROM users WHERE id >= 100000 AND id <= 999999)),
    (7, 1000000, 9999999, 9000000, 4500000, (SELECT COUNT(*) FROM users WHERE id >= 1000000 AND id <= 9999999)),
    (8, 10000000, 99999999, 90000000, 45000000, (SELECT COUNT(*) FROM users WHERE id >= 10000000 AND id <= 99999999))
ON CONFLICT (tier_digit) DO UPDATE SET
    used_count = EXCLUDED.used_count;

-- 3. Drop the sequence default on users.id so omitted IDs enter trigger allocation.
ALTER TABLE users ALTER COLUMN id DROP DEFAULT;

-- 4. Allocate a random unused ID when NEW.id IS NULL and maintain tier counters/alerts.
CREATE OR REPLACE FUNCTION allocate_random_user_id()
RETURNS TRIGGER AS $$
DECLARE
    v_t4_count bigint;
    v_t5_count bigint;
    v_t6_count bigint;
    v_t7_count bigint;
    v_max_id bigint := 9999;
    v_candidate bigint;
    v_candidate_tier int;
    v_new_count bigint;
    v_attempts int := 0;
BEGIN
    IF NEW.id IS NULL THEN
        -- Serialize concurrent allocations within the transaction so two inserts
        -- cannot draw the same random ID before visibility.
        PERFORM pg_advisory_xact_lock(hashtext('user_id_allocation_lock'));

        -- Expand the upper bound to the next digit tier once earlier tiers pass 50%.
        SELECT used_count INTO v_t4_count FROM user_id_pool_tiers WHERE tier_digit = 4;
        IF v_t4_count > 4500 THEN
            SELECT used_count INTO v_t5_count FROM user_id_pool_tiers WHERE tier_digit = 5;
            IF v_t5_count > 45000 THEN
                SELECT used_count INTO v_t6_count FROM user_id_pool_tiers WHERE tier_digit = 6;
                IF v_t6_count > 450000 THEN
                    SELECT used_count INTO v_t7_count FROM user_id_pool_tiers WHERE tier_digit = 7;
                    IF v_t7_count > 4500000 THEN
                        v_max_id := 99999999;
                    ELSE
                        v_max_id := 9999999;
                    END IF;
                ELSE
                    v_max_id := 999999;
                END IF;
            ELSE
                v_max_id := 99999;
            END IF;
        ELSE
            v_max_id := 9999;
        END IF;

        -- Retry random draws within [1000, v_max_id] until an unoccupied ID is selected.
        LOOP
            v_attempts := v_attempts + 1;
            IF v_attempts > 100 THEN
                RAISE EXCEPTION 'failed to allocate unique random user ID after 100 attempts';
            END IF;

            v_candidate := floor(random() * (v_max_id - 1000 + 1) + 1000)::bigint;

            IF NOT EXISTS (SELECT 1 FROM users WHERE id = v_candidate) THEN
                NEW.id := v_candidate;
                EXIT;
            END IF;
        END LOOP;
    END IF;

    -- Keep explicit IDs (migrations or test fixtures) and track 4-to-8-digit usage.
    IF NEW.id >= 1000 AND NEW.id <= 99999999 THEN
        v_candidate_tier := length(NEW.id::text);
        UPDATE user_id_pool_tiers
        SET used_count = used_count + 1
        WHERE tier_digit = v_candidate_tier
        RETURNING used_count INTO v_new_count;

        -- Emit tier-8 capacity warnings and ops alert events when thresholds are crossed.
        IF v_candidate_tier = 8 THEN
            IF v_new_count = 45000000 OR (v_new_count > 45000000 AND v_new_count - 1 < 45000000) THEN
                RAISE WARNING '8位数账号ID池使用已超过50%%: %/90000000', v_new_count;
                IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'ops_alert_events') THEN
                    INSERT INTO ops_alert_events (severity, status, title, description, metric_value, threshold_value, dimensions)
                    VALUES ('warning', 'firing', '8位数账号ID池已使用超过50%', '8位数账号ID池容量提醒：已使用 ' || v_new_count || ' / 90,000,000 (50%)。', v_new_count::double precision / 90000000.0 * 100.0, 50.0, '{"tier": 8, "threshold": "50%"}'::jsonb);
                END IF;
            ELSIF v_new_count = 67500000 OR (v_new_count > 67500000 AND v_new_count - 1 < 67500000) THEN
                RAISE WARNING '8位数账号ID池使用已超过75%%: %/90000000', v_new_count;
                IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'ops_alert_events') THEN
                    INSERT INTO ops_alert_events (severity, status, title, description, metric_value, threshold_value, dimensions)
                    VALUES ('warning', 'firing', '8位数账号ID池已使用超过75%', '8位数账号ID池容量提醒：已使用 ' || v_new_count || ' / 90,000,000 (75%)。', v_new_count::double precision / 90000000.0 * 100.0, 75.0, '{"tier": 8, "threshold": "75%"}'::jsonb);
                END IF;
            ELSIF v_new_count = 81000000 OR (v_new_count > 81000000 AND v_new_count - 1 < 81000000) THEN
                RAISE WARNING '8位数账号ID池使用已超过90%%: %/90000000', v_new_count;
                IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'ops_alert_events') THEN
                    INSERT INTO ops_alert_events (severity, status, title, description, metric_value, threshold_value, dimensions)
                    VALUES ('critical', 'firing', '8位数账号ID池已使用超过90%', '8位数账号ID池容量告警：已使用 ' || v_new_count || ' / 90,000,000 (90%)，请尽快规划扩容或后续ID策略。', v_new_count::double precision / 90000000.0 * 100.0, 90.0, '{"tier": 8, "threshold": "90%"}'::jsonb);
                END IF;
            END IF;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_allocate_random_user_id ON users;
CREATE TRIGGER trigger_allocate_random_user_id
    BEFORE INSERT ON users
    FOR EACH ROW
    EXECUTE FUNCTION allocate_random_user_id();

-- 5. Decrement the tier counter when a user row is physically deleted.
CREATE OR REPLACE FUNCTION handle_user_id_pool_on_delete()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.id >= 1000 AND OLD.id <= 99999999 THEN
        UPDATE user_id_pool_tiers
        SET used_count = GREATEST(0, used_count - 1)
        WHERE tier_digit = length(OLD.id::text);
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_handle_user_id_pool_on_delete ON users;
CREATE TRIGGER trigger_handle_user_id_pool_on_delete
    AFTER DELETE ON users
    FOR EACH ROW
    EXECUTE FUNCTION handle_user_id_pool_on_delete();

-- 6. Recalculate tier counters directly from existing users rows.
CREATE OR REPLACE FUNCTION recalibrate_user_id_pool_tiers()
RETURNS void AS $$
BEGIN
    UPDATE user_id_pool_tiers t
    SET used_count = (
        SELECT COUNT(*)
        FROM users u
        WHERE u.id >= t.min_id AND u.id <= t.max_id
    );
END;
$$ LANGUAGE plpgsql;
