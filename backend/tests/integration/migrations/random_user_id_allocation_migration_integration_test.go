//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// random_user_id_allocation_migration_integration_test.go verifies PostgreSQL
// triggers, tier expansion counters, deletion decrements, recalibration, and
// capacity alert events introduced by migration 336. All assertions run inside
// an isolated transaction rolled back by testTx.

// TestMigration336_RandomUserIDAllocationAndTierLifecycle checks tier metadata,
// dropped users.id default, random four-digit allocation, explicit ID retention,
// physical-delete decrement, tier-5 expansion, recalibration, and tier-8 alerts.
func TestMigration336_RandomUserIDAllocationAndTierLifecycle(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)

	// Confirm the five pool tiers are present with half-capacity expansion thresholds.
	rows, err := tx.QueryContext(ctx, `
		SELECT tier_digit, min_id, max_id, capacity, threshold
		FROM user_id_pool_tiers
		ORDER BY tier_digit ASC
	`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	type tierSpec struct {
		digit     int
		minID     int64
		maxID     int64
		capacity  int64
		threshold int64
	}
	var got []tierSpec
	for rows.Next() {
		var row tierSpec
		require.NoError(t, rows.Scan(&row.digit, &row.minID, &row.maxID, &row.capacity, &row.threshold))
		got = append(got, row)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []tierSpec{
		{digit: 4, minID: 1000, maxID: 9999, capacity: 9000, threshold: 4500},
		{digit: 5, minID: 10000, maxID: 99999, capacity: 90000, threshold: 45000},
		{digit: 6, minID: 100000, maxID: 999999, capacity: 900000, threshold: 450000},
		{digit: 7, minID: 1000000, maxID: 9999999, capacity: 9000000, threshold: 4500000},
		{digit: 8, minID: 10000000, maxID: 99999999, capacity: 90000000, threshold: 45000000},
	}, got)

	// Confirm users.id no longer carries a column default after dropping the sequence default.
	var colDefault *string
	err = tx.QueryRowContext(ctx, `
		SELECT column_default
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'users'
		  AND column_name = 'id'
	`).Scan(&colDefault)
	require.NoError(t, err)
	require.Nil(t, colDefault)

	// Reset counters to zero on the empty fixture transaction to isolate delta assertions.
	_, err = tx.ExecContext(ctx, `SELECT recalibrate_user_id_pool_tiers()`)
	require.NoError(t, err)

	// Insert two users without specifying id and verify both land in the four-digit pool.
	var randomID1, randomID2 int64
	require.NoError(t, tx.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ('rand1@example.test', 'hash')
		RETURNING id
	`).Scan(&randomID1))
	require.NoError(t, tx.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ('rand2@example.test', 'hash')
		RETURNING id
	`).Scan(&randomID2))
	require.GreaterOrEqual(t, randomID1, int64(1000))
	require.LessOrEqual(t, randomID1, int64(9999))
	require.GreaterOrEqual(t, randomID2, int64(1000))
	require.LessOrEqual(t, randomID2, int64(9999))
	require.NotEqual(t, randomID1, randomID2)

	var tier4Used int64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT used_count FROM user_id_pool_tiers WHERE tier_digit = 4`).Scan(&tier4Used))
	require.Equal(t, int64(2), tier4Used)

	// Preserve an explicit five-digit ID and verify its tier counter increments once.
	var explicitID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
		INSERT INTO users (id, email, password_hash)
		VALUES (54321, 'explicit5@example.test', 'hash')
		RETURNING id
	`).Scan(&explicitID))
	require.Equal(t, int64(54321), explicitID)

	var tier5Used int64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT used_count FROM user_id_pool_tiers WHERE tier_digit = 5`).Scan(&tier5Used))
	require.Equal(t, int64(1), tier5Used)

	// Physically deleting the explicit user must decrement the five-digit counter.
	_, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id = 54321`)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT used_count FROM user_id_pool_tiers WHERE tier_digit = 5`).Scan(&tier5Used))
	require.Equal(t, int64(0), tier5Used)

	// Crossing the tier-4 threshold (> 4500) must unlock five-digit allocation up to 99999.
	_, err = tx.ExecContext(ctx, `UPDATE user_id_pool_tiers SET used_count = 4501 WHERE tier_digit = 4`)
	require.NoError(t, err)
	sawFiveDigit := false
	for i := range 20 {
		var candidateID int64
		require.NoError(t, tx.QueryRowContext(ctx, `
			INSERT INTO users (email, password_hash)
			VALUES ($1, 'hash')
			RETURNING id
		`, "expand-"+intToDigits(i)+"@example.test").Scan(&candidateID))
		require.GreaterOrEqual(t, candidateID, int64(1000))
		require.LessOrEqual(t, candidateID, int64(99999))
		if candidateID >= 10000 {
			sawFiveDigit = true
		}
	}
	require.True(t, sawFiveDigit, "expected at least one 5-digit allocation after tier 4 exceeds 4500")

	// Crossing 50%, 75%, and 90% on tier 8 must write matching ops_alert_events records.
	cases := []struct {
		before   int64
		email    string
		explicit int64
		severity string
		pctLabel string
	}{
		{before: 44999999, email: "tier8-50@example.test", explicit: 10000001, severity: "warning", pctLabel: "50%"},
		{before: 67499999, email: "tier8-75@example.test", explicit: 10000002, severity: "warning", pctLabel: "75%"},
		{before: 80999999, email: "tier8-90@example.test", explicit: 10000003, severity: "critical", pctLabel: "90%"},
	}
	for _, tc := range cases {
		_, err = tx.ExecContext(ctx, `UPDATE user_id_pool_tiers SET used_count = $1 WHERE tier_digit = 8`, tc.before)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'hash')`, tc.explicit, tc.email)
		require.NoError(t, err)

		var gotSeverity, gotStatus string
		err = tx.QueryRowContext(ctx, `
			SELECT severity, status
			FROM ops_alert_events
			WHERE dimensions ->> 'tier' = '8'
			  AND dimensions ->> 'threshold' = $1
			ORDER BY id DESC
			LIMIT 1
		`, tc.pctLabel).Scan(&gotSeverity, &gotStatus)
		require.NoError(t, err)
		require.Equal(t, tc.severity, gotSeverity)
		require.Equal(t, "firing", gotStatus)
	}

	// Recalibration must realign tier counts strictly with existing rows in users.
	_, err = tx.ExecContext(ctx, `SELECT recalibrate_user_id_pool_tiers()`)
	require.NoError(t, err)

	var recal8 int64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT used_count FROM user_id_pool_tiers WHERE tier_digit = 8`).Scan(&recal8))
	require.Equal(t, int64(3), recal8)
}

// intToDigits renders a small non-negative integer for unique test emails.
func intToDigits(v int) string {
	return string([]byte{'a' + byte(v/10), '0' + byte(v%10)})
}
