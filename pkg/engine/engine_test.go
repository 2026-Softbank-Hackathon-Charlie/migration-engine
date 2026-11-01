package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func contains(calls []string, want string) bool { return slices.Contains(calls, want) }

func indexOf(t *testing.T, calls []string, want string) int {
	t.Helper()
	i := slices.Index(calls, want)
	if i < 0 {
		t.Fatalf("call %q not made; calls: %v", want, calls)
	}
	return i
}

func assertOrder(t *testing.T, calls []string, seq ...string) {
	t.Helper()
	prev := -1
	for _, s := range seq {
		i := indexOf(t, calls, s)
		if i <= prev {
			t.Fatalf("%q happened out of order; calls: %v", s, calls)
		}
		prev = i
	}
}

func TestRunHappyPath(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := e.State()
	if st.Outcome != OutcomeSucceeded {
		t.Fatalf("outcome = %s", st.Outcome)
	}
	for _, name := range Steps {
		if st.Steps[name].Status != StatusSucceeded {
			t.Errorf("step %s = %s", name, st.Steps[name].Status)
		}
	}
	calls := h.w.Calls()
	assertOrder(t, calls,
		"gw.hold",
		"verify.gate",
		`src.setPrimary db-eks token=""`,
		`dst.setPrimary db-eks token="tok-1000"`,
		"gw.route eks",
		"gw.release",
		"verify.report",
	)
	if contains(calls, "gw.route gke") {
		t.Error("traffic was routed back to source on a successful run")
	}
	if st.WritesHeld || !st.TrafficOnTarget || !st.PromoteRequested {
		t.Errorf("flags: %+v", st)
	}
	if h.w.dstDB.recovery {
		t.Error("target should be writable")
	}
	if len(h.kinds(EventDone)) != 1 {
		t.Error("expected one done event")
	}
}

func TestApplyManifestWhenTargetMissing(t *testing.T) {
	h := newHarness(t)
	h.w.dst.exists = false
	if err := h.engine(t, "").Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !contains(h.w.Calls(), "dst.apply target.yaml") {
		t.Fatalf("manifest not applied: %v", h.w.Calls())
	}
}

// The rollback boundary: what a failure at each point may undo.
func TestFailureOutcomes(t *testing.T) {
	cases := []struct {
		name        string
		fault       func(w *world)
		failStep    string
		outcome     string
		mustCall    []string
		mustNotCall []string
	}{
		{
			name:        "precheck: source unhealthy -> failed, nothing touched",
			fault:       func(w *world) { w.src.st.ReadyInstances = 1 },
			failStep:    StepPrecheck,
			outcome:     OutcomeFailed,
			mustNotCall: []string{"gw.hold", "gw.release", `src.setPrimary db-eks token=""`},
		},
		{
			name:        "precheck: topology not wired back -> failed",
			fault:       func(w *world) { w.src.st.ReplicaSource = "" },
			failStep:    StepPrecheck,
			outcome:     OutcomeFailed,
			mustNotCall: []string{"gw.hold"},
		},
		{
			name:        "fence: gateway cannot hold -> failed, nothing to undo",
			fault:       func(w *world) { w.gw.holdErr = errors.New("gateway down") },
			failStep:    StepFence,
			outcome:     OutcomeFailed,
			mustNotCall: []string{"gw.release", "gw.route gke"},
		},
		// "fence: last file sync fails -> rolled back" needs earlier syncs to pass: TestFileSyncFailsInsideFence.
		{
			name:        "switchover: fence gate mismatch -> rolled back before demotion",
			fault:       func(w *world) { w.ver.gateOK = false },
			failStep:    StepSwitchover,
			outcome:     OutcomeRolledBack,
			mustCall:    []string{"gw.route gke", "gw.release"},
			mustNotCall: []string{`src.setPrimary db-eks token=""`, `dst.setPrimary db-eks token="tok-1000"`},
		},
		{
			name:        "switchover: no demotion token -> source re-promoted, rolled back",
			fault:       func(w *world) { w.noDemotionToken = true },
			failStep:    StepSwitchover,
			outcome:     OutcomeRolledBack,
			mustCall:    []string{`src.setPrimary db-eks token=""`, `src.setPrimary db-gke token=""`, "gw.route gke", "gw.release"},
			mustNotCall: []string{`dst.setPrimary db-eks token="tok-1000"`},
		},
		{
			name:        "switchover: target never writable after promotion -> halted, no rollback",
			fault:       func(w *world) { w.targetNeverWakes = true },
			failStep:    StepSwitchover,
			outcome:     OutcomeHalted,
			mustCall:    []string{`dst.setPrimary db-eks token="tok-1000"`},
			mustNotCall: []string{"gw.route gke", "gw.release", `src.setPrimary db-gke token=""`},
		},
		{
			name:        "cutover: route fails -> halted",
			fault:       func(w *world) { w.gw.routeErr = errors.New("503") },
			failStep:    StepCutover,
			outcome:     OutcomeHalted,
			mustNotCall: []string{"gw.release", `src.setPrimary db-gke token=""`},
		},
		{
			name:        "verify: missing rows -> halted (target already serving)",
			fault:       func(w *world) { w.ver.reportOK = false },
			failStep:    StepVerify,
			outcome:     OutcomeHalted,
			mustCall:    []string{"gw.route eks", "gw.release"},
			mustNotCall: []string{"gw.route gke"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.fault(h.w)
			e := h.engine(t, "")
			err := e.Run(context.Background())
			if err == nil {
				t.Fatal("expected failure")
			}
			st := e.State()
			if st.Outcome != tc.outcome {
				t.Fatalf("outcome = %s, want %s (err: %v)", st.Outcome, tc.outcome, err)
			}
			if st.FailureStep != tc.failStep || st.Steps[tc.failStep].Status != StatusFailed {
				t.Fatalf("failure step = %s (%s), want %s", st.FailureStep, st.Steps[tc.failStep].Status, tc.failStep)
			}
			calls := h.w.Calls()
			for _, c := range tc.mustCall {
				indexOf(t, calls, c)
			}
			for _, c := range tc.mustNotCall {
				if contains(calls, c) {
					t.Errorf("must not call %q; calls: %v", c, calls)
				}
			}
			if tc.outcome == OutcomeRolledBack {
				if st.WritesHeld || st.SourceDemoted {
					t.Errorf("rollback left flags set: held=%v demoted=%v", st.WritesHeld, st.SourceDemoted)
				}
				if h.w.srcDB.recovery {
					t.Error("source is not writable after rollback")
				}
				if len(h.kinds(EventRollback)) == 0 {
					t.Error("no rollback event")
				}
			}
			if tc.outcome == OutcomeHalted && len(h.kinds(EventHalt)) == 0 {
				t.Error("no halt event")
			}
		})
	}
}

func TestFileSyncFailsInsideFence(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")
	ctx := context.Background()
	for _, s := range []string{StepPrecheck, StepPrepare, StepPresync, StepDelta} {
		if err := e.RunStep(ctx, s); err != nil {
			t.Fatal(s, err)
		}
	}
	h.w.files.syncErr = errors.New("bucket unreachable")
	if err := e.RunStep(ctx, StepFence); err == nil {
		t.Fatal("expected failure")
	}
	st := e.State()
	if st.Outcome != OutcomeRolledBack {
		t.Fatalf("outcome = %s", st.Outcome)
	}
	calls := h.w.Calls()
	assertOrder(t, calls, "gw.hold", "gw.route gke", "gw.release")
	if st.DowntimeMillis < 0 || st.WritesHeld {
		t.Errorf("state after rollback: %+v", st)
	}
}

func TestRollbackFailureHalts(t *testing.T) {
	h := newHarness(t)
	h.w.ver.gateOK = false
	h.w.gw.releaseErr = errors.New("gateway lost")
	e := h.engine(t, "")
	err := e.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("err = %v", err)
	}
	st := e.State()
	if st.Outcome != OutcomeHalted || st.RollbackError == "" {
		t.Fatalf("outcome=%s rollbackError=%q", st.Outcome, st.RollbackError)
	}
}

func TestDowntimeOverBudgetIsRecordedNotFailed(t *testing.T) {
	h := newHarness(t)
	h.p.Thresholds.MaxDowntime.Duration = time.Nanosecond
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	ev := e.State().Steps[StepCutover].Evidence
	if ev["withinBudget"] != false {
		t.Fatalf("withinBudget = %v", ev["withinBudget"])
	}
}

func TestResumeSkipsSucceededSteps(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first := h.engine(t, "")
	for _, s := range []string{StepPrecheck, StepPrepare, StepPresync} {
		if err := first.RunStep(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	runID := first.RunID()
	srcGets := h.w.src.gets

	// A new process resumes from the saved state file.
	second := h.engine(t, runID)
	if err := second.Run(ctx); err != nil {
		t.Fatal(err)
	}
	skipped := h.kinds(EventStepSkipped)
	if len(skipped) != 3 {
		t.Fatalf("skipped %d steps, want 3", len(skipped))
	}
	if second.State().Outcome != OutcomeSucceeded {
		t.Fatal(second.State().Outcome)
	}
	if h.w.src.gets == srcGets {
		t.Error("later steps did not run")
	}
}

func TestStateFileRoundTrip(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(e.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"outcome": "succeeded"`) {
		t.Fatalf("state file:\n%s", b)
	}
	if filepath.Dir(e.StatePath()) != h.dir {
		t.Fatalf("state path %s not in %s", e.StatePath(), h.dir)
	}
	if _, err := os.Stat(e.StatePath() + ".tmp"); !os.IsNotExist(err) {
		t.Error("temporary state file left behind")
	}
}

func TestRunGuards(t *testing.T) {
	ctx := context.Background()

	t.Run("step order enforced", func(t *testing.T) {
		h := newHarness(t)
		e := h.engine(t, "")
		err := e.RunStep(ctx, StepFence)
		if err == nil || !strings.Contains(err.Error(), "requires") {
			t.Fatalf("err = %v", err)
		}
		if contains(h.w.Calls(), "gw.hold") {
			t.Fatal("fence ran out of order")
		}
	})

	t.Run("unknown step", func(t *testing.T) {
		h := newHarness(t)
		if err := h.engine(t, "").RunStep(ctx, "teleport"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("halted run cannot be resumed", func(t *testing.T) {
		h := newHarness(t)
		h.w.targetNeverWakes = true
		e := h.engine(t, "")
		_ = e.Run(ctx)
		again := h.engine(t, e.RunID())
		if err := again.Run(ctx); err == nil || !strings.Contains(err.Error(), "start a new run") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("failed run can be retried after the fault is fixed", func(t *testing.T) {
		h := newHarness(t)
		h.w.src.st.ReadyInstances = 1
		e := h.engine(t, "")
		if err := e.Run(ctx); err == nil {
			t.Fatal("expected failure")
		}
		h.w.src.st.ReadyInstances = 2
		if err := h.engine(t, e.RunID()).Run(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("run id from another plan is rejected", func(t *testing.T) {
		h := newHarness(t)
		e := h.engine(t, "")
		h.p.ID = "other-plan"
		_, err := New(h.p, Options{StateDir: h.dir, RunID: e.RunID(), SourceCluster: h.w.src, TargetCluster: h.w.dst})
		if err == nil || !strings.Contains(err.Error(), "belongs to plan") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("unknown run id", func(t *testing.T) {
		h := newHarness(t)
		_, err := New(h.p, Options{StateDir: h.dir, RunID: "nope", SourceCluster: h.w.src, TargetCluster: h.w.dst})
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLagWaitTimesOut(t *testing.T) {
	h := newHarness(t)
	h.w.srcDB.pos = 1 << 30 // target far behind
	h.p.Thresholds.CatchupTimeout.Duration = 50 * time.Millisecond
	e := h.engine(t, "")
	err := e.Run(context.Background())
	if err == nil || e.State().FailureStep != StepPresync {
		t.Fatalf("err=%v step=%s", err, e.State().FailureStep)
	}
	if e.State().Outcome != OutcomeFailed {
		t.Fatalf("outcome = %s", e.State().Outcome)
	}
	if len(h.kinds(EventMetric)) == 0 {
		t.Error("no lag metrics emitted")
	}
}
