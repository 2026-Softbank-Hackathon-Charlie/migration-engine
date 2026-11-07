//go:build integration

package verify_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/verify"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBoundedBatchVerificationAndCompositeKeys(t *testing.T) {
	src, dst := pair(t)
	calls := 0
	if _, err := dst.Pool.Exec(t.Context(), `DELETE FROM it.orders WHERE id=10; DELETE FROM it.order_items WHERE order_id=5 AND line=3`); err != nil {
		t.Fatal(err)
	}
	results, missing := verify.MissingRowsWithOptions(t.Context(), src.Pool, dst.Pool, tables(t, src), verify.ScanOptions{BatchSize: 17, QueryTimeout: time.Second, BeforeBatch: func(context.Context) error { calls++; return nil }})
	if missing != 2 || calls < 60 {
		t.Fatalf("missing=%d batches=%d results=%+v", missing, calls, results)
	}
	for _, r := range results {
		if r.Error != "" {
			t.Fatal(r.Error)
		}
	}
}
func TestVerificationBudgetFailureIsNotCleanResult(t *testing.T) {
	src, dst := pair(t)
	calls := 0
	results, _ := verify.MissingRowsWithOptions(t.Context(), src.Pool, dst.Pool, []verify.Table{{Schema: "it", Name: "orders", PK: []string{"id"}}}, verify.ScanOptions{BatchSize: 7, BeforeBatch: func(context.Context) error {
		calls++
		if calls > 3 {
			return errors.New("over budget")
		}
		return nil
	}})
	if len(results) != 1 || results[0].Error == "" || results[0].Checked >= 500 {
		t.Fatalf("incomplete scan accepted: %+v", results)
	}
	if err := src.Pool.Ping(t.Context()); err != nil {
		t.Fatal("source cursor connection was not returned")
	}
}
func TestLockedTargetQueryTimesOutAndReleasesSourceCursor(t *testing.T) {
	src, dst := pair(t)
	tx, err := dst.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `LOCK TABLE it.orders IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	results, _ := verify.MissingRowsWithOptions(t.Context(), src.Pool, dst.Pool, []verify.Table{{Schema: "it", Name: "orders", PK: []string{"id"}}}, verify.ScanOptions{BatchSize: 10, QueryTimeout: 30 * time.Millisecond})
	if results[0].Error == "" || time.Since(start) > 2*time.Second {
		t.Fatalf("unbounded query: %+v", results)
	}
	if err := src.Pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func TestQuotedCompositeTextKeysUseTypedLookup(t *testing.T) {
	src, dst := pair(t)
	query := `CREATE TABLE it.quoted ("a'b" text, "other" text, PRIMARY KEY ("a'b",other)); INSERT INTO it.quoted VALUES ('a,b','quote"and\\slash'),('','(empty)')`
	if _, err := src.Pool.Exec(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Pool.Exec(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	table := verify.Table{Schema: "it", Name: "quoted", PK: []string{"a'b", "other"}}
	rows, err := src.Pool.Query(t.Context(), `SELECT ROW("a'b",other)::text FROM it.quoted`)
	if err != nil {
		t.Fatal(err)
	}
	var records []verify.WrittenRecord
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		records = append(records, verify.WrittenRecord{Table: "it.quoted", PK: key})
	}
	rows.Close()
	result := verify.CheckWrittenWithOptions(t.Context(), dst.Pool, []verify.Table{table}, records, verify.ScanOptions{BatchSize: 1})
	if result.Missing != 0 || result.Checked != 2 || len(result.Errors) != 0 {
		t.Fatalf("quoted keys failed: %+v", result)
	}
	compared, missing := verify.MissingRowsWithOptions(t.Context(), src.Pool, dst.Pool, []verify.Table{table}, verify.ScanOptions{BatchSize: 1})
	if missing != 0 || compared[0].Error != "" || compared[0].Checked != 2 {
		t.Fatalf("quoted batch failed: %+v", compared)
	}
}

func TestNonKeyNotNullDomainAndEmptyTarget(t *testing.T) {
	src, dst := pair(t)
	query := `CREATE DOMAIN it.required_text AS text NOT NULL; CREATE TABLE it.domained (id int PRIMARY KEY, note it.required_text); INSERT INTO it.domained VALUES (1,'valid'),(2,'another')`
	for _, pool := range []*pgxpool.Pool{src.Pool, dst.Pool} {
		if _, err := pool.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	table := verify.Table{Schema: "it", Name: "domained", PK: []string{"id"}}
	for _, empty := range []bool{false, true} {
		if empty {
			if _, err := dst.Pool.Exec(t.Context(), `DELETE FROM it.domained`); err != nil {
				t.Fatal(err)
			}
		}
		results, missing := verify.MissingRowsWithOptions(t.Context(), src.Pool, dst.Pool, []verify.Table{table}, verify.ScanOptions{BatchSize: 1})
		want := int64(0)
		if empty {
			want = 2
		}
		if missing != want || results[0].Error != "" || results[0].Checked != 2 {
			t.Fatalf("empty=%v results=%+v", empty, results)
		}
		record := verify.WrittenRecord{Table: table.String(), PK: "1", Absent: empty}
		written := verify.CheckWrittenWithOptions(t.Context(), dst.Pool, []verify.Table{table}, []verify.WrittenRecord{record}, verify.ScanOptions{})
		if written.Checked != 1 || written.Missing != 0 || len(written.Errors) != 0 {
			t.Fatalf("empty=%v written=%+v", empty, written)
		}
	}
}
