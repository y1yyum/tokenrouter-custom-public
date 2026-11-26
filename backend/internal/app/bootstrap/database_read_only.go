package bootstrap

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"

	"github.com/lib/pq"

	postgresinfra "github.com/TokenFlux/TokenRouter/internal/infra/postgres"
)

// database_read_only.go opens the SQL pool used by the read-only review server.
// The session default covers statements executed without an explicit transaction.
// PostgreSQL's pq driver emits READ WRITE for ordinary BeginTx calls, so the
// connector also marks transactions explicitly read-only before they reach pq.

// reviewPostgresConnector wraps pq sessions with the review transaction rule.
type reviewPostgresConnector struct {
	driver.Connector // underlying PostgreSQL connector
}

// reviewPostgresSession preserves the driver's contextual operations and
// pool hooks for connections wrapped by the review connector.
type reviewPostgresSession interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

// reviewPostgresConn fixes transaction mode and delegates other SQL operations.
type reviewPostgresConn struct {
	reviewPostgresSession // physical PostgreSQL connection
}

// openReviewDatabase creates a lazy pool with read-only sessions and optional
// server timing. The caller owns the returned pool and checks database
// readiness through its subsequent queries.
func openReviewDatabase(dsn string, enableTiming bool) (*sql.DB, error) {
	base, err := pq.NewConnector(dsn + " default_transaction_read_only=on")
	if err != nil {
		return nil, err
	}
	var connector driver.Connector = reviewPostgresConnector{Connector: base}
	if enableTiming {
		connector = postgresinfra.NewTimingConnector(connector)
	}
	return sql.OpenDB(connector), nil
}

// Connect returns a read-only review wrapper for an acquired driver
// connection. If the driver cannot preserve the required contextual and pool
// interfaces, it releases the connection and fails startup rather than
// returning an unrestricted session.
func (c reviewPostgresConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	session, ok := conn.(reviewPostgresSession)
	if !ok {
		return nil, errors.Join(errors.New("crypto review PostgreSQL driver lacks required session interfaces"), conn.Close())
	}
	return reviewPostgresConn{reviewPostgresSession: session}, nil
}

// Begin opens an explicit read-only transaction for legacy driver callers.
func (c reviewPostgresConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx starts a read-only transaction at the requested isolation level.
// This overrides the driver's READ WRITE default even when the caller passes
// empty transaction options.
func (c reviewPostgresConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	opts.ReadOnly = true
	return c.reviewPostgresSession.BeginTx(ctx, opts)
}
