package pg

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WriterFence observes and drains source SQL clients after a source-local HBA
// deny has actually rejected a fresh TCP connection for every writer role.
// The source DSN must be a distinct read-only observer with pg_read_all_stats
// and pg_signal_backend. It is never silently elevated or granted privileges.
type WriterFence struct {
	DB    *DB
	DSN   string
	Roles []string
	pid   int32
}

func (f *WriterFence) VerifySystemIdentifier(ctx context.Context, expected string) error {
	actual, err := f.DB.SystemIdentifier(ctx)
	if err != nil {
		return err
	}
	if !SameSystemIdentifier(actual, expected) {
		return errors.New("source PostgreSQL system identifier disagrees with the identified CNPG cluster")
	}
	return nil
}

func (d *DB) SystemIdentifier(ctx context.Context) (string, error) {
	var actual string
	if err := d.Pool.QueryRow(ctx, `SELECT system_identifier::text FROM pg_catalog.pg_control_system()`).Scan(&actual); err != nil {
		return "", fmt.Errorf("SQL system identifier cannot be observed: %w", err)
	}
	if !SameSystemIdentifier(actual, actual) {
		return "", errors.New("SQL system identifier is absent, zero or invalid")
	}
	return actual, nil
}

// SameSystemIdentifier compares actual positive PostgreSQL system identifiers;
// absence, malformed values and zero never count as an identity match.
func SameSystemIdentifier(actual, expected string) bool {
	value, err := strconv.ParseUint(actual, 10, 64)
	return err == nil && value != 0 && actual == expected
}

// CheckpointREDOLSN reads the same control-file checkpoint source used by
// pg_controldata/CNPG tokens. A source that just restarted from its own clean
// shutdown checkpoint can legitimately have no pg_last_wal_replay_lsn yet.
func (d *DB) CheckpointREDOLSN(ctx context.Context) (string, error) {
	var lsn string
	err := d.Pool.QueryRow(ctx, `SELECT redo_lsn::text FROM pg_catalog.pg_control_checkpoint() WHERE redo_lsn>'0/0'::pg_catalog.pg_lsn`).Scan(&lsn)
	if err != nil {
		return "", fmt.Errorf("SQL control checkpoint cannot be observed: %w", err)
	}
	return lsn, nil
}

// The check is conservative: uncertain function or membership authority blocks
// the run. Superusers are a separate administrator trust boundary, not fenced
// application writers. Unix-socket peer access inside a database Pod is likewise
// outside a TCP application fence.
const potentialWritersSQL = `SELECT login.rolname FROM pg_roles login
WHERE login.rolcanlogin AND NOT login.rolsuper AND EXISTS (
 SELECT 1 FROM pg_roles r WHERE
 (r.oid=login.oid OR pg_has_role(login.oid,r.oid,'SET') OR pg_has_role(login.oid,r.oid,'USAGE')) AND (
 r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolbypassrls OR
 has_database_privilege(r.oid,current_database(),'CREATE') OR
 EXISTS(SELECT 1 FROM pg_namespace n WHERE left(n.nspname,3)<>'pg_' AND n.nspname<>'information_schema'
 AND has_schema_privilege(r.oid,n.oid,'CREATE')) OR
 EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE left(n.nspname,3)<>'pg_' AND n.nspname<>'information_schema'
 AND CASE WHEN c.relkind IN ('r','p','v','m','f') THEN
 (has_table_privilege(r.oid,c.oid,'INSERT,UPDATE,DELETE,TRUNCATE,TRIGGER')
 OR has_any_column_privilege(r.oid,c.oid,'INSERT,UPDATE') OR pg_has_role(r.oid,c.relowner,'USAGE')) ELSE false END) OR
 EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE left(n.nspname,3)<>'pg_' AND n.nspname<>'information_schema'
 AND CASE WHEN c.relkind='S' THEN has_sequence_privilege(r.oid,c.oid,'USAGE,UPDATE') ELSE false END) OR
 EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE left(n.nspname,3)<>'pg_' AND n.nspname<>'information_schema' AND p.prosecdef
 AND has_function_privilege(r.oid,p.oid,'EXECUTE'))))
ORDER BY login.rolname`

func (f *WriterFence) CheckRoles(ctx context.Context) error {
	var observer string
	var database string
	var stats, signals, super, recovery bool
	err := f.DB.Pool.QueryRow(ctx, `SELECT current_user,pg_has_role(current_user,'pg_read_all_stats','USAGE'),
	 pg_has_role(current_user,'pg_signal_backend','USAGE'),rolsuper,pg_is_in_recovery()
	 FROM pg_roles WHERE rolname=current_user`).Scan(&observer, &stats, &signals, &super, &recovery)
	if err != nil {
		return err
	}
	if super || !stats || !signals || slices.Contains(f.Roles, observer) || recovery {
		return errors.New("write fence requires a distinct non-superuser source observer with pg_read_all_stats and pg_signal_backend")
	}
	if err = f.DB.Pool.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		return err
	}
	if database == "postgres" || database == "template0" || database == "template1" {
		return errors.New("source write fence refuses PostgreSQL maintenance and template databases")
	}
	var valid int
	err = f.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)
	 AND rolcanlogin AND NOT rolsuper AND NOT rolreplication AND NOT rolcreaterole AND NOT rolcreatedb AND NOT rolbypassrls`, f.Roles).Scan(&valid)
	if err != nil {
		return err
	}
	if valid != len(f.Roles) {
		return errors.New("write fence writer roles must all exist and be ordinary LOGIN roles")
	}
	var privileged int
	err = f.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_roles login WHERE login.rolname=ANY($1) AND EXISTS (
	 SELECT 1 FROM pg_roles r WHERE (r.oid=login.oid OR pg_has_role(login.oid,r.oid,'SET') OR pg_has_role(login.oid,r.oid,'USAGE'))
	 AND (r.rolsuper OR r.rolreplication OR r.rolcreaterole OR r.rolcreatedb OR r.rolbypassrls))`, f.Roles).Scan(&privileged)
	if err != nil {
		return err
	}
	if privileged != 0 {
		return errors.New("write fence refuses application logins that can assume privileged roles")
	}
	rows, err := f.DB.Pool.Query(ctx, potentialWritersSQL)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var role string
		if err = rows.Scan(&role); err != nil {
			return err
		}
		if !slices.Contains(f.Roles, role) {
			return fmt.Errorf("write fence has an undeclared potential writer role %q (including observer or SECURITY DEFINER access)", role)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	var unsupported int
	err = f.DB.Pool.QueryRow(ctx, `SELECT
	 (SELECT count(*) FROM pg_prepared_xacts WHERE database=current_database())+
	 (SELECT count(*) FROM pg_stat_activity a JOIN pg_roles r ON r.oid=a.usesysid
	  WHERE a.datname=current_database() AND r.rolsuper AND a.backend_type='client backend')+
	 (SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()
	  AND backend_type NOT IN ('client backend','autovacuum worker','parallel worker'))`).Scan(&unsupported)
	if err != nil {
		return err
	}
	if unsupported != 0 {
		return errors.New("write fence refuses prepared transactions, active administrator clients, or unsupported background writers")
	}
	var shared int
	err = f.DB.Pool.QueryRow(ctx, `SELECT
	 (SELECT count(*) FROM pg_database WHERE NOT datistemplate AND datname NOT IN ('postgres',current_database()))+
	 (SELECT count(*) FROM pg_stat_activity WHERE usename=ANY($1) AND datname<>current_database())`, f.Roles).Scan(&shared)
	if err != nil {
		return err
	}
	if shared != 0 {
		return errors.New("source write fence requires a dedicated application database cluster and unshared writer roles")
	}
	return nil
}

func (f *WriterFence) Precheck(ctx context.Context) error {
	if err := f.CheckRoles(ctx); err != nil {
		return err
	}
	cfg := f.DB.Pool.Config()
	cfg.MaxConns, cfg.MinConns = 1, 1
	cfg.MaxConnLifetime, cfg.MaxConnIdleTime = 0, 24*time.Hour
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	var pid int32
	if err = p.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		p.Close()
		return err
	}
	f.DB.Pool.Close()
	f.DB.Pool, f.pid = p, pid
	return nil
}

func (f *WriterFence) Pinned(ctx context.Context) error {
	var pid int32
	if err := f.DB.Pool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil || pid != f.pid {
		return errors.New("write fence verifier connection was lost; leave fence and held writes for operator reconciliation")
	}
	return nil
}

// Rejected probes real authentication on every invocation. A missing role, bad
// password, DNS error, refused socket or failed TLS verification is not a fence.
func (f *WriterFence) Rejected(ctx context.Context) error {
	return f.probeAdmission(ctx, true)
}

func (f *WriterFence) Admitted(ctx context.Context) error {
	return f.probeAdmission(ctx, false)
}

func (f *WriterFence) probeAdmission(ctx context.Context, reject bool) error {
	base := f.DB.Pool.Config().ConnConfig.Copy()
	if base.Host == "" || strings.HasPrefix(base.Host, "/") {
		return errors.New("write fence requires a TCP source endpoint")
	}
	for _, role := range f.Roles {
		cfg := base.Copy()
		cfg.User, cfg.Password = role, "deliberately-invalid-write-fence-probe"
		cfg.Fallbacks = nil
		cfg.ConnectTimeout = 2 * time.Second
		probe, pErr := pgx.ConnectConfig(ctx, cfg)
		if probe != nil {
			_ = probe.Close(ctx)
		}
		var pgErr *pgconn.PgError
		isPG := errors.As(pErr, &pgErr)
		if !reject && (pErr == nil || (isPG && pgErr.Code == "28P01")) {
			continue
		}
		if !reject || !isPG || pgErr.Code != "28000" || !strings.Contains(pgErr.Message, "pg_hba.conf rejects connection") {
			return fmt.Errorf("writer role %q is not proven rejected by source HBA", role)
		}
	}
	return nil
}

func (f *WriterFence) Drain(ctx context.Context) (int, error) {
	if err := f.Pinned(ctx); err != nil {
		return 0, err
	}
	if err := f.CheckRoles(ctx); err != nil {
		return 0, err
	}
	if err := f.Rejected(ctx); err != nil {
		return 0, err
	}
	terminated := 0
	for {
		rows, err := f.DB.Pool.Query(ctx, `SELECT pg_terminate_backend(pid,1000)
		 FROM pg_stat_activity WHERE datname=current_database() AND usename=ANY($1)
		 AND backend_type='client backend'`, f.Roles)
		if err != nil {
			return terminated, err
		}
		n := 0
		for rows.Next() {
			var stopped bool
			if err = rows.Scan(&stopped); err != nil {
				rows.Close()
				return terminated, err
			}
			if !stopped {
				rows.Close()
				return terminated, errors.New("writer backend did not terminate")
			}
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return terminated, err
		}
		terminated += n
		if n == 0 {
			return terminated, f.AssertDrained(ctx)
		}
		select {
		case <-ctx.Done():
			return terminated, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (f *WriterFence) AssertDrained(ctx context.Context) error {
	if err := f.Pinned(ctx); err != nil {
		return err
	}
	if err := f.CheckRoles(ctx); err != nil {
		return err
	}
	if err := f.Rejected(ctx); err != nil {
		return err
	}
	var clients int
	if err := f.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
	 WHERE datname=current_database() AND usename=ANY($1)`, f.Roles).Scan(&clients); err != nil {
		return err
	}
	if clients != 0 {
		return errors.New("writer sessions remain after source HBA fence")
	}
	return nil
}
