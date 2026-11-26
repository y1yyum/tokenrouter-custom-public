-- 337_user_id_pool_count_inserted_rows.sql ties pool counts and capacity events
-- to rows PostgreSQL actually inserts. Migration 336 ran bookkeeping before
-- PostgreSQL resolved conflicts, so an ignored retry could consume capacity
-- and fire an alert without creating a user.
--
-- The allocator still fills missing IDs before insertion. The accounting
-- trigger runs after each accepted row, so it shares the user transaction and
-- rolls back with it. The existing recalibration function repairs old counts
-- while the migration holds a lock against concurrent user writes.

-- Hold off user writes during trigger replacement and recount. Readers can
-- still access users while the migration repairs the derived counters.
LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE;

-- allocate_random_user_id fills a missing ID using migration 336's tier
-- boundaries and retry policy. Only missing IDs take the existing advisory
-- lock, which serializes random draws across transactions. Explicit IDs pass
-- through unchanged, with duplicate handling left to the INSERT statement.
CREATE OR REPLACE FUNCTION allocate_random_user_id()
RETURNS TRIGGER AS $$
DECLARE
    v_t4_count bigint;
    v_t5_count bigint;
    v_t6_count bigint;
    v_t7_count bigint;
    v_max_id bigint := 9999;
    v_candidate bigint;
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

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- count_inserted_user_id updates the pool for each accepted tracked ID and
-- records the tier-eight event at a threshold crossing. The row update returns
-- the post-increment count while PostgreSQL holds the tier row lock, so two
-- committed inserts cannot both observe the same crossing.
--
-- The warning is delivered when the threshold is reached, including in
-- schemas without the Ops table. A later rollback reverses the count and any
-- event stored in that table, while the already delivered warning remains.
CREATE OR REPLACE FUNCTION count_inserted_user_id()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    digit INT := length(NEW.id::text);
    new_count BIGINT;
    threshold_percent INT;
    is_critical BOOLEAN;
BEGIN
    UPDATE user_id_pool_tiers
    SET used_count = used_count + 1
    WHERE tier_digit = digit
    RETURNING used_count INTO new_count;

    IF digit <> 8 THEN
        RETURN NULL;
    END IF;

    -- Each accepted row advances the count by one, so equality detects a
    -- crossing even when one statement inserts several rows.
    threshold_percent := CASE new_count
        WHEN 45000000 THEN 50
        WHEN 67500000 THEN 75
        WHEN 81000000 THEN 90
    END;
    IF threshold_percent IS NULL THEN
        RETURN NULL;
    END IF;

    -- Warn on each crossing even when the schema has no Ops event table
    -- to record this insert's transactional event.
    RAISE WARNING '8位数账号ID池使用已超过%: %/90000000', threshold_percent || '%', new_count;
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = current_schema() AND table_name = 'ops_alert_events'
    ) THEN
        RETURN NULL;
    END IF;

    is_critical := threshold_percent = 90;
    INSERT INTO ops_alert_events (severity, status, title, description, metric_value, threshold_value, dimensions)
    VALUES (
        CASE WHEN is_critical THEN 'critical' ELSE 'warning' END,
        'firing',
        format('8位数账号ID池已使用超过%s%%', threshold_percent),
        format(
            '8位数账号ID池容量%s：已使用 %s / 90,000,000 (%s%%)。%s',
            CASE WHEN is_critical THEN '告警' ELSE '提醒' END,
            new_count,
            threshold_percent,
            CASE WHEN is_critical THEN '请尽快规划扩容或后续ID策略。' ELSE '' END
        ),
        threshold_percent,
        threshold_percent,
        jsonb_build_object('tier', 8, 'threshold', threshold_percent || '%')
    );
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS trigger_count_inserted_user_id ON users;
CREATE TRIGGER trigger_count_inserted_user_id
    AFTER INSERT ON users
    FOR EACH ROW
    WHEN (NEW.id BETWEEN 1000 AND 99999999)
    EXECUTE FUNCTION count_inserted_user_id();

-- Reuse the original rebuild so historical overcounts are repaired from users
-- under the same write lock that protects the trigger change.
SELECT recalibrate_user_id_pool_tiers();
