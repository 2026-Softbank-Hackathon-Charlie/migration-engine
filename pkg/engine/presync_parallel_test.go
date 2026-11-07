package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/files"
)

type presyncDB struct {
	Database
	position func(context.Context) (string, error)
}

func (d presyncDB) WALPosition(ctx context.Context) (string, error) { return d.position(ctx) }

type presyncFiles struct {
	FileSyncer
	sync func(context.Context) (files.SyncResult, error)
}

func (f presyncFiles) Sync(ctx context.Context, _, _ string) (files.SyncResult, error) {
	return f.sync(ctx)
}

func TestPresyncOverlapsInitialCatchupAndFileTransfer(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")
	defer e.Close()
	dbStarted, filesStarted := make(chan struct{}), make(chan struct{})
	e.srcDB = presyncDB{Database: h.w.srcDB, position: func(ctx context.Context) (string, error) {
		close(dbStarted)
		select {
		case <-filesStarted:
			return h.w.srcDB.WALPosition(ctx)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}
	e.dstDB = h.w.dstDB
	e.rclone = presyncFiles{FileSyncer: h.w.files, sync: func(ctx context.Context) (files.SyncResult, error) {
		close(filesStarted)
		select {
		case <-dbStarted:
			return files.SyncResult{Output: "inspected"}, nil
		case <-ctx.Done():
			return files.SyncResult{}, ctx.Err()
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ev, err := e.presync(ctx)
	if err != nil {
		t.Fatalf("independent work was serialized: %v", err)
	}
	if ev["lagBytes"] != int64(0) || ev["filesSync"].(files.SyncResult).Output != "inspected" {
		t.Fatalf("lost evidence: %v", ev)
	}
	if e.State().WritesHeld || e.State().PromoteRequested {
		t.Fatal("presync changed the cutover boundary")
	}
}

func TestPresyncFileFailureCancelsAndJoinsCatchup(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")
	defer e.Close()
	started, exited := make(chan struct{}), make(chan struct{})
	e.srcDB = presyncDB{Database: h.w.srcDB, position: func(ctx context.Context) (string, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-ctx.Done()
		select {
		case <-exited:
		default:
			close(exited)
		}
		return "", ctx.Err()
	}}
	e.dstDB = h.w.dstDB
	want := errors.New("file policy denied")
	e.rclone = presyncFiles{FileSyncer: h.w.files, sync: func(ctx context.Context) (files.SyncResult, error) {
		select {
		case <-started:
			return files.SyncResult{}, want
		case <-ctx.Done():
			return files.SyncResult{}, ctx.Err()
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := e.presync(ctx)
	if !errors.Is(err, want) {
		t.Fatalf("file failure did not stop catchup: %v", err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("presync returned while catchup was still running")
	}
	if e.State().WritesHeld || e.State().PromoteRequested {
		t.Fatal("failed presync advanced migration")
	}
}

func TestPresyncCatchupFailureCancelsAndJoinsFileTransfer(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")
	defer e.Close()
	started, exited := make(chan struct{}), make(chan struct{})
	want := errors.New("database unavailable")
	e.connect = func(ctx context.Context, _ string) (Database, error) {
		select {
		case <-started:
			return nil, want
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	e.rclone = presyncFiles{FileSyncer: h.w.files, sync: func(ctx context.Context) (files.SyncResult, error) {
		close(started)
		<-ctx.Done()
		close(exited)
		return files.SyncResult{}, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := e.presync(ctx)
	if !errors.Is(err, want) {
		t.Fatalf("catchup failure lost: %v", err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("failed catchup left an active transfer")
	}
}

type prepareFailCluster struct{ Cluster }

func (c prepareFailCluster) WaitFor(context.Context, time.Duration, func(*cnpg.Status) bool) (*cnpg.Status, error) {
	return nil, errors.New("not ready")
}

func TestPrepareTimingSurvivesReadinessFailure(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")
	defer e.Close()
	e.dst = prepareFailCluster{Cluster: h.w.dst}
	ev, err := e.prepare(context.Background())
	if err == nil {
		t.Fatal("readiness failure passed")
	}
	timing, ok := ev["timingMillis"].(map[string]int64)
	if !ok {
		t.Fatal("failed prepare has no stage timing")
	}
	for _, name := range []string{"targetLookup", "targetHealthy", "total"} {
		if duration, ok := timing[name]; !ok || duration < 0 {
			t.Fatalf("missing phase %s: %v", name, timing)
		}
	}
	if _, ok := timing["targetRecovery"]; ok {
		t.Fatal("unexecuted database read reported as measured")
	}
}
