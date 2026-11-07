// Package pg holds the PostgreSQL queries the engine uses for replication
// progress and promotion checks.
package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
	return connectWithPeer(ctx, dsn, "")
}

// ConnectPinned adds the expected TLS peer before opening any connection or
// sending authentication credentials. Checking a pool only after Ping is late.
func ConnectPinned(ctx context.Context, dsn, expected string) (*DB, error) {
	if expected == "" {
		return nil, errors.New("SQL endpoint fingerprint is required")
	}
	return connectWithPeer(ctx, dsn, expected)
}

func connectWithPeer(ctx context.Context, dsn, expected string) (*DB, error) {
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
	if expected != "" {
		if err = pinPoolConfig(cfg, expected); err != nil {
			return nil, err
		}
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

// SettleStatus reads the three promotion-stability signals in one SQL statement.
// The engine still opens and closes a fresh connection for each observation so
// these probes cannot keep CNPG's smart shutdown waiting on an idle client.
func (d *DB) SettleStatus(ctx context.Context) (inRecovery, pendingRestart bool, postmasterStart time.Time, err error) {
	err = d.Pool.QueryRow(ctx, `SELECT pg_is_in_recovery(),
		EXISTS (SELECT 1 FROM pg_settings WHERE pending_restart),
		pg_postmaster_start_time()`).Scan(&inRecovery, &pendingRestart, &postmasterStart)
	return
}

// ServerVersionNum is server_version_num: major*10000 + minor.
func (d *DB) ServerVersionNum(ctx context.Context) (int, error) {
	var n int
	err := d.Pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&n)
	return n, err
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

// External source fence (ADR 0019). These need a superuser connection.

// SourceFacts is what the engine reads from an external source before the
// fence and again right before the target's promotion.
type SourceFacts struct {
	InRecovery bool `json:"inRecovery"`
	// ReadOnly is default_transaction_read_only in a new session.
	ReadOnly bool `json:"defaultTransactionReadOnly"`
	// NextXid is the next transaction id (pg_snapshot_xmax). Read-only work
	// assigns none, so it moves only if something wrote.
	NextXid string `json:"nextXid"`
	// WriteSessions counts other client sessions holding a transaction id: an
	// open write that could still commit.
	WriteSessions int `json:"writeSessions"`
	// ReadOnlyOverrides lists ALTER ROLE/DATABASE ... SET entries of
	// default_transaction_read_only, which beat the server-wide fence.
	ReadOnlyOverrides []string `json:"readOnlyOverrides,omitempty"`
	Superuser         bool     `json:"superuser"`
	// AllowAlterSystem is allow_alter_system (PostgreSQL 17+; true before).
	AllowAlterSystem bool `json:"allowAlterSystem"`
}

// Facts reads SourceFacts.
func (d *DB) Facts(ctx context.Context) (SourceFacts, error) {
	var f SourceFacts
	err := d.Pool.QueryRow(ctx, `SELECT pg_is_in_recovery(),
		current_setting('default_transaction_read_only')::bool,
		pg_snapshot_xmax(pg_current_snapshot())::text,
		(SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'
			AND pid <> pg_backend_pid() AND backend_xid IS NOT NULL),
		coalesce((SELECT array_agg(coalesce(r.rolname, '*') || '@' || coalesce(db.datname, '*') || ': ' || s.c ORDER BY 1)
			FROM pg_db_role_setting rs
			CROSS JOIN LATERAL unnest(rs.setconfig) AS s(c)
			LEFT JOIN pg_roles r ON r.oid = rs.setrole
			LEFT JOIN pg_database db ON db.oid = rs.setdatabase
			WHERE s.c ILIKE 'default_transaction_read_only=%'), '{}'),
		(SELECT rolsuper FROM pg_roles WHERE rolname = current_user),
		coalesce(current_setting('allow_alter_system', true), 'on')::bool`).Scan(
		&f.InRecovery, &f.ReadOnly, &f.NextXid, &f.WriteSessions, &f.ReadOnlyOverrides, &f.Superuser, &f.AllowAlterSystem)
	return f, err
}

// DefaultReadOnly reports whether default_transaction_read_only is on for
// new transactions, which is what FenceWrites turns on.
func (d *DB) DefaultReadOnly(ctx context.Context) (bool, error) {
	var b bool
	err := d.Pool.QueryRow(ctx, "SELECT current_setting('default_transaction_read_only')::bool").Scan(&b)
	return b, err
}

// Fence is what FenceWrites recorded.
type Fence struct {
	// LSN is the WAL insert position after the fence; it is flushed, so a
	// standby reaches it.
	LSN string `json:"lsn"`
	// NextXid is SourceFacts.NextXid right after the fence.
	NextXid    string `json:"nextXid"`
	Terminated int64  `json:"terminated"`
}

// fenceWait bounds how long FenceWrites waits for the reload to reach new
// sessions and for each ended session to exit.
const fenceWait = 5 * time.Second

// FenceWrites makes a primary outside CNPG refuse new writes:
// default_transaction_read_only on for the whole server (ALTER SYSTEM,
// reload), checked in a new session; then every other client session ends,
// waiting until each has exited, so no transaction that began read-write is
// left. The returned LSN is the WAL insert position after that, flushed so
// commits made with synchronous_commit off are included; the returned xid is
// the next one to be assigned. Roles are not changed: role changes replicate
// to the target. The fence does not stop a session that asks for a
// read-write transaction itself; the caller compares NextXid later.
func (d *DB) FenceWrites(ctx context.Context) (Fence, error) {
	var f Fence
	for _, q := range []string{"ALTER SYSTEM SET default_transaction_read_only = on", "SELECT pg_reload_conf()"} {
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			return f, fmt.Errorf("%s: %w", q, err)
		}
	}
	if err := d.waitNewSessionReadOnly(ctx); err != nil {
		return f, err
	}
	// pg_terminate_backend(pid, timeout) waits until the session has exited.
	// It is false on a timeout and for a session that ended by itself in
	// between, so the ones it did not confirm are looked up once more.
	var left []int32
	if err := d.Pool.QueryRow(ctx, `WITH t AS (SELECT pid, pg_terminate_backend(pid, $1) AS done
			FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid())
		SELECT count(*) FILTER (WHERE done), coalesce(array_agg(pid) FILTER (WHERE NOT done), '{}') FROM t`,
		fenceWait.Milliseconds()).Scan(&f.Terminated, &left); err != nil {
		return f, fmt.Errorf("terminate client sessions: %w", err)
	}
	if len(left) > 0 {
		var n int
		if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE pid = ANY($1)", left).Scan(&n); err != nil {
			return f, fmt.Errorf("check ended sessions: %w", err)
		}
		if n > 0 {
			return f, fmt.Errorf("%d client sessions did not exit within %s", n, fenceWait)
		}
	}
	var flushed bool
	if err := d.Pool.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn()::text,
		pg_current_wal_flush_lsn() >= pg_current_wal_insert_lsn(),
		pg_snapshot_xmax(pg_current_snapshot())::text`).Scan(&f.LSN, &flushed, &f.NextXid); err != nil {
		return f, fmt.Errorf("read fence position: %w", err)
	}
	if !flushed {
		// Only commits flush WAL. A tail that holds no synchronous commit, or
		// one on a partial page, can stay unflushed, and a standby never
		// receives unflushed WAL. A segment switch flushes it.
		if _, err := d.Pool.Exec(ctx, "SELECT pg_switch_wal()"); err != nil {
			return f, fmt.Errorf("flush WAL: %w", err)
		}
	}
	return f, nil
}

// waitNewSessionReadOnly polls new sessions until the reload has reached them.
func (d *DB) waitNewSessionReadOnly(ctx context.Context) error {
	wctx, cancel := context.WithTimeout(ctx, fenceWait)
	defer cancel()
	var last error
	for {
		conn, err := pgx.ConnectConfig(wctx, d.Pool.Config().ConnConfig.Copy())
		if err == nil {
			var ro bool
			err = conn.QueryRow(wctx, "SELECT current_setting('default_transaction_read_only')::bool").Scan(&ro)
			conn.Close(context.Background())
			if err == nil && ro {
				return nil
			}
		}
		if err != nil {
			last = err
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("new sessions are not read-only after the reload (last error: %v)", last)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// UnfenceWrites undoes FenceWrites: new transactions are read-write again.
func (d *DB) UnfenceWrites(ctx context.Context) error {
	for _, q := range []string{"ALTER SYSTEM RESET default_transaction_read_only", "SELECT pg_reload_conf()"} {
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}

func indexIdent(name string) (string, error) {
	schema, index, ok := strings.Cut(name, ".")
	if !ok || schema == "" || index == "" {
		return "", fmt.Errorf("index %q is not schema.index", name)
	}
	return pgx.Identifier{schema, index}.Sanitize(), nil
}

// IsIndex reports whether "schema.index" names an index (plain or partitioned).
func (d *DB) IsIndex(ctx context.Context, name string) (bool, error) {
	ident, err := indexIdent(name)
	if err != nil {
		return false, err
	}
	var ok bool
	err = d.Pool.QueryRow(ctx, `SELECT coalesce((SELECT c.relkind IN ('i', 'I') FROM pg_class c
		WHERE c.oid = to_regclass($1)), false)`, ident).Scan(&ok)
	return ok, err
}

// ReindexIndex rebuilds one index, named "schema.index".
func (d *DB) ReindexIndex(ctx context.Context, name string) error {
	ident, err := indexIdent(name)
	if err != nil {
		return err
	}
	_, err = d.Pool.Exec(ctx, "REINDEX INDEX "+ident)
	return err
}
