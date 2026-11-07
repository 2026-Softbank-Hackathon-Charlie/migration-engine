package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

type fakeSourceWriteFencer struct {
	w                                                       *world
	precheckErr, applyErr, heldErr, observerErr, restoreErr error
	restoreHook                                             func()
}

func (f *fakeSourceWriteFencer) Precheck(context.Context, string) (cnpg.HBASnapshot, error) {
	f.w.record("sql-fence.precheck")
	return cnpg.HBASnapshot{UID: "source-uid", Hash: "original-hash", Absent: true}, f.precheckErr
}
func (f *fakeSourceWriteFencer) Apply(context.Context, cnpg.HBASnapshot, string) (int, error) {
	f.w.record("sql-fence.apply")
	if f.applyErr == nil {
		f.w.mu.Lock()
		f.w.srcDB.pos = 2000
		f.w.dstDB.pos = 2000
		f.w.mu.Unlock()
	}
	return 2, f.applyErr
}
func (f *fakeSourceWriteFencer) AssertHeld(context.Context, cnpg.HBASnapshot, string) error {
	f.w.record("sql-fence.held")
	return f.heldErr
}
func (f *fakeSourceWriteFencer) ObserverAlive(context.Context) error { return f.observerErr }
func (f *fakeSourceWriteFencer) ConfirmReadOnlySource(ctx context.Context, db Database, _ cnpg.HBASnapshot, _ string) (string, error) {
	f.w.record("sql-fence.readonly")
	recovery, err := db.InRecovery(ctx)
	if err != nil || !recovery {
		return "", errors.New("source not read-only")
	}
	f.w.mu.Lock()
	f.w.srcDB.pos = 3000
	f.w.dstDB.pos = 3000
	f.w.mu.Unlock()
	return db.WALPosition(ctx)
}
func (f *fakeSourceWriteFencer) Restore(_ context.Context, _ cnpg.HBASnapshot, _ string, primary bool) error {
	if primary {
		f.w.record("sql-fence.restore-primary")
	} else {
		f.w.record("sql-fence.restore-replica")
		if f.restoreHook != nil {
			f.restoreHook()
		}
	}
	return f.restoreErr
}

func TestSourceSQLFenceCleanupSaveFailureNeverRepromotesDemotedSource(t *testing.T) {
	h, e, f := fencedHarness(t)
	f.restoreHook = func() { e.statePath = t.TempDir() }
	if err := e.Run(context.Background()); err == nil {
		t.Fatal("failed cleanup persistence succeeded")
	}
	st := e.State()
	if st.Outcome != OutcomeHalted || !st.WritesHeld || !st.SourceDemoted || st.PromoteRequested || !st.SourceWriteFence.Released {
		t.Fatalf("unsafe compensation state: %+v", st)
	}
	for _, call := range h.w.Calls() {
		if strings.HasPrefix(call, "src.setPrimary db-gke") || strings.HasPrefix(call, "dst.setPrimary") || call == "gw.release" {
			t.Fatalf("post-demotion persistence failure triggered unsafe compensation: %s", call)
		}
	}
}

func (g *fakeGateway) SourceFenceReady(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (g *fakeGateway) SourceFenceSupported(context.Context) error { return nil }
func (g *fakeGateway) AcquireSourceReadFence(context.Context, string, string) error {
	g.w.record("gw.exclude-source-reads")
	return nil
}
func (g *fakeGateway) ReleaseSourceReadFence(context.Context, string, string) error {
	g.w.record("gw.release-source-reads")
	return nil
}

func fencedHarness(t *testing.T) (*harness, *Engine, *fakeSourceWriteFencer) {
	t.Helper()
	h := newHarness(t)
	h.p.Gateway.Mode, h.p.Gateway.URL = "http", "http://gateway.test"
	h.p.SourceWriteFence = &plan.SourceWriteFenceSpec{Mode: "cnpg_hba", WriterRoles: []string{"app"}, Timeout: plan.Duration{Duration: time.Second}}
	e := h.engine(t, "")
	f := &fakeSourceWriteFencer{w: h.w}
	e.writeFencer = f
	return h, e, f
}

func TestSourceSQLFenceOrdersDrainBeforeMarkerAndRestoresOnlyReplica(t *testing.T) {
	h, e, _ := fencedHarness(t)
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.State().FenceLSN != "3000" {
		t.Fatal("WAL marker was read before SQL clients drained")
	}
	assertOrder(t, h.w.Calls(), "gw.hold", "sql-fence.apply", "sql-fence.held", `src.setPrimary db-eks token=""`, "sql-fence.readonly", "verify.gate", "sql-fence.restore-replica", `dst.setPrimary db-eks token="tok-2000"`, "gw.route eks", "gw.release")
	if !e.State().SourceWriteFence.Released {
		t.Fatal("fence release proof not persisted")
	}
}

func TestSourceSQLFenceUnsupportedPrivilegeFailsBeforeHold(t *testing.T) {
	h, e, f := fencedHarness(t)
	f.precheckErr = errors.New("observer permission missing")
	if err := e.Run(context.Background()); err == nil {
		t.Fatal("unsupported fence executed")
	}
	if contains(h.w.Calls(), "gw.hold") || contains(h.w.Calls(), "sql-fence.apply") {
		t.Fatal("writers paused before capability check")
	}
}

func TestSourceSQLFenceBeforePromotionFailureRestoresBeforeRelease(t *testing.T) {
	h, e, f := fencedHarness(t)
	f.heldErr = errors.New("a transient source admission probe failed before demotion")
	if err := e.Run(context.Background()); err == nil {
		t.Fatal("bad gate succeeded")
	}
	if e.State().Outcome != OutcomeRolledBack {
		t.Fatal(e.State().Outcome)
	}
	assertOrder(t, h.w.Calls(), "sql-fence.apply", "sql-fence.restore-primary", "gw.route gke", "gw.release")
}

func TestSourceSQLFenceNeverReopensAfterObserverLossOrUnknownDemotion(t *testing.T) {
	for _, fault := range []string{"observer", "demote-request", "demotion", "promotion"} {
		t.Run(fault, func(t *testing.T) {
			h, e, f := fencedHarness(t)
			switch fault {
			case "observer":
				f.applyErr = errors.New("connection lost")
				f.observerErr = errors.New("old verifier lost")
			case "demotion":
				h.w.noDemotionToken = true
			case "demote-request":
				h.w.src.setErr = errors.New("source demotion request timed out before a conclusive acknowledgment")
			case "promotion":
				h.w.dst.setErr = errors.New("promote timeout")
			}
			if err := e.Run(context.Background()); err == nil {
				t.Fatal("uncertain state succeeded")
			}
			if e.State().Outcome != OutcomeHalted || !e.State().WritesHeld {
				t.Fatalf("unsafe state: %+v", e.State())
			}
			for _, call := range []string{"sql-fence.restore-primary", "gw.release"} {
				if contains(h.w.Calls(), call) {
					t.Fatalf("unsafe compensation %s", call)
				}
			}
			if fault != "promotion" && contains(h.w.Calls(), "sql-fence.restore-replica") {
				t.Fatal("read admission restored without confirmed source demotion")
			}
		})
	}
}

func TestSourceSQLFenceIntentMustSaveBeforeExternalPatch(t *testing.T) {
	h, e, _ := fencedHarness(t)
	if err := e.prepareSourceWriteFence(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.statePath = t.TempDir() // A directory cannot be replaced by a JSON state file.
	if _, err := e.acquireSourceWriteFence(context.Background()); err == nil {
		t.Fatal("unsaved intent accepted")
	}
	if contains(h.w.Calls(), "sql-fence.apply") {
		t.Fatal("external patch ran before intent persisted")
	}
}

func TestSourceSQLFenceProcessRestartRequiresReconciliation(t *testing.T) {
	h, e, _ := fencedHarness(t)
	runThrough(t, e, StepFence)
	if err := e.RunStep(context.Background(), StepFence); err != nil {
		t.Fatal(err)
	}
	e.Close()
	resumed := h.engine(t, e.RunID())
	if err := resumed.Run(context.Background()); err == nil || resumed.State().Outcome != OutcomeHalted {
		t.Fatal("persisted SQL fence replayed after process restart")
	}
}
