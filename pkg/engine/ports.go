package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/files"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/pg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/verify"
)

// The engine talks to the outside world only through these ports. Production
// wiring uses pkg/cnpg, pkg/pg, pkg/files and pkg/verify; tests plug in fakes.

// Cluster is one CNPG Cluster resource (*cnpg.Client).
type Cluster interface {
	Get(ctx context.Context) (*cnpg.Status, error)
	ApplyManifest(ctx context.Context, path string) error
	SetPrimary(ctx context.Context, primary, promotionToken string) error
	WaitFor(ctx context.Context, interval time.Duration, cond func(*cnpg.Status) bool) (*cnpg.Status, error)
}

// Database is a connection to one PostgreSQL primary or standby (*pg.DB).
type Database interface {
	InRecovery(ctx context.Context) (bool, error)
	PendingRestart(ctx context.Context) (bool, error)
	WALPosition(ctx context.Context) (string, error)
	LSNDiff(ctx context.Context, a, b string) (int64, error)
	Close()
}

// Connector opens a Database for a DSN.
type Connector func(ctx context.Context, dsn string) (Database, error)

// FileSyncer copies and checks object storage (*files.Rclone).
type FileSyncer interface {
	Available() error
	Sync(ctx context.Context, src, dst string) (files.SyncResult, error)
	Check(ctx context.Context, src, dst string) (files.CheckResult, error)
}

// Verifier compares source and target data.
type Verifier interface {
	// FenceGate compares both sides while they are frozen at the same point.
	FenceGate(ctx context.Context, src, dst Database) ([]verify.TableResult, bool, error)
	// Report runs the post-cutover checks.
	Report(ctx context.Context, src, dst Database) (*Report, error)
}

func connectPG(ctx context.Context, dsn string) (Database, error) {
	db, err := pg.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// sqlVerifier is the production Verifier; it needs *pg.DB connections.
type sqlVerifier struct {
	plan *plan.Plan
	rc   FileSyncer
}

func pools(src, dst Database) (*pg.DB, *pg.DB, error) {
	s, ok1 := src.(*pg.DB)
	d, ok2 := dst.(*pg.DB)
	if !ok1 || !ok2 {
		return nil, nil, errors.New("sql verifier requires *pg.DB connections")
	}
	return s, d, nil
}

func (v sqlVerifier) FenceGate(ctx context.Context, src, dst Database) ([]verify.TableResult, bool, error) {
	s, d, err := pools(src, dst)
	if err != nil {
		return nil, false, err
	}
	tables, err := verify.ListTables(ctx, s.Pool, v.plan.Verify.Schemas, v.plan.Verify.Tables)
	if err != nil {
		return nil, false, fmt.Errorf("list tables: %w", err)
	}
	res, ok := verify.CompareTables(ctx, s.Pool, d.Pool, tables)
	return res, ok, nil
}

func (v sqlVerifier) Report(ctx context.Context, src, dst Database) (*Report, error) {
	s, d, err := pools(src, dst)
	if err != nil {
		return nil, err
	}
	return Verify(ctx, v.plan, s, d, v.rc)
}

// measureLag measures how far target is behind source in bytes.
func measureLag(ctx context.Context, source, target Database) (n int64, srcLSN, dstLSN string, err error) {
	if srcLSN, err = source.WALPosition(ctx); err != nil {
		return 0, "", "", fmt.Errorf("source position: %w", err)
	}
	if dstLSN, err = target.WALPosition(ctx); err != nil {
		return 0, srcLSN, "", fmt.Errorf("target position: %w", err)
	}
	if n, err = source.LSNDiff(ctx, srcLSN, dstLSN); err != nil {
		return 0, srcLSN, dstLSN, err
	}
	return max(n, 0), srcLSN, dstLSN, nil
}
