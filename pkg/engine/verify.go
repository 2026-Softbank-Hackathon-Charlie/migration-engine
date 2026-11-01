package engine

import (
	"context"
	"fmt"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/files"
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
}

// Verify runs the post-cutover checks. It is also usable standalone (CLI `verify`).
// rc may be nil when files are not migrated.
func Verify(ctx context.Context, p *plan.Plan, src, dst *pg.DB, rc *files.Rclone) (*Report, error) {
	rep := &Report{}
	tables, err := verify.ListTables(ctx, src.Pool, p.Verify.Schemas, p.Verify.Tables)
	if err != nil {
		return rep, fmt.Errorf("list tables: %w", err)
	}
	rep.Tables, rep.MissingRows = verify.MissingRows(ctx, src.Pool, dst.Pool, tables)
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
			w := verify.CheckWritten(ctx, dst.Pool, tables, recs)
			rep.Written = &w
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
