//go:build integration

// Integration tests against real PostgreSQL. Run with test/integration/run.sh.
package verify_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/pg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/verify"
)

const schema = `
DROP SCHEMA IF EXISTS it CASCADE;
CREATE SCHEMA it;
CREATE TABLE it.orders (id bigint PRIMARY KEY, total numeric(10,2), placed_at timestamptz, note text);
CREATE TABLE it.order_items (order_id bigint, line int, sku text, PRIMARY KEY (order_id, line));
CREATE TABLE it.audit (msg text);
INSERT INTO it.orders SELECT g, g * 1.5, timestamptz '2026-11-01 09:00:00+00' + g * interval '1 minute', 'n' || g
  FROM generate_series(1, 500) g;
INSERT INTO it.order_items SELECT g, l, 'sku-' || l FROM generate_series(1, 200) g, generate_series(1, 3) l;
INSERT INTO it.audit SELECT 'event ' || g FROM generate_series(1, 50) g;
`

// pair gives a source database and a separate, writable target database on the
// same server, loaded with identical data.
func pair(t *testing.T) (src, dst *pg.DB) {
	t.Helper()
	dsn := os.Getenv("HOPCHARLIE_IT_PRIMARY_DSN")
	if dsn == "" {
		t.Skip("HOPCHARLIE_IT_PRIMARY_DSN not set; run test/integration/run.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	src = connect(t, ctx, dsn)

	// The target database simulates a cluster configured with another time zone.
	_, _ = src.Pool.Exec(ctx, "DROP DATABASE IF EXISTS shop_target WITH (FORCE)")
	if _, err := src.Pool.Exec(ctx, "CREATE DATABASE shop_target"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Pool.Exec(ctx, "ALTER DATABASE shop_target SET timezone = 'Asia/Seoul'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = src.Pool.Exec(context.Background(), "DROP DATABASE IF EXISTS shop_target WITH (FORCE)")
	})
	dst = connect(t, ctx, strings.Replace(dsn, "/shop?", "/shop_target?", 1))

	for _, db := range []*pg.DB{src, dst} {
		if _, err := db.Pool.Exec(ctx, schema); err != nil {
			t.Fatal(err)
		}
	}
	return src, dst
}

func connect(t *testing.T, ctx context.Context, dsn string) *pg.DB {
	t.Helper()
	db, err := pg.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func tables(t *testing.T, db *pg.DB) []verify.Table {
	t.Helper()
	ts, err := verify.ListTables(context.Background(), db.Pool, []string{"it"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestListTablesFindsPrimaryKeys(t *testing.T) {
	src, _ := pair(t)
	got := map[string][]string{}
	for _, tb := range tables(t, src) {
		got[tb.String()] = tb.PK
	}
	want := map[string]string{"it.audit": "", "it.order_items": "order_id,line", "it.orders": "id"}
	if len(got) != len(want) {
		t.Fatalf("tables = %v", got)
	}
	for name, pk := range want {
		if strings.Join(got[name], ",") != pk {
			t.Errorf("%s PK = %v, want %q", name, got[name], pk)
		}
	}

	only, err := verify.ListTables(context.Background(), src.Pool, []string{"it"}, []string{"orders", "it.audit"})
	if err != nil || len(only) != 2 {
		t.Fatalf("filtered tables = %v, %v", only, err)
	}
}

// Identical data must hash identically even when the two clusters use
// different server time zones (timestamptz renders differently per session).
func TestCompareTablesMatchesIdenticalData(t *testing.T) {
	src, dst := pair(t)
	res, ok := verify.CompareTables(context.Background(), src.Pool, dst.Pool, tables(t, src))
	if !ok {
		t.Fatalf("identical data reported as different: %+v", res)
	}
	for _, r := range res {
		if r.SourceCount == 0 || r.SourceHash == "" {
			t.Errorf("%s: empty count or hash: %+v", r.Table, r)
		}
	}
}

func TestCompareTablesDetectsDifferences(t *testing.T) {
	cases := []struct {
		name, change, table string
	}{
		{"changed value", "UPDATE it.orders SET note = 'changed' WHERE id = 42", "it.orders"},
		{"missing row", "DELETE FROM it.order_items WHERE order_id = 7 AND line = 2", "it.order_items"},
		{"extra row", "INSERT INTO it.orders VALUES (9999, 1, now(), 'extra')", "it.orders"},
		{"table without PK", "UPDATE it.audit SET msg = 'x' WHERE msg = 'event 3'", "it.audit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src, dst := pair(t)
			if _, err := dst.Pool.Exec(context.Background(), c.change); err != nil {
				t.Fatal(err)
			}
			res, ok := verify.CompareTables(context.Background(), src.Pool, dst.Pool, tables(t, src))
			if ok {
				t.Fatal("difference not detected")
			}
			for _, r := range res {
				if r.Match == (r.Table == c.table) {
					t.Errorf("%s: match=%v (changed table is %s)", r.Table, r.Match, c.table)
				}
			}
		})
	}
}

func TestMissingRows(t *testing.T) {
	src, dst := pair(t)
	ctx := context.Background()
	// The target may have newer rows after cutover; only missing source rows count.
	if _, err := dst.Pool.Exec(ctx, `
		DELETE FROM it.orders WHERE id IN (10, 20);
		DELETE FROM it.order_items WHERE order_id = 5 AND line = 3;
		INSERT INTO it.orders VALUES (10001, 1, now(), 'newer on target');
		DELETE FROM it.audit;`); err != nil {
		t.Fatal(err)
	}
	res, total := verify.MissingRows(ctx, src.Pool, dst.Pool, tables(t, src))
	if total != 3 {
		t.Fatalf("total missing = %d, want 3: %+v", total, res)
	}
	by := map[string]verify.MissingResult{}
	for _, r := range res {
		by[r.Table] = r
	}
	if r := by["it.orders"]; r.Missing != 2 || r.Checked != 500 || strings.Join(r.Samples, ",") != "10,20" && strings.Join(r.Samples, ",") != "20,10" {
		t.Errorf("orders = %+v", r)
	}
	if r := by["it.order_items"]; r.Missing != 1 || len(r.Samples) != 1 || r.Samples[0] != "(5,3)" {
		t.Errorf("order_items = %+v", r)
	}
	if r := by["it.audit"]; r.Skipped == "" || r.Missing != 0 {
		t.Errorf("audit (no PK) = %+v, want skipped", r)
	}
}

// The k6 log uses the same key text as MissingRows: "42" and "(1,2)".
func TestCheckWritten(t *testing.T) {
	src, dst := pair(t)
	ctx := context.Background()
	if _, err := dst.Pool.Exec(ctx, "DELETE FROM it.orders WHERE id = 300"); err != nil {
		t.Fatal(err)
	}
	recs := []verify.WrittenRecord{
		{Table: "orders", PK: "1"},
		{Table: "it.orders", PK: "300"},
		{Table: "order_items", PK: "(7,2)"},
		{Table: "audit", PK: "x"},
		{Table: "nope", PK: "1"},
	}
	res := verify.CheckWritten(ctx, dst.Pool, tables(t, src), recs)
	if res.Checked != 3 || res.Missing != 3 || len(res.Errors) != 2 {
		t.Fatalf("result = %+v; want 3 checked, 3 missing (1 row + 2 unverifiable), 2 errors", res)
	}
	if len(res.Samples) != 1 || res.Samples[0] != "it.orders:300" {
		t.Errorf("samples = %v", res.Samples)
	}
}

// After cutover the old source is a replica of the new primary and keeps
// receiving the rows written there. MissingRows must not report those as
// missing (seen in a local end-to-end run under load).
func TestMissingRowsWithSourceFollowingTarget(t *testing.T) {
	primaryDSN, standbyDSN := os.Getenv("HOPCHARLIE_IT_PRIMARY_DSN"), os.Getenv("HOPCHARLIE_IT_STANDBY_DSN")
	if primaryDSN == "" || standbyDSN == "" {
		t.Skip("run test/integration/run.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	target := connect(t, ctx, primaryDSN) // the new primary
	source := connect(t, ctx, standbyDSN) // the old source, now following it
	if _, err := target.Pool.Exec(ctx, `DROP TABLE IF EXISTS follow; CREATE TABLE follow (id bigserial PRIMARY KEY, pad text);
		INSERT INTO follow (pad) SELECT repeat('x', 200) FROM generate_series(1, 20000)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = target.Pool.Exec(context.Background(), "DROP TABLE IF EXISTS follow") })

	wctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { // keep writing on the target while verifying
		defer close(done)
		for wctx.Err() == nil {
			_, _ = target.Pool.Exec(wctx, "INSERT INTO follow (pad) SELECT repeat('y', 200) FROM generate_series(1, 200)")
		}
	}()
	tbl := []verify.Table{{Schema: "public", Name: "follow", PK: []string{"id"}}}
	for i := 0; i < 60; i++ {
		res, missing := verify.MissingRows(ctx, source.Pool, target.Pool, tbl)
		if missing != 0 {
			stop()
			<-done
			t.Fatalf("round %d: %d rows reported missing although the source only follows the target: %+v", i, missing, res)
		}
	}
	stop()
	<-done
}
