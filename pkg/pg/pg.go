// Package pg holds the PostgreSQL queries the engine uses for replication
// progress and promotion checks.
package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionSettings are set on every engine connection so that row text, and
// therefore verification hashes, do not depend on each cluster's defaults.
var SessionSettings = map[string]string{
	"TimeZone":           "UTC",
	"DateStyle":          "ISO, MDY",
	"IntervalStyle":      "postgres",
	"extra_float_digits": "1",
	"bytea_output":       "hex",
	// Lets the engine leave its own sessions out when it counts clients.
	"application_name": AppName,
}

// AppName is the application_name of every engine connection.
const AppName = "hopcharlie-engine"

// DB wraps a connection pool.
type DB struct{ Pool *pgxpool.Pool }

// Connect opens a pool and pings it.
func Connect(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 8
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second
	// Verification hashes rows as text, and some types render differently per
	// session (timestamptz follows TimeZone, floats follow extra_float_digits).
	// Pin these so the same data hashes the same on clusters configured differently.
	for k, v := range SessionSettings {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping %s:%d: %w", cfg.ConnConfig.Host, cfg.ConnConfig.Port, err)
	}
	return &DB{Pool: pool}, nil
}

// Close closes the pool.
func (d *DB) Close() {
	if d != nil && d.Pool != nil {
		d.Pool.Close()
	}
}

// InRecovery reports whether the server is a standby.
func (d *DB) InRecovery(ctx context.Context) (bool, error) {
	var b bool
	err := d.Pool.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&b)
	return b, err
}

// PendingRestart reports whether a changed setting waits for a server restart.
// CNPG restarts a freshly promoted primary in place when this is true.
func (d *DB) PendingRestart(ctx context.Context) (bool, error) {
	var b bool
	err := d.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_settings WHERE pending_restart)").Scan(&b)
	return b, err
}

// PostmasterStart is when the server process started. It changes on every
// restart, so two reads that agree prove no restart happened between them,
// even if connections failed in between.
func (d *DB) PostmasterStart(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := d.Pool.QueryRow(ctx, "SELECT pg_postmaster_start_time()").Scan(&t)
	return t, err
}

// ClientSessions counts client sessions other than the engine's own: what is
// still connected to this database from applications, batch jobs or people.
func (d *DB) ClientSessions(ctx context.Context) (int, error) {
	var n int
	err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		WHERE backend_type = 'client backend' AND application_name <> $1`, AppName).Scan(&n)
	return n, err
}

// ErrNoReplayLSN is returned when a standby has not replayed any WAL yet.
var ErrNoReplayLSN = errors.New("standby has no replay LSN yet")

// WALPosition returns the current write LSN on a primary or the replay LSN on a standby.
func (d *DB) WALPosition(ctx context.Context) (string, error) {
	var lsn *string
	err := d.Pool.QueryRow(ctx, `SELECT (CASE WHEN pg_is_in_recovery()
		THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END)::text`).Scan(&lsn)
	if err != nil {
		return "", err
	}
	if lsn == nil {
		return "", ErrNoReplayLSN
	}
	return *lsn, nil
}

// LSNDiff returns a - b in bytes.
func (d *DB) LSNDiff(ctx context.Context, a, b string) (int64, error) {
	var n int64
	err := d.Pool.QueryRow(ctx, "SELECT pg_wal_lsn_diff($1::pg_lsn, $2::pg_lsn)::bigint", a, b).Scan(&n)
	return n, err
}

// Lag measures how far target is behind source in bytes, returning both positions.
func Lag(ctx context.Context, source, target *DB) (lag int64, srcLSN, dstLSN string, err error) {
	if srcLSN, err = source.WALPosition(ctx); err != nil {
		return 0, "", "", fmt.Errorf("source position: %w", err)
	}
	if dstLSN, err = target.WALPosition(ctx); err != nil {
		return 0, srcLSN, "", fmt.Errorf("target position: %w", err)
	}
	if lag, err = source.LSNDiff(ctx, srcLSN, dstLSN); err != nil {
		return 0, srcLSN, dstLSN, err
	}
	if lag < 0 {
		lag = 0
	}
	return lag, srcLSN, dstLSN, nil
}
