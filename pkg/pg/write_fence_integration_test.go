//go:build integration && writefence

package pg

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This changes only the disposable compose primary's pg_hba.conf. The explicit
// fixture flag and endpoint guard prevent running it as a generic DB probe.
func TestSourceHBAWriteFence(t *testing.T) {
	if os.Getenv("HOPCHARLIE_IT_HBA_FIXTURE") != "disposable-compose" {
		t.Skip("disposable HBA fixture not enabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srcConfig, err := pgx.ParseConfig(os.Getenv("HOPCHARLIE_IT_PRIMARY_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	if srcConfig.Host != "127.0.0.1" || srcConfig.Port != 55432 || srcConfig.Database != "shop" || srcConfig.User != "postgres" {
		t.Fatal("refusing HBA fixture outside the disposable integration endpoint")
	}
	maintenance := srcConfig.Copy()
	maintenance.Database = "postgres"
	control, err := pgx.ConnectConfig(ctx, maintenance)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close(context.Background())
	var original, path string
	if err = control.QueryRow(ctx, `SELECT current_setting('hba_file'),pg_read_file(current_setting('hba_file'))`).Scan(&path, &original); err != nil {
		t.Fatal(err)
	}
	writeHBA := func(text string) {
		t.Helper()
		// The fixture has no backslash-containing authentication options. COPY
		// text preserves its plain host/scram rules and comments.
		quoted := "'" + strings.ReplaceAll(text, "'", "''") + "'"
		file := "'" + strings.ReplaceAll(path, "'", "''") + "'"
		if _, e := control.Exec(ctx, `COPY (SELECT unnest(string_to_array(`+quoted+`,E'\n'))) TO `+file); e != nil {
			t.Fatal(e)
		}
		if _, e := control.Exec(ctx, `SELECT pg_reload_conf()`); e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		cleanupControl, e := pgx.ConnectConfig(cleanup, maintenance)
		if e != nil {
			return
		}
		defer cleanupControl.Close(cleanup)
		quoted := "'" + strings.ReplaceAll(original, "'", "''") + "'"
		file := "'" + strings.ReplaceAll(path, "'", "''") + "'"
		_, _ = cleanupControl.Exec(cleanup, `COPY (SELECT unnest(string_to_array(`+quoted+`,E'\n'))) TO `+file)
		_, _ = cleanupControl.Exec(cleanup, `SELECT pg_reload_conf()`)
		cfg := srcConfig.Copy()
		admin, e := pgx.ConnectConfig(cleanup, cfg)
		if e == nil {
			_, _ = admin.Exec(cleanup, `DROP FUNCTION IF EXISTS public.pg_control_system(),public.pg_control_checkpoint(); DROP SCHEMA IF EXISTS pgdata CASCADE; DROP TABLE IF EXISTS public.fence_probe; DROP OWNED BY hop_fence_observer,hop_fence_writer,hop_fence_reader,hop_fence_hidden; DROP ROLE IF EXISTS hop_fence_observer,hop_fence_reader,hop_fence_writer,hop_fence_hidden,hop_fence_privileged`)
			_ = admin.Close(cleanup)
		}
	})
	_, err = control.Exec(ctx, `CREATE ROLE hop_fence_hidden LOGIN PASSWORD 'fixture-only'; CREATE ROLE hop_fence_writer LOGIN PASSWORD 'fixture-only'; CREATE ROLE hop_fence_reader LOGIN PASSWORD 'fixture-only'; GRANT hop_fence_writer TO hop_fence_reader WITH INHERIT FALSE, SET TRUE; CREATE ROLE hop_fence_observer LOGIN PASSWORD 'fixture-only'; GRANT pg_read_all_stats,pg_signal_backend TO hop_fence_observer`)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, srcConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.Exec(ctx, `GRANT EXECUTE ON FUNCTION pg_control_system(),pg_control_checkpoint() TO hop_fence_observer; CREATE SCHEMA pgdata; CREATE TABLE pgdata.hidden(id integer); GRANT USAGE ON SCHEMA pgdata TO hop_fence_hidden; CREATE TABLE public.fence_probe(id integer PRIMARY KEY); GRANT SELECT ON public.fence_probe TO hop_fence_observer,hop_fence_writer; GRANT INSERT ON public.fence_probe TO hop_fence_writer`)
	_ = admin.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// pgx.Config.ConnString returns the original parsed string; mutating User
	// on a copy does not serialize a new observer identity.
	observerDSN := "postgres://hop_fence_observer:fixture-only@127.0.0.1:55432/shop?sslmode=disable"
	observer, err := Connect(ctx, observerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	fence := &WriterFence{DB: observer, DSN: observerDSN, Roles: []string{"hop_fence_writer"}}
	if err = fence.CheckRoles(ctx); err == nil || !strings.Contains(err.Error(), "hop_fence_reader") {
		t.Fatalf("INHERIT FALSE / SET TRUE writer login bypassed inventory: %v", err)
	}
	fence.Roles = append(fence.Roles, "hop_fence_reader")
	admin, err = pgx.ConnectConfig(ctx, srcConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.Exec(ctx, `GRANT INSERT ON pgdata.hidden TO hop_fence_hidden`)
	_ = admin.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = fence.CheckRoles(ctx); err == nil || !strings.Contains(err.Error(), "hop_fence_hidden") {
		t.Fatalf("writer with INSERT only on pgdata.hidden bypassed inventory: %v", err)
	}
	// Remove only this fixture grant after proving the namespace regression.
	admin, err = pgx.ConnectConfig(ctx, srcConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.Exec(ctx, `REVOKE INSERT ON pgdata.hidden FROM hop_fence_hidden`)
	_ = admin.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = control.Exec(ctx, `CREATE ROLE hop_fence_privileged NOLOGIN CREATEDB; GRANT hop_fence_privileged TO hop_fence_reader WITH INHERIT FALSE, SET TRUE`); err != nil {
		t.Fatal(err)
	}
	if err = fence.CheckRoles(ctx); err == nil || !strings.Contains(err.Error(), "assume privileged") {
		t.Fatalf("declared login could assume a privileged NOLOGIN role: %v", err)
	}
	if _, err = control.Exec(ctx, `REVOKE hop_fence_privileged FROM hop_fence_reader; DROP ROLE hop_fence_privileged`); err != nil {
		t.Fatal(err)
	}
	if err = fence.Precheck(ctx); err != nil {
		t.Fatal(err)
	}
	var systemID string
	if err = control.QueryRow(ctx, `SELECT system_identifier::text FROM pg_control_system()`).Scan(&systemID); err != nil {
		t.Fatal(err)
	}
	if err = fence.VerifySystemIdentifier(ctx, systemID); err != nil {
		t.Fatal(err)
	}
	admin, err = pgx.ConnectConfig(ctx, srcConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.Exec(ctx, `CREATE FUNCTION public.pg_control_system() RETURNS TABLE(system_identifier bigint) LANGUAGE sql AS 'SELECT 0::bigint'; CREATE FUNCTION public.pg_control_checkpoint() RETURNS TABLE(redo_lsn pg_lsn) LANGUAGE sql AS $$SELECT '0/0'::pg_lsn$$`)
	_ = admin.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = observer.Pool.Exec(ctx, `SET search_path=public,pg_catalog`); err != nil {
		t.Fatal(err)
	}
	var spoofed string
	if err = observer.Pool.QueryRow(ctx, `SELECT system_identifier::text FROM pg_control_system()`).Scan(&spoofed); err != nil || spoofed != "0" {
		t.Fatalf("catalog-shadowing fixture not active: value=%q err=%v", spoofed, err)
	}
	if err = fence.VerifySystemIdentifier(ctx, systemID); err != nil {
		t.Fatalf("application function shadowed the authoritative SQL system identifier: %v", err)
	}
	if lsn, e := observer.CheckpointREDOLSN(ctx); e != nil || lsn == "" || lsn == "0/0" {
		t.Fatalf("control checkpoint lsn=%q err=%v", lsn, e)
	}
	writerConfig := srcConfig.Copy()
	writerConfig.User, writerConfig.Password = "hop_fence_writer", "fixture-only"
	writer, err := pgx.ConnectConfig(ctx, writerConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close(context.Background())
	tx, err := writer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO public.fence_probe VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	if err = fence.Rejected(ctx); err == nil {
		t.Fatal("a password rejection was mistaken for an HBA fence")
	}
	readerConfig := writerConfig.Copy()
	readerConfig.User = "hop_fence_reader"
	reader, err := pgx.ConnectConfig(ctx, readerConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close(context.Background())
	readerTx, err := reader.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = readerTx.Exec(ctx, `SET ROLE hop_fence_writer; INSERT INTO public.fence_probe VALUES(2)`); err != nil {
		t.Fatal(err)
	}
	writeHBA("host all hop_fence_writer 0.0.0.0/0 reject\nhost all hop_fence_writer ::/0 reject\nhost all hop_fence_reader 0.0.0.0/0 reject\nhost all hop_fence_reader ::/0 reject\n" + original)
	for fence.Rejected(ctx) != nil {
		select {
		case <-ctx.Done():
			t.Fatal("HBA deny never became active")
		case <-time.After(10 * time.Millisecond):
		}
	}
	n, err := fence.Drain(ctx)
	if err != nil || n != 2 {
		t.Fatalf("drain=%d err=%v", n, err)
	}
	if err = tx.Commit(ctx); err == nil {
		t.Fatal("an existing direct writer committed through the fence")
	}
	if err = readerTx.Commit(ctx); err == nil {
		t.Fatal("SET ROLE writer session committed through the fence")
	}
	var count int
	if err = observer.Pool.QueryRow(ctx, `SELECT count(*) FROM public.fence_probe`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("old transaction survived: count=%d err=%v", count, err)
	}
	// Source-local HBA is not WAL-replicated: a new target reader using the
	// same writer LOGIN must remain able to connect and read during the fence.
	targetConfig, err := pgx.ParseConfig(os.Getenv("HOPCHARLIE_IT_STANDBY_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	targetConfig.User, targetConfig.Password = "hop_fence_writer", "fixture-only"
	var target *pgx.Conn
	for {
		target, err = pgx.ConnectConfig(ctx, targetConfig)
		if err == nil {
			if err = target.QueryRow(ctx, `SELECT count(*) FROM public.fence_probe`).Scan(&count); err == nil {
				break
			}
			_ = target.Close(ctx)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("target reader unavailable during source fence: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	_ = target.Close(ctx)
	if err = fence.AssertDrained(ctx); err != nil {
		t.Fatal(err)
	}
	writeHBA(original)
	for fence.Admitted(ctx) != nil {
		select {
		case <-ctx.Done():
			t.Fatal("original HBA never restored")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
