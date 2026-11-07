package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQueries(t *testing.T) {
	single := Table{Schema: "public", Name: "orders", PK: []string{"id"}}
	if got := pkExpr(single); got != `t."id"::text` {
		t.Errorf("pkExpr single = %s", got)
	}
	comp := Table{Schema: "public", Name: "order_items", PK: []string{"order_id", "line"}}
	if got := pkExpr(comp); got != `ROW(t."order_id", t."line")::text` {
		t.Errorf("pkExpr composite = %s", got)
	}
	if q := hashQuery(comp); !strings.Contains(q, `ORDER BY t."order_id", t."line"`) || !strings.Contains(q, `"public"."order_items"`) {
		t.Errorf("hashQuery = %s", q)
	}
	if q := hashQuery(Table{Schema: "public", Name: "log"}); !strings.Contains(q, "ORDER BY t::text") {
		t.Errorf("hashQuery no pk = %s", q)
	}
}

func TestReadWritten(t *testing.T) {
	p := filepath.Join(t.TempDir(), "w.jsonl")
	os.WriteFile(p, []byte("{\"table\":\"orders\",\"pk\":\"1\"}\n\n{\"table\":\"public.orders\",\"pk\":\"2\"}\n"), 0o644)
	recs, err := ReadWritten(p)
	if err != nil || len(recs) != 2 {
		t.Fatalf("recs=%v err=%v", recs, err)
	}
	os.WriteFile(p, []byte("{\"table\":\"orders\"}\n"), 0o644)
	if _, err := ReadWritten(p); err == nil {
		t.Fatal("expected error for missing pk")
	}
}

func TestBytewiseHashQuery(t *testing.T) {
	comp := Table{Schema: "public", Name: "order_items", PK: []string{"order_id", "sku"}}
	if q := hashQueryBytewise(comp); !strings.Contains(q, `ORDER BY t."order_id"::text COLLATE "C", t."sku"::text COLLATE "C"`) {
		t.Errorf("hashQueryBytewise = %s", q)
	}
	if q := hashQueryBytewise(Table{Schema: "public", Name: "log"}); !strings.Contains(q, `ORDER BY t::text COLLATE "C"`) {
		t.Errorf("hashQueryBytewise no pk = %s", q)
	}
	// The CNPG path's query is unchanged.
	if q := hashQuery(comp); strings.Contains(q, "COLLATE") {
		t.Errorf("hashQuery = %s", q)
	}
}
