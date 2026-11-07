// Package verify proves that no data was lost in a migration.
//
// Three checks are provided:
//   - CompareTables: exact count + ordered hash per table. Used inside the write
//     fence, when source and target must be identical.
//   - MissingRows: every source primary key exists on the target. Used after
//     cutover, when the target may legitimately have newer rows.
//   - CheckWritten: every write k6 saw succeed exists on the target.
package verify

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Table is a user table and its primary key columns.
type Table struct {
	Schema string   `json:"schema"`
	Name   string   `json:"name"`
	PK     []string `json:"pk,omitempty"`
}

// String returns schema.name.
func (t Table) String() string { return t.Schema + "." + t.Name }

// Ident returns the quoted identifier.
func (t Table) Ident() string { return pgx.Identifier{t.Schema, t.Name}.Sanitize() }

// ListTables lists ordinary and partitioned (root) tables in schemas.
// Empty schemas means every user schema: all but the system schemas
// (pg_catalog, information_schema, pg_toast*, pg_temp*) and schemas an
// extension owns. filter, when non-empty, keeps only "schema.table" or bare
// "table" names.
func ListTables(ctx context.Context, db *pgxpool.Pool, schemas, filter []string) ([]Table, error) {
	rows, err := db.Query(ctx, `
SELECT n.nspname::text, c.relname::text,
  coalesce((SELECT array_agg(a.attname::text ORDER BY k.ord)
    FROM pg_index i
    CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
    JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
    WHERE i.indrelid = c.oid AND i.indisprimary), '{}'::text[])
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p') AND NOT c.relispartition AND CASE
  WHEN cardinality($1::text[]) > 0 THEN n.nspname = ANY($1)
  ELSE n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'
    AND NOT EXISTS (SELECT 1 FROM pg_depend d
      WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid
        AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e')
  END
ORDER BY 1, 2`, schemas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keep := map[string]bool{}
	for _, f := range filter {
		keep[f] = true
	}
	var out []Table
	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.Schema, &t.Name, &t.PK); err != nil {
			return nil, err
		}
		if len(keep) > 0 && !keep[t.String()] && !keep[t.Name] {
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TableResult is the comparison result of one table.
type TableResult struct {
	Table       string `json:"table"`
	SourceCount int64  `json:"sourceCount"`
	TargetCount int64  `json:"targetCount"`
	SourceHash  string `json:"sourceHash,omitempty"`
	TargetHash  string `json:"targetHash,omitempty"`
	Match       bool   `json:"match"`
	Error       string `json:"error,omitempty"`
}

// CompareTables computes count + ordered row hash on both sides, in parallel per table.
func CompareTables(ctx context.Context, src, dst *pgxpool.Pool, tables []Table) ([]TableResult, bool) {
	return compareTables(ctx, src, dst, tables, hashQuery)
}

// CompareTablesBytewise is CompareTables with the rows ordered by the bytes
// of their text, not by each side's collation (ADR 0019). An external source
// may sort text by a different C library than the target, and a text index
// the target copied from it can be corrupt until it is rebuilt; neither must
// change the hash of identical data.
func CompareTablesBytewise(ctx context.Context, src, dst *pgxpool.Pool, tables []Table) ([]TableResult, bool) {
	return compareTables(ctx, src, dst, tables, hashQueryBytewise)
}

func compareTables(ctx context.Context, src, dst *pgxpool.Pool, tables []Table, hash func(Table) string) ([]TableResult, bool) {
	all := true
	out := make([]TableResult, 0, len(tables))
	for _, t := range tables {
		q := hash(t)
		r := TableResult{Table: t.String()}
		var wg sync.WaitGroup
		var se, de error
		wg.Add(2)
		go func() { defer wg.Done(); se = src.QueryRow(ctx, q).Scan(&r.SourceCount, &r.SourceHash) }()
		go func() { defer wg.Done(); de = dst.QueryRow(ctx, q).Scan(&r.TargetCount, &r.TargetHash) }()
		wg.Wait()
		switch {
		case se != nil:
			r.Error = "source: " + se.Error()
		case de != nil:
			r.Error = "target: " + de.Error()
		default:
			r.Match = r.SourceCount == r.TargetCount && r.SourceHash == r.TargetHash
		}
		if !r.Match {
			all = false
		}
		out = append(out, r)
	}
	return out, all
}

func hashQuery(t Table) string {
	order := "t::text"
	if len(t.PK) > 0 {
		cols := make([]string, len(t.PK))
		for i, c := range t.PK {
			cols[i] = "t." + pgx.Identifier{c}.Sanitize()
		}
		order = strings.Join(cols, ", ")
	}
	return fmt.Sprintf(`SELECT count(*), coalesce(md5(string_agg(md5(t::text), '' ORDER BY %s)), '') FROM %s AS t`, order, t.Ident())
}

// hashQueryBytewise orders by each key's text under COLLATE "C". The cast
// makes it valid for every type (COLLATE on an integer is an error), and the
// session settings (pg.SessionSettings) make the text the same on both sides.
// The order differs from hashQuery's, so the hashes do too; only the two sides
// of one comparison have to agree.
func hashQueryBytewise(t Table) string {
	order := `t::text COLLATE "C"`
	if len(t.PK) > 0 {
		cols := make([]string, len(t.PK))
		for i, c := range t.PK {
			cols[i] = "t." + pgx.Identifier{c}.Sanitize() + `::text COLLATE "C"`
		}
		order = strings.Join(cols, ", ")
	}
	return fmt.Sprintf(`SELECT count(*), coalesce(md5(string_agg(md5(t::text), '' ORDER BY %s)), '') FROM %s AS t`, order, t.Ident())
}

// pkExpr renders the primary key as one text value: "42" or "(1,abc)".
func pkExpr(t Table) string {
	if len(t.PK) == 1 {
		return "t." + pgx.Identifier{t.PK[0]}.Sanitize() + "::text"
	}
	cols := make([]string, len(t.PK))
	for i, c := range t.PK {
		cols[i] = "t." + pgx.Identifier{c}.Sanitize()
	}
	return "ROW(" + strings.Join(cols, ", ") + ")::text"
}

// MissingResult is the source ⊆ target check of one table.
type MissingResult struct {
	Table   string   `json:"table"`
	Checked int64    `json:"checked"`
	Missing int64    `json:"missing"`
	Samples []string `json:"samples,omitempty"`
	Skipped string   `json:"skipped,omitempty"`
	Error   string   `json:"error,omitempty"`
}

const maxSamples = 20

// MissingRows checks that every source primary key exists on the target.
func MissingRows(ctx context.Context, src, dst *pgxpool.Pool, tables []Table) ([]MissingResult, int64) {
	return MissingRowsWithOptions(ctx, src, dst, tables, ScanOptions{})
}

// WrittenRecord is one line of the k6 success log.
type WrittenRecord struct {
	Table string `json:"table"` // "orders" or "public.orders"
	PK    string `json:"pk"`    // as text; composite keys as "(1,abc)"
	// Expected is the final row as to_jsonb(row)::text; absent proves a DELETE.
	// Supply only the last acknowledged state per key after writers stop.
	Expected json.RawMessage `json:"expected,omitempty"`
	Absent   bool            `json:"absent,omitempty"`
}

// WrittenResult is the k6 success-log check.
type WrittenResult struct {
	Checked int64    `json:"checked"`
	Missing int64    `json:"missing"`
	Samples []string `json:"samples,omitempty"`
	Errors  []string `json:"errors,omitempty"`
}

// ReadWritten parses a JSONL file of WrittenRecord.
func ReadWritten(path string) ([]WrittenRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []WrittenRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" {
			continue
		}
		var w WrittenRecord
		if err := json.Unmarshal([]byte(s), &w); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if w.Table == "" || w.PK == "" {
			return nil, fmt.Errorf("%s:%d: table and pk are required", path, line)
		}
		if w.Absent && len(w.Expected) != 0 {
			return nil, fmt.Errorf("%s:%d: absent and expected are mutually exclusive", path, line)
		}
		if len(w.Expected) != 0 && (!json.Valid(w.Expected) || strings.TrimSpace(string(w.Expected))[0] != '{') {
			return nil, fmt.Errorf("%s:%d: expected must be a JSON object", path, line)
		}
		out = append(out, w)
	}
	return out, sc.Err()
}

// CheckWritten verifies every recorded successful write exists on the target.
func CheckWritten(ctx context.Context, dst *pgxpool.Pool, tables []Table, recs []WrittenRecord) WrittenResult {
	return CheckWrittenWithOptions(ctx, dst, tables, recs, ScanOptions{})
}
