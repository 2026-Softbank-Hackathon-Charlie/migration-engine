package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ScanOptions bounds memory, query duration and verification duty cycle.
// BeforeBatch may wait for the current primary's load budget. It is never used
// by the write-fenced exact count/hash gate, where waiting would extend outage.
type ScanOptions struct {
	BatchSize    int
	BatchPause   time.Duration
	QueryTimeout time.Duration
	BeforeBatch  func(context.Context) error
}

func (o ScanOptions) normalized() (ScanOptions, error) {
	if o.BatchSize == 0 {
		o.BatchSize = 256
	}
	if o.QueryTimeout == 0 {
		o.QueryTimeout = 5 * time.Second
	}
	if o.BatchSize < 1 || o.BatchSize > 1000 || o.BatchPause < 0 || o.BatchPause > time.Second || o.QueryTimeout < 10*time.Millisecond || o.QueryTimeout > 30*time.Second {
		return o, errors.New("invalid verification scan budget")
	}
	return o, nil
}

// MissingRowsWithOptions preserves source-before-target ordering without an
// all-keys source slice or all-keys target map. A read-only cursor fetches a
// bounded batch; native typed PK equality allows target index lookups.
func MissingRowsWithOptions(ctx context.Context, src, dst *pgxpool.Pool, tables []Table, opt ScanOptions) ([]MissingResult, int64) {
	var total int64
	out := make([]MissingResult, 0, len(tables))
	options, err := opt.normalized()
	for _, t := range tables {
		r := MissingResult{Table: t.String()}
		if err != nil {
			r.Error = err.Error()
		} else if len(t.PK) == 0 {
			r.Skipped = "no primary key"
		} else if scanErr := missingBatches(ctx, src, dst, t, &r, options); scanErr != nil {
			r.Error = scanErr.Error()
		}
		total += r.Missing
		out = append(out, r)
	}
	return out, total
}

func queryContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, timeout)
}

func missingBatches(ctx context.Context, src, dst *pgxpool.Pool, t Table, r *MissingResult, opt ScanOptions) error {
	// A cursor retains a source snapshot. Bound its lifetime even if a caller
	// supplied no deadline, so admission waits cannot pin vacuum indefinitely.
	ctx, cancelAll := context.WithTimeout(ctx, 30*time.Minute)
	defer cancelAll()
	if opt.BeforeBatch != nil {
		if err := opt.BeforeBatch(ctx); err != nil {
			return err
		}
	}
	one, cancel := queryContext(ctx, opt.QueryTimeout)
	tx, err := src.BeginTx(one, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	cancel()
	if err != nil {
		return errors.New("source verification transaction unavailable")
	}
	defer func() {
		rollback, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollback)
	}()
	fields := make([]string, 0, len(t.PK)*2)
	args := make([]any, 0, len(t.PK))
	join := make([]string, 0, len(t.PK))
	for i, key := range t.PK {
		quoted := pgx.Identifier{key}.Sanitize()
		fields = append(fields, fmt.Sprintf("$%d::text", i+1), "t."+quoted)
		args = append(args, key)
		join = append(join, "t."+quoted+" = k."+quoted)
	}
	declare := fmt.Sprintf("DECLARE hopcharlie_verify_keys NO SCROLL CURSOR FOR SELECT %s, jsonb_build_object(%s) FROM %s AS t", pkExpr(t), strings.Join(fields, ","), t.Ident())
	one, cancel = queryContext(ctx, opt.QueryTimeout)
	_, err = tx.Exec(one, declare, args...)
	cancel()
	if err != nil {
		return errors.New("source verification cursor unavailable")
	}
	lookup := fmt.Sprintf("WITH base AS (SELECT b AS row_value FROM %s AS b LIMIT 1) SELECT %s FROM base CROSS JOIN LATERAL jsonb_populate_recordset(base.row_value,$1::jsonb) AS k JOIN %s AS t ON %s", t.Ident(), pkExpr(t), t.Ident(), strings.Join(join, " AND "))
	for {
		if opt.BeforeBatch != nil {
			if err := opt.BeforeBatch(ctx); err != nil {
				return err
			}
		}
		one, cancel = queryContext(ctx, opt.QueryTimeout)
		rows, err := tx.Query(one, fmt.Sprintf("FETCH FORWARD %d FROM hopcharlie_verify_keys", opt.BatchSize))
		if err != nil {
			cancel()
			return errors.New("source verification batch failed")
		}
		keys := make([]string, 0, opt.BatchSize)
		records := make([]json.RawMessage, 0, opt.BatchSize)
		batchBytes := 0
		for rows.Next() {
			var key string
			var raw json.RawMessage
			if err = rows.Scan(&key, &raw); err != nil {
				break
			}
			batchBytes += len(key) + len(raw)
			if batchBytes > 8<<20 {
				err = errors.New("verification key batch exceeds 8 MiB")
				break
			}
			keys = append(keys, key)
			records = append(records, raw)
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
		cancel()
		if err != nil {
			return errors.New("source verification batch incomplete")
		}
		if len(keys) == 0 {
			return nil
		}
		payload, err := json.Marshal(records)
		if err != nil {
			return errors.New("verification key encoding failed")
		}
		one, cancel = queryContext(ctx, opt.QueryTimeout)
		target, err := dst.Query(one, lookup, string(payload))
		if err != nil {
			cancel()
			return errors.New("target verification batch failed")
		}
		have := make(map[string]bool, len(keys))
		wanted := make(map[string]bool, len(keys))
		for _, key := range keys {
			wanted[key] = true
		}
		for target.Next() {
			var key string
			if err = target.Scan(&key); err != nil {
				break
			}
			if !wanted[key] {
				err = errors.New("target key does not match requested batch")
				break
			}
			have[key] = true
		}
		target.Close()
		if err == nil {
			err = target.Err()
		}
		cancel()
		if err != nil {
			return errors.New("target verification batch incomplete")
		}
		for _, key := range keys {
			r.Checked++
			if !have[key] {
				r.Missing++
				if len(r.Samples) < maxSamples {
					r.Samples = append(r.Samples, key)
				}
			}
		}
		if opt.BatchPause > 0 {
			timer := time.NewTimer(opt.BatchPause)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}
