package engine

import (
	"context"
	"fmt"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/files"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/loadguard"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/pg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/verify"
)

// Report is the post-cutover verification evidence ("누락 0건" report).
type Report struct {
	OK          bool                   `json:"ok"`
	Problems    []string               `json:"problems,omitempty"`
	Tables      []verify.MissingResult `json:"tables"`
	MissingRows int64                  `json:"missingRows"`
	Written     *verify.WrittenResult  `json:"written,omitempty"`
	Files       *files.CheckResult     `json:"files,omitempty"`
	// This engine uses asynchronous cross-environment replication. A successful
	// comparison does not prove durability after losing the primary environment.
	Durability string `json:"durability"`
}

// Verify runs the post-cutover checks. It is also usable standalone (CLI `verify`).
// rc may be nil when files are not migrated.
func Verify(ctx context.Context, p *plan.Plan, src, dst *pg.DB, rc FileSyncer) (*Report, error) {
	var options verify.ScanOptions
	policy, err := loadguard.FromEnv()
	if err != nil {
		return nil, err
	}
	if policy != nil {
		guard, err := loadguard.New(policy, endpointScope(p.Target), nil)
		if err != nil {
			return nil, err
		}
		options = scanOptions(guard)
	}
	return verifyWithOptions(ctx, p, src, dst, rc, options)
}

func verifyWithOptions(ctx context.Context, p *plan.Plan, src, dst *pg.DB, rc FileSyncer, options verify.ScanOptions) (*Report, error) {
	if p.Source.External() {
		return verifyExternal(ctx, p, dst, rc, options)
	}
	rep := &Report{Durability: "asynchronous; planned return requires reachable primary and WAL; environment-loss RPO=0 unsupported"}
	tables, err := verify.ListTables(ctx, src.Pool, p.Verify.Schemas, p.Verify.Tables)
	if err != nil {
		return rep, fmt.Errorf("list tables: %w", err)
	}
	rep.Tables, rep.MissingRows = verify.MissingRowsWithOptions(ctx, src.Pool, dst.Pool, tables, options)
	if rep.MissingRows > 0 {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%d source rows missing on target", rep.MissingRows))
	}
	for _, t := range rep.Tables {
		if t.Error != "" {
			rep.Problems = append(rep.Problems, t.Table+": "+t.Error)
		}
	}

	if p.Verify.WrittenRecords != "" {
		recs, err := verify.ReadWritten(p.Verify.WrittenRecords)
		if err != nil {
			rep.Problems = append(rep.Problems, "written records: "+err.Error())
		} else {
			w := verify.CheckWrittenWithOptions(ctx, dst.Pool, tables, recs, options)
			rep.Written = &w
			if len(w.Errors) > 0 {
				rep.Problems = append(rep.Problems, "written verification incomplete or failed")
			}
			if w.Missing > 0 {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%d of %d k6-confirmed writes missing on target", w.Missing, w.Checked+int64(len(w.Errors))))
			}
		}
	}

	if rc != nil && p.Files != nil {
		c, err := rc.Check(ctx, p.Files.Source, p.Files.Target)
		rep.Files = &c
		switch {
		case err != nil:
			rep.Problems = append(rep.Problems, "files: "+err.Error())
		case !c.OK:
			rep.Problems = append(rep.Problems, fmt.Sprintf("files: %d differences, %d missing on target", c.Differences, c.Missing))
		}
	}
	rep.OK = len(rep.Problems) == 0
	return rep, nil
}

// verifyExternal is the post-cutover check of an external source (ADR 0019).
// The source stays frozen at the fence and does not follow the new primary,
// so its rows were compared once, by the fence gate before the promotion.
// This checks the k6 writes on the target and that every source object is on
// the target (rclone check --one-way).
func verifyExternal(ctx context.Context, p *plan.Plan, dst *pg.DB, rc FileSyncer, options verify.ScanOptions) (*Report, error) {
	rep := &Report{Durability: "external source frozen read-only at the fence; rows compared by the fence gate before promotion; the old source does not follow the new primary"}
	if p.Verify.WrittenRecords != "" {
		tables, err := verify.ListTables(ctx, dst.Pool, p.Verify.Schemas, p.Verify.Tables)
		if err != nil {
			return rep, fmt.Errorf("list tables: %w", err)
		}
		recs, err := verify.ReadWritten(p.Verify.WrittenRecords)
		if err != nil {
			rep.Problems = append(rep.Problems, "written records: "+err.Error())
		} else {
			w := verify.CheckWrittenWithOptions(ctx, dst.Pool, tables, recs, options)
			rep.Written = &w
			if len(w.Errors) > 0 {
				rep.Problems = append(rep.Problems, "written verification incomplete or failed")
			}
			if w.Missing > 0 {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%d of %d k6-confirmed writes missing on target", w.Missing, w.Checked+int64(len(w.Errors))))
			}
		}
	}
	if rc != nil && p.Files != nil {
		c, err := rc.Check(ctx, p.Files.Source, p.Files.Target)
		rep.Files = &c
		switch {
		case err != nil:
			rep.Problems = append(rep.Problems, "files: "+err.Error())
		case !c.OK:
			rep.Problems = append(rep.Problems, fmt.Sprintf("files: %d differences, %d missing on target", c.Differences, c.Missing))
		}
	}
	rep.OK = len(rep.Problems) == 0
	return rep, nil
}
