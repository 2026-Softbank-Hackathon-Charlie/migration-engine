//go:build integration

// Integration tests against a real primary and streaming standby.
// Run with test/integration/run.sh (needs Docker).
package pg

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func connectEnv(t *testing.T, env string) *DB {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skipf("%s not set; run test/integration/run.sh", env)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", env, err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestRecoveryState(t *testing.T) {
	ctx := context.Background()
	primary := connectEnv(t, "HOPCHARLIE_IT_PRIMARY_DSN")
	standby := connectEnv(t, "HOPCHARLIE_IT_STANDBY_DSN")

	if rec, err := primary.InRecovery(ctx); err != nil || rec {
		t.Fatalf("primary InRecovery = %v, %v; want false", rec, err)
	}
	if rec, err := standby.InRecovery(ctx); err != nil || !rec {
		t.Fatalf("standby InRecovery = %v, %v; want true", rec, err)
	}
}

func TestLSNDiff(t *testing.T) {
	ctx := context.Background()
	primary := connectEnv(t, "HOPCHARLIE_IT_PRIMARY_DSN")

	cases := []struct {
		a, b string
		want int64
	}{
		{"0/3000000", "0/3000000", 0},
		{"0/3000100", "0/3000000", 256},
		{"0/3000000", "0/3000100", -256},
		{"1/0", "0/FFFFFFFF", 1},
	}
	for _, c := range cases {
		got, err := primary.LSNDiff(ctx, c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("LSNDiff(%s, %s) = %d, %v; want %d", c.a, c.b, got, err, c.want)
		}
	}
	if _, err := primary.LSNDiff(ctx, "not-an-lsn", "0/0"); err == nil {
		t.Error("LSNDiff accepted an invalid LSN")
	}
}

// After a write on the primary, the standby's replay position catches up and
// Lag reports zero. This is what presync/delta and the fence wait rely on.
func TestLagCatchesUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	primary := connectEnv(t, "HOPCHARLIE_IT_PRIMARY_DSN")
	standby := connectEnv(t, "HOPCHARLIE_IT_STANDBY_DSN")

	if _, err := primary.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS lag_probe (id bigserial PRIMARY KEY, pad text);
		INSERT INTO lag_probe (pad) SELECT repeat('x', 1000) FROM generate_series(1, 2000)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = primary.Pool.Exec(context.Background(), "DROP TABLE IF EXISTS lag_probe") })

	fence, err := primary.WALPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for {
		lag, src, dst, err := Lag(ctx, primary, standby)
		if err != nil && !errors.Is(err, ErrNoReplayLSN) {
			t.Fatal(err)
		}
		if err == nil {
			if lag < 0 {
				t.Fatalf("negative lag %d", lag)
			}
			behind, derr := primary.LSNDiff(ctx, fence, dst)
			if derr != nil {
				t.Fatal(derr)
			}
			if behind <= 0 {
				t.Logf("standby replayed past fence %s (src %s, dst %s, lag %d)", fence, src, dst, lag)
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("standby did not reach %s: %v", fence, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// The engine's own connections are left out; other clients are counted.
func TestClientSessions(t *testing.T) {
	ctx := context.Background()
	primary := connectEnv(t, "HOPCHARLIE_IT_PRIMARY_DSN")
	other, err := pgxpool.New(ctx, os.Getenv("HOPCHARLIE_IT_PRIMARY_DSN")+"&application_name=some-app")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	base, err := primary.ClientSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := primary.ClientSessions(ctx)
	if err != nil || n != base+1 {
		t.Fatalf("sessions = %d, %v; want %d (one more app session)", n, err, base+1)
	}
}

// The external source fence (ADR 0019) on a real primary and standby.
func TestFenceWritesAndUnfence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dsn := os.Getenv("HOPCHARLIE_IT_PRIMARY_DSN")
	primary := connectEnv(t, "HOPCHARLIE_IT_PRIMARY_DSN")
	standby := connectEnv(t, "HOPCHARLIE_IT_STANDBY_DSN")
	t.Cleanup(func() {
		c, err := Connect(context.Background(), dsn)
		if err == nil {
			_ = c.UnfenceWrites(context.Background())
			c.Close()
		}
	})
	if _, err := primary.Pool.Exec(ctx, `DROP TABLE IF EXISTS fence_it;
		CREATE TABLE fence_it (id serial PRIMARY KEY, name text);
		CREATE INDEX fence_it_name ON fence_it (name);
		INSERT INTO fence_it (name) VALUES ('a'), ('b');`); err != nil {
		t.Fatal(err)
	}
	// The fence ends every other session, idle pool connections included, so
	// each step after it uses a new connection, as the engine's adapter does.
	fresh := func() *DB {
		t.Helper()
		db, err := Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(db.Close)
		return db
	}
	pool := func(params string) *pgxpool.Pool {
		t.Helper()
		p, err := pgxpool.New(ctx, dsn+params)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}

	facts, err := primary.Facts(ctx)
	if err != nil || facts.InRecovery || facts.ReadOnly || !facts.Superuser || !facts.AllowAlterSystem || len(facts.ReadOnlyOverrides) != 0 {
		t.Fatalf("facts before = %+v, %v", facts, err)
	}

	// A role-level override beats the server-wide fence; precheck refuses it.
	if _, err := primary.Pool.Exec(ctx, "ALTER ROLE postgres SET default_transaction_read_only = off"); err != nil {
		t.Fatal(err)
	}
	facts, err = primary.Facts(ctx)
	if _, rerr := primary.Pool.Exec(ctx, "ALTER ROLE postgres RESET default_transaction_read_only"); rerr != nil {
		t.Fatal(rerr)
	}
	if err != nil || len(facts.ReadOnlyOverrides) != 1 || !strings.Contains(facts.ReadOnlyOverrides[0], "postgres@*") {
		t.Fatalf("override not reported: %+v, %v", facts, err)
	}

	// Writes committed with synchronous_commit off just before the fence.
	async := pool("&application_name=async-app")
	if _, err := async.Exec(ctx, "SET synchronous_commit = off; INSERT INTO fence_it (name) SELECT 'async ' || g FROM generate_series(1, 50) g"); err != nil {
		t.Fatal(err)
	}
	// An open read-write transaction, and a session named like the engine's.
	app := pool("&application_name=some-app")
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO fence_it (name) VALUES ('in flight')"); err != nil {
		t.Fatal(err)
	}
	impostor := pool("&application_name=" + AppName)
	if err := impostor.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	f, err := fresh().FenceWrites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f.Terminated < 3 {
		t.Errorf("terminated %d sessions, want at least the app's, the async app's and the impostor's", f.Terminated)
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("the in-flight transaction committed after the fence")
	}
	if _, err := app.Exec(ctx, "INSERT INTO fence_it (name) VALUES ('after')"); err == nil {
		t.Fatal("a new write was accepted after the fence")
	}
	facts, err = fresh().Facts(ctx)
	if err != nil || !facts.ReadOnly || facts.NextXid != f.NextXid || facts.WriteSessions != 0 {
		t.Fatalf("facts after fence = %+v (fence %+v), %v", facts, f, err)
	}

	// The standby reaches the fence LSN and has every committed row,
	// including the asynchronous ones.
	for {
		pos, err := standby.WALPosition(ctx)
		if err == nil {
			if d, err := standby.LSNDiff(ctx, pos, f.LSN); err == nil && d >= 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("standby did not replay to %s", f.LSN)
		case <-time.After(50 * time.Millisecond):
		}
	}
	var n int
	if err := standby.Pool.QueryRow(ctx, "SELECT count(*) FROM fence_it").Scan(&n); err != nil || n != 52 {
		t.Fatalf("standby rows = %d, %v; want 52", n, err)
	}

	// The fence does not stop a session that asks for a read-write
	// transaction; the next xid shows it.
	if _, err := app.Exec(ctx, "BEGIN READ WRITE; INSERT INTO fence_it (name) VALUES ('sneaked'); COMMIT"); err != nil {
		t.Fatalf("BEGIN READ WRITE: %v", err)
	}
	facts, err = fresh().Facts(ctx)
	if err != nil || facts.NextXid == f.NextXid {
		t.Fatalf("a write after the fence left the next xid at %s: %+v, %v", f.NextXid, facts, err)
	}

	primary = fresh()
	if err := primary.UnfenceWrites(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the reload reaches new sessions
	if _, err := pool("&application_name=late-app").Exec(ctx, "INSERT INTO fence_it (name) VALUES ('after unfence')"); err != nil {
		t.Fatalf("write after unfence: %v", err)
	}
	for name, want := range map[string]bool{"public.fence_it_name": true, "public.fence_it": false, "public.nope": false} {
		if ok, err := primary.IsIndex(ctx, name); err != nil || ok != want {
			t.Errorf("IsIndex(%s) = %v, %v; want %v", name, ok, err, want)
		}
	}
	if err := primary.ReindexIndex(ctx, "public.fence_it_name"); err != nil {
		t.Fatal(err)
	}
	if err := primary.ReindexIndex(ctx, "fence_it_name"); err == nil {
		t.Fatal("an unqualified index name was accepted")
	}
}
