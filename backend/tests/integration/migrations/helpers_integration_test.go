//go:build integration

package migrations_test

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"

	postgresinfra "github.com/TokenFlux/TokenRouter/internal/infra/postgres"
	"github.com/TokenFlux/TokenRouter/migrations"
)

// helpers_integration_test.go prepares a shared migrated database or a
// test-owned database for an earlier schema. Historical fixtures
// select embedded files by lexical filename comparison, so their callers supply
// a complete cutoff or a prefix matching the migration order they intend to
// reproduce. The selected history is committed by ApplyMigrations before
// fixture writes begin in a separate transaction.
//
// A test-owned database lets a fixture commit writes before an upgrade runs
// through the production runner. Its pool closes before the shared connection
// force-drops the database, keeping committed fixtures away from other cases.
// Ordinary testTx fixtures instead roll back their changes on the shared database
// when the test finishes.

// integrationDSN and integrationDB are initialized together by
// runPostgresTests. Historical fixtures replace the DSN's database path and use
// the shared pool to create and delete their databases until the package run
// finishes.
var (
	integrationDSN string  // shared PostgreSQL connection URL
	integrationDB  *sql.DB // fully migrated shared pool
)

// testTx opens a transaction on the fully migrated shared database and
// registers a rollback attempt at test cleanup. Callers use the returned
// transaction for fixture SQL and keep parallel access to shared tables under
// their control.
func testTx(t *testing.T) *sql.Tx {
	t.Helper()
	tx, err := integrationDB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	// Cleanup attempts rollback after assertion failures as well as
	// success; its error is discarded because callers may already have
	// ended the fixture transaction.
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// historicalTx creates a test-owned database, applies files sorted before the
// supplied filename or prefix, and returns a transaction for the fixture. The
// cutoff is a lexical filename boundary, including any prefix the caller
// deliberately uses; it does not parse migration numbers.
//
// The migration runner commits the selected history before the returned
// transaction begins. Cleanup rolls back that transaction, closes its pool, and
// force-drops the database, including when subsequent setup or assertions stop
// the test. Rollback errors are discarded, while pool-close and database
// deletion errors fail the test.
func historicalTx(t *testing.T, before string) *sql.Tx {
	t.Helper()

	db := isolatedMigrationDB(t, before)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	// Unwind this transaction before the fixture closes its pool and drops the
	// database, including when an assertion stops the test early.
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// isolatedMigrationDB creates a database for one test and applies embedded files
// before the lexical cutoff. Callers can commit fixture writes and then apply
// the pending migrations to that same database.
//
// The runner commits each selected migration before returning the pool. Cleanup
// closes the pool before the shared connection force-drops the owned database,
// and both teardown steps are registered before fixture setup can fail.
func isolatedMigrationDB(t *testing.T, before string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("migration_history_%d", time.Now().UnixNano())
	_, err := integrationDB.ExecContext(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	// Register deletion immediately after creation so later setup failures
	// still remove the database; later cleanup registrations run first to
	// release its local resources.
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		require.NoError(t, err)
	})
	dsn, err := url.Parse(integrationDSN)
	require.NoError(t, err)
	dsn.Path = "/" + name
	db, err := sql.Open("postgres", dsn.String())
	require.NoError(t, err)
	// Closing this pool precedes force-dropping the database during
	// cleanup. The FORCE drop can terminate lingering server sessions after
	// the local resources unwind.
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	history := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.Name() >= before {
			continue
		}
		data, err := migrations.FS.ReadFile(entry.Name())
		require.NoError(t, err)
		// Retain each original filename and SQL bytes so the runner's
		// ordering and checksum checks describe the selected history,
		// including nontransactional migration suffixes.
		history[entry.Name()] = &fstest.MapFile{Data: data}
	}
	// Apply the selected history through the production runner so the test can
	// make committed writes before applying any remaining migrations.
	require.NoError(t, postgresinfra.ApplyMigrations(ctx, db, history))
	return db
}
