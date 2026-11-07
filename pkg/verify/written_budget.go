package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func writtenKey(t Table, text string) ([]byte, error) {
	parts := []string{text}
	if len(t.PK) > 1 {
		parts = nil
		scanner := pgtype.NewCompositeTextScanner(pgtype.NewMap(), []byte(text))
		for scanner.Next() {
			if scanner.Bytes() == nil {
				return nil, errors.New("null primary key")
			}
			parts = append(parts, string(scanner.Bytes()))
		}
		if scanner.Err() != nil {
			return nil, errors.New("invalid composite primary key")
		}
	}
	if len(parts) != len(t.PK) {
		return nil, errors.New("primary key field count mismatch")
	}
	key := map[string]string{}
	for i, name := range t.PK {
		key[name] = parts[i]
	}
	return json.Marshal(key)
}

// CheckWrittenWithOptions uses typed indexed lookups for both legacy PK-only
// records and final UPDATE/DELETE states. No target-wide PK map is constructed.
func CheckWrittenWithOptions(ctx context.Context, dst *pgxpool.Pool, tables []Table, recs []WrittenRecord, opt ScanOptions) WrittenResult {
	var res WrittenResult
	opt, err := opt.normalized()
	if err != nil {
		res.Errors = []string{err.Error()}
		return res
	}
	byName := map[string]Table{}
	for _, t := range tables {
		byName[t.String()] = t
		if _, ok := byName[t.Name]; !ok {
			byName[t.Name] = t
		}
	}
	for i, r := range recs {
		if i%opt.BatchSize == 0 {
			if opt.BeforeBatch != nil {
				if err := opt.BeforeBatch(ctx); err != nil {
					res.Errors = append(res.Errors, "written verification load budget unavailable")
					return res
				}
			}
			if i > 0 && opt.BatchPause > 0 {
				timer := time.NewTimer(opt.BatchPause)
				select {
				case <-ctx.Done():
					timer.Stop()
					res.Errors = append(res.Errors, "written verification cancelled")
					return res
				case <-timer.C:
				}
			}
		}
		t, ok := byName[r.Table]
		if !ok || len(t.PK) == 0 {
			res.Errors = append(res.Errors, r.Table+": unknown table or no primary key")
			res.Missing++
			continue
		}
		key, err := writtenKey(t, r.PK)
		if err != nil {
			res.Errors = append(res.Errors, t.String()+": invalid primary key")
			res.Missing++
			continue
		}
		conditions := make([]string, len(t.PK))
		for i, name := range t.PK {
			col := pgx.Identifier{name}.Sanitize()
			conditions[i] = "t." + col + " = k." + col
		}
		query := fmt.Sprintf("WITH base AS (SELECT b AS row_value FROM %s AS b LIMIT 1) SELECT count(*),coalesce(bool_and(to_jsonb(t)=$2::jsonb),false) FROM base CROSS JOIN LATERAL jsonb_populate_record(base.row_value,$1::jsonb) AS k JOIN %s AS t ON %s WHERE %s=$3::text", t.Ident(), t.Ident(), strings.Join(conditions, " AND "), pkExpr(t))
		expected := "null"
		if len(r.Expected) > 0 {
			expected = string(r.Expected)
		}
		one, cancel := queryContext(ctx, opt.QueryTimeout)
		var count int64
		var matches bool
		err = dst.QueryRow(one, query, string(key), expected, r.PK).Scan(&count, &matches)
		cancel()
		if err != nil {
			res.Errors = append(res.Errors, t.String()+": written verification query failed; remaining records unchecked")
			return res
		}
		res.Checked++
		missing := count != 1
		if r.Absent {
			missing = count != 0
		} else if len(r.Expected) > 0 {
			missing = count != 1 || !matches
		}
		if missing {
			res.Missing++
			if len(res.Samples) < maxSamples {
				res.Samples = append(res.Samples, t.String()+":"+r.PK)
			}
		}
	}
	return res
}
