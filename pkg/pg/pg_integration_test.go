//go:build integration

// Integration tests against a real primary and streaming standby.
// Run with test/integration/run.sh (needs Docker).
package pg

import (
	"context"
	"errors"
	"os"
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
