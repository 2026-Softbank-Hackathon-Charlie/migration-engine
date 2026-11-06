//go:build integration

package verify_test

import (
	"context"
	"encoding/json"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/verify"
	"testing"
)

func TestAcknowledgedUpdateDeleteAndTransactionFinalState(t *testing.T) {
	_, dst := pair(t)
	ctx := context.Background()
	// One committed transaction contains an INSERT, UPDATE and DELETE.
	_, err := dst.Pool.Exec(ctx, `BEGIN;
 INSERT INTO it.orders(id, note) VALUES (501, 'committed insert');
 UPDATE it.orders SET note='acknowledged update' WHERE id=1;
 DELETE FROM it.orders WHERE id=2;
 COMMIT;`)
	if err != nil {
		t.Fatal(err)
	}
	var expected string
	if err := dst.Pool.QueryRow(ctx, `SELECT to_jsonb(t)::text FROM it.orders t WHERE id=1`).Scan(&expected); err != nil {
		t.Fatal(err)
	}
	tables, err := verify.ListTables(ctx, dst.Pool, []string{"it"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	records := []verify.WrittenRecord{
		{Table: "it.orders", PK: "501"},
		{Table: "it.orders", PK: "1", Expected: json.RawMessage(expected)},
		{Table: "it.orders", PK: "2", Absent: true},
	}
	r := verify.CheckWritten(ctx, dst.Pool, tables, records)
	if r.Checked != 3 || r.Missing != 0 || len(r.Errors) != 0 {
		t.Fatalf("%+v", r)
	}
	// Reverting the UPDATE and resurrecting the DELETE must both fail.
	_, err = dst.Pool.Exec(ctx, `UPDATE it.orders SET note='stale' WHERE id=1; INSERT INTO it.orders(id) VALUES(2)`)
	if err != nil {
		t.Fatal(err)
	}
	r = verify.CheckWritten(ctx, dst.Pool, tables, records)
	if r.Missing != 2 {
		t.Fatalf("stale state accepted: %+v", r)
	}
}
