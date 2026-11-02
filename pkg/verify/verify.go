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
// filter, when non-empty, keeps only "schema.table" or bare "table" names.
func ListTables(ctx context.Context, db *pgxpool.Pool, schemas, filter []string) ([]Table, error) {
	rows, err := db.Query(ctx, `
SELECT n.nspname::text, c.relname::text,
  coalesce((SELECT array_agg(a.attname::text ORDER BY k.ord)
    FROM pg_index i
    CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
    JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
    WHERE i.indrelid = c.oid AND i.indisprimary), '{}'::text[])
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p') AND NOT c.relispartition AND n.nspname = ANY($1)
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
	all := true
	out := make([]TableResult, 0, len(tables))
	for _, t := range tables {
		q := hashQuery(t)
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

func pkSet(ctx context.Context, db *pgxpool.Pool, t Table) (map[string]struct{}, error) {
	rows, err := db.Query(ctx, fmt.Sprintf("SELECT %s FROM %s AS t", pkExpr(t), t.Ident()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := map[string]struct{}{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		set[k] = struct{}{}
	}
	return set, rows.Err()
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
	var total int64
	out := make([]MissingResult, 0, len(tables))
	for _, t := range tables {
		r := MissingResult{Table: t.String()}
		if len(t.PK) == 0 {
			r.Skipped = "no primary key"
			out = append(out, r)
			continue
		}
		if err := missingRows(ctx, src, dst, t, &r); err != nil {
			r.Error = err.Error()
		}
		total += r.Missing
		out = append(out, r)
	}
	return out, total
}

func missingRows(ctx context.Context, src, dst *pgxpool.Pool, t Table, r *MissingResult) error {
	// Read the source first. After cutover the old source replicates the new
	// primary, so rows written to the target between two reads can show up on
	// the source; reading the target first would report them as missing. The
	// target only gains rows, so every key seen on the source earlier must be
	// on the target read afterwards.
	keys, err := pkList(ctx, src, t)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	have, err := pkSet(ctx, dst, t)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	for _, k := range keys {
		r.Checked++
		if _, ok := have[k]; !ok {
			r.Missing++
			if len(r.Samples) < maxSamples {
				r.Samples = append(r.Samples, k)
			}
		}
	}
	return nil
}

func pkList(ctx context.Context, db *pgxpool.Pool, t Table) ([]string, error) {
	rows, err := db.Query(ctx, fmt.Sprintf("SELECT %s FROM %s AS t", pkExpr(t), t.Ident()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// WrittenRecord is one line of the k6 success log.
type WrittenRecord struct {
	Table string `json:"table"` // "orders" or "public.orders"
	PK    string `json:"pk"`    // as text; composite keys as "(1,abc)"
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
		out = append(out, w)
	}
	return out, sc.Err()
}

// CheckWritten verifies every recorded successful write exists on the target.
func CheckWritten(ctx context.Context, dst *pgxpool.Pool, tables []Table, recs []WrittenRecord) WrittenResult {
	byName := map[string]Table{}
	for _, t := range tables {
		byName[t.String()] = t
		if _, dup := byName[t.Name]; !dup {
			byName[t.Name] = t
		}
	}
	group := map[string][]string{}
	for _, r := range recs {
		group[r.Table] = append(group[r.Table], r.PK)
	}
	var res WrittenResult
	for name, pks := range group {
		t, ok := byName[name]
		if !ok || len(t.PK) == 0 {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: unknown table or no primary key (%d records unchecked)", name, len(pks)))
			res.Missing += int64(len(pks))
			continue
		}
		have, err := pkSet(ctx, dst, t)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", name, err))
			res.Missing += int64(len(pks))
			continue
		}
		for _, pk := range pks {
			res.Checked++
			if _, ok := have[pk]; !ok {
				res.Missing++
				if len(res.Samples) < maxSamples {
					res.Samples = append(res.Samples, t.String()+":"+pk)
				}
			}
		}
	}
	return res
}
