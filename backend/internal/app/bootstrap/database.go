package bootstrap

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/mode"
	postgresinfra "github.com/TokenFlux/TokenRouter/internal/infra/postgres"
	"github.com/TokenFlux/TokenRouter/migrations"
)

// database.go initializes PostgreSQL and resolves the schema and signing keys
// before returning shared database clients.
// Database initialization fixes the process timezone before constructing the
// shared PostgreSQL pool. Migrations run before persistent system secrets are
// loaded, and complete configuration validation runs after those secrets have
// populated cfg. A ten-minute context bounds migration retries and secret
// initialization while retaining the caller's cancellation signal.
//
// Crypto review instances compare the existing migration ledger with the
// bundled files and use separate signing keys. When interactive writes are
// disabled, the shared pool opens read-only transactions so that
// incidental writes from authentication cannot update shared PostgreSQL rows.
//
// Ent and direct SQL consumers use the same pool, so successful construction
// transfers closing responsibility to the returned Ent client. Failed
// initialization closes the acquired pool and joins any close error with the
// original failure. This prevents a migration or secret failure from leaving
// an application connection pool open during aborted startup.

// InitEnt initializes the timezone and database, then returns an Ent client and
// its shared SQL pool. Ordinary instances apply migrations before loading
// persisted signing keys. Review instances check the existing ledger and
// initialize local keys even when their interactive write switch is enabled.
//
// Migration retries and secret initialization share a ten-minute budget derived
// from ctx. The caller closes the Ent client on success, which also closes the
// pool used by direct SQL consumers. Failure closes an acquired pool before
// returning and retains cleanup errors in the returned error chain.
// @project-doc docs/operations/deployment_and_migrations.md#crypto_review_instance
func InitEnt(ctx context.Context, cfg *config.Config) (_ *ent.Client, _ *sql.DB, resultErr error) {
	if err := InitTimezone(cfg.Timezone); err != nil {
		return nil, nil, err
	}

	// Pass the configured timezone to PostgreSQL through the DSN.
	dsn := cfg.Database.DSNWithTimezone(cfg.Timezone)
	openDatabase := postgresinfra.Open
	if mode.ReadOnly() {
		openDatabase = openReviewDatabase
	}

	db, err := openDatabase(dsn, cfg.Server.EnableServerTiming)
	if err != nil {
		return nil, nil, err
	}
	// Construction has not transferred ownership while a named error is present.
	// Close the pool and join cleanup failures so the initialization cause remains
	// observable through the returned error chain.
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, db.Close())
		}
	}()
	drv := entsql.OpenDB(dialect.Postgres, db)
	applyDBPoolSettings(drv.DB(), cfg)

	// Bound retries and secret initialization together so repeated migration
	// failures cannot consume a fresh full deadline before secret loading starts.
	migrationCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	initializeSchema := ApplyMigrations
	initializeSecrets := ensureBootstrapSecrets
	if mode.TestInstance() {
		// Select validation before touching the ledger. The migration runner
		// creates metadata even when all migration files are already recorded.
		initializeSchema = validateReviewMigrations
		initializeSecrets = prepareReviewSecrets
	}
	if err := postgresinfra.InitializeWithRetry(migrationCtx, func(ctx context.Context) error {
		return initializeSchema(ctx, drv.DB())
	}); err != nil {
		return nil, nil, err
	}

	client := ent.NewClient(ent.Driver(drv))

	// The normal bootstrap uses tables installed by migrations. Both modes
	// resolve signing keys before the final validation requires a JWT secret.
	if err := initializeSecrets(migrationCtx, client, cfg); err != nil {
		return nil, nil, err
	}

	// Bootstrap loading permits a missing persisted secret. Validate cfg after
	// secret resolution so the complete application receives a usable secret.
	if err := cfg.Validate(); err != nil {
		return nil, nil, fmt.Errorf("validate config after secret bootstrap: %w", err)
	}

	return client, drv.DB(), nil
}

// validateReviewMigrations compares the embedded migration list with the
// existing ledger using the runner's trimmed-content SHA-256 format. An absent
// ledger, missing row, or different checksum fails startup. The review path
// uses exact checksum equality and never creates or changes ledger rows.
func validateReviewMigrations(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT filename, checksum FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("read crypto review migration ledger: %w", err)
	}
	// An early scan error can return before iteration closes the rows. Release
	// that result set before startup returns the pool to application ownership.
	defer func() { _ = rows.Close() }()
	applied := make(map[string]string)
	for rows.Next() {
		var name, checksum string
		if err := rows.Scan(&name, &checksum); err != nil {
			return fmt.Errorf("scan crypto review migration ledger: %w", err)
		}
		applied[name] = checksum
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate crypto review migration ledger: %w", err)
	}

	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return fmt.Errorf("list embedded migrations: %w", err)
	}
	for _, name := range files {
		content, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return fmt.Errorf("read embedded migration %s: %w", name, err)
		}
		trimmed := strings.TrimSpace(string(content))
		if trimmed == "" {
			continue
		}
		checksum, ok := applied[name]
		if !ok {
			return fmt.Errorf("crypto review database is missing migration %s", name)
		}
		if checksum != fmt.Sprintf("%x", sha256.Sum256([]byte(trimmed))) {
			return fmt.Errorf("crypto review migration %s checksum mismatch", name)
		}
	}
	return nil
}

// postgresPoolOptions converts deployment database configuration to pool settings.
func postgresPoolOptions(cfg *config.Config) postgresinfra.PoolOptions {
	return postgresinfra.PoolOptions{
		MaxOpenConns:           cfg.Database.MaxOpenConns,
		MaxIdleConns:           cfg.Database.MaxIdleConns,
		ConnMaxLifetimeMinutes: cfg.Database.ConnMaxLifetimeMinutes,
		ConnMaxIdleTimeMinutes: cfg.Database.ConnMaxIdleTimeMinutes,
	}
}

// applyDBPoolSettings applies deployment connection limits to the shared SQL
// pool. PostgreSQL infrastructure resolves nonpositive or excessive connection
// ages to its default lifetimes before configuring this pool.
func applyDBPoolSettings(db *sql.DB, cfg *config.Config) {
	postgresinfra.ApplyPoolSettings(db, postgresPoolOptions(cfg))
}

// ApplyMigrations runs the embedded migration files through the PostgreSQL
// migration implementation using db and ctx. The caller supplies and owns the
// database pool and receives migration failures directly.
func ApplyMigrations(ctx context.Context, db *sql.DB) error {
	return postgresinfra.ApplyMigrations(ctx, db, migrations.FS)
}
