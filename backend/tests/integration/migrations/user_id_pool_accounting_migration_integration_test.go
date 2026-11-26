//go:build integration

package migrations_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	postgresinfra "github.com/TokenFlux/TokenRouter/internal/infra/postgres"
	"github.com/TokenFlux/TokenRouter/migrations"
)

// user_id_pool_accounting_migration_integration_test.go verifies the upgrade
// from migration 336 and the accounting contract after migration 337. One
// database starts on the old schema and commits a replay that inflates its
// counter. The runner must correct the count during the upgrade while leaving
// stored user IDs and the event history intact.
//
// The current-schema case compares accepted users, tier counts, and Ops events
// in a transaction that rolls back its data changes. A dedicated connection
// captures warnings from a conflict-only statement and a mixed batch, including
// the warning already delivered before a rollback to the savepoint.

// TestMigration337_RecountsPersistedUsersOnUpgrade commits drift with migration
// 336 installed and applies the remaining history. The repaired count must
// match existing users and preserve the previously recorded event.
func TestMigration337_RecountsPersistedUsersOnUpgrade(t *testing.T) {
	ctx := t.Context()
	db := isolatedMigrationDB(t, "337_")
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash) VALUES
			(42, '337-legacy@example.test', 'hash'),
			(4321, '337-four@example.test', 'hash'),
			(10000001, '337-eight@example.test', 'hash');
		UPDATE user_id_pool_tiers SET used_count = 44999999 WHERE tier_digit = 8;
	`)
	require.NoError(t, err)

	// Replay the known 336 failure on committed data. The filtered insert
	// still leaves a drifted count and an event visible to the next migration.
	result, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash)
		VALUES (10000001, '337-replay@example.test', 'hash')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err)
	inserted, err := result.RowsAffected()
	require.NoError(t, err)
	require.Zero(t, inserted)
	assertTierEightAccounting(t, db, 45000000, 1)

	require.NoError(t, postgresinfra.ApplyMigrations(ctx, db, migrations.FS))
	assertTierEightAccounting(t, db, 1, 1)

	var unchanged bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT (SELECT array_agg(id ORDER BY id) FROM users) = ARRAY[42, 4321, 10000001]::bigint[]
		   AND (SELECT array_agg(used_count ORDER BY tier_digit) FROM user_id_pool_tiers)
		       = ARRAY[1, 0, 0, 0, 1]::bigint[]
	`).Scan(&unchanged))
	require.True(t, unchanged, "recount must preserve users and correct all five tiers")
}

// TestMigration337_CountsAcceptedUsersAndRollsBackAlerts checks that implicit and
// explicit conflicts have zero effect. A successful insert must update the
// matching tier, and rolling back that row must also remove its Ops event.
func TestMigration337_CountsAcceptedUsersAndRollsBackAlerts(t *testing.T) {
	ctx := t.Context()
	connector, err := pq.NewConnector(integrationDSN)
	require.NoError(t, err)
	var warnings []string
	db := sql.OpenDB(pq.ConnectorWithNoticeHandler(connector, func(notice *pq.Error) {
		if notice.Severity == "WARNING" {
			warnings = append(warnings, notice.Message)
		}
	}))
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	// Roll back the fixture before closing its single warning-capturing
	// transaction connection, including when an assertion stops the test early.
	t.Cleanup(func() { _ = tx.Rollback() })

	var implicitID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ('337-implicit@example.test', 'hash')
		RETURNING id
	`).Scan(&implicitID))
	require.GreaterOrEqual(t, implicitID, int64(1000))
	require.LessOrEqual(t, implicitID, int64(9999))

	result, err := tx.ExecContext(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ('337-implicit@example.test', 'hash')
		ON CONFLICT DO NOTHING
	`)
	require.NoError(t, err)
	inserted, err := result.RowsAffected()
	require.NoError(t, err)
	require.Zero(t, inserted)
	var tierFourCount int64
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT used_count FROM user_id_pool_tiers WHERE tier_digit = 4
	`).Scan(&tierFourCount))
	require.EqualValues(t, 1, tierFourCount)

	// Position the counter one below the cutoff so a single incorrect
	// increment would leave a visible event without needing millions of rows.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash)
		VALUES (10000001, '337-present@example.test', 'hash');
		UPDATE user_id_pool_tiers SET used_count = 44999999 WHERE tier_digit = 8;
	`)
	require.NoError(t, err)

	result, err = tx.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash)
		VALUES (10000001, '337-replay@example.test', 'hash')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err)
	inserted, err = result.RowsAffected()
	require.NoError(t, err)
	require.Zero(t, inserted)
	assertTierEightAccounting(t, tx, 44999999, 0)
	require.Empty(t, warnings)

	// Keep the original user and threshold fixture outside the savepoint
	// so the rollback isolates the two newly accepted rows and their event.
	_, err = tx.ExecContext(ctx, "SAVEPOINT batch")
	require.NoError(t, err)
	result, err = tx.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash) VALUES
			(10000001, '337-replay@example.test', 'hash'),
			(10000002, '337-batch-first@example.test', 'hash'),
			(10000003, '337-batch-second@example.test', 'hash')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err)
	inserted, err = result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 2, inserted)
	assertTierEightAccounting(t, tx, 45000001, 1)
	require.Equal(t, []string{"8位数账号ID池使用已超过50%: 45000000/90000000"}, warnings)

	// Return to a database state where the accepted rows never existed.
	// Their counter and event must disappear in that same rollback.
	_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT batch")
	require.NoError(t, err)
	assertTierEightAccounting(t, tx, 44999999, 0)
	var rolledBackRows int64
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT count(*) FROM users WHERE id IN (10000002, 10000003)
	`).Scan(&rolledBackRows))
	require.Zero(t, rolledBackRows)
	require.Len(t, warnings, 1, "rollback cannot retract the delivered PostgreSQL warning")
}

// assertTierEightAccounting reads the pool and event count from the same SQL
// snapshot so a caller can inspect them before or after a rollback.
func assertTierEightAccounting(
	t *testing.T,
	db interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	},
	wantUsed, wantEvents int64,
) {
	t.Helper()
	var used, events int64
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT used_count, (SELECT count(*) FROM ops_alert_events WHERE dimensions ->> 'tier' = '8')
		FROM user_id_pool_tiers
		WHERE tier_digit = 8
	`).Scan(&used, &events))
	require.Equal(t, wantUsed, used, "tier-eight occupancy")
	require.Equal(t, wantEvents, events, "tier-eight events")
}
