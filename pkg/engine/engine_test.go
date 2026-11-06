package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
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
			// Re-promoting while CNPG is still demoting leaves the source fenced (seen on a real cluster).
			name:        "switchover: demotion never finishes -> halted, source not touched mid-transition",
			fault:       func(w *world) { w.noDemotionToken = true },
			failStep:    StepSwitchover,
			outcome:     OutcomeHalted,
			mustCall:    []string{`src.setPrimary db-eks token=""`},
			mustNotCall: []string{"src.setPrimary db-gke", "gw.release", `dst.setPrimary db-eks token="tok-1000"`},
		},
		{
			name:        "switchover: demotion finishes after the timeout -> waits for it, then rolled back",
			fault:       func(w *world) { w.tokenAfter = 1200 * time.Millisecond },
			failStep:    StepSwitchover,
			outcome:     OutcomeRolledBack,
			mustCall:    []string{`src.setPrimary db-eks token=""`, `src.setPrimary db-gke token="tok-1000"`, "gw.route gke", "gw.release"},
			mustNotCall: []string{`dst.setPrimary db-eks token="tok-1000"`},
		},
		{
			name:        "switchover: token left from an earlier demotion is not used to promote",
			fault:       func(w *world) { w.src.st.DemotionToken = "tok-old"; w.noDemotionToken = true },
			failStep:    StepSwitchover,
			outcome:     OutcomeHalted,
			mustNotCall: []string{`dst.setPrimary db-eks token="tok-old"`, "src.setPrimary db-gke"},
		},
		{
			name:        "switchover: target never writable after promotion -> halted, no rollback",
			fault:       func(w *world) { w.targetNeverWakes = true },
			failStep:    StepSwitchover,
			outcome:     OutcomeHalted,
			mustCall:    []string{`dst.setPrimary db-eks token="tok-1000"`},
			mustNotCall: []string{"gw.route gke", "gw.release", "src.setPrimary db-gke"},
		},
		{
			name:        "cutover: route fails -> halted",
			fault:       func(w *world) { w.gw.routeErr = errors.New("503") },
			failStep:    StepCutover,
			outcome:     OutcomeHalted,
			mustNotCall: []string{"gw.release", "src.setPrimary db-gke"},
		},
		// --- precheck: every refusal leaves both clusters and the gateway untouched ---
		{
			name:        "precheck: source is not the topology primary -> failed",
			fault:       func(w *world) { w.src.st.ReplicaPrimary = "db-eks" },
			failStep:    StepPrecheck,
			outcome:     OutcomeFailed,
			mustNotCall: []string{"gw.hold", "dst.apply", "src.setPrimary"},
		},
		{
			name:        "precheck: source database in recovery -> failed",
			fault:       func(w *world) { w.srcDB.recovery = true },
			failStep:    StepPrecheck,
			outcome:     OutcomeFailed,
			mustNotCall: []string{"gw.hold", "src.setPrimary"},
		},
		{
			name:        "precheck: database unreachable -> failed",
			fault:       func(w *world) { w.connectErr = errors.New("dial tcp: i/o timeout") },
			failStep:    StepPrecheck,
			outcome:     OutcomeFailed,
			mustNotCall: []string{"gw.hold", "src.setPrimary"},
		},
		{
			name:        "precheck: gateway unhealthy -> failed",
			fault:       func(w *world) { w.gw.healthErr = errors.New("503") },
			failStep:    StepPrecheck,
			outcome:     OutcomeFailed,
			mustNotCall: []string{"gw.hold", "src.setPrimary"},
		},
		{
			name:        "precheck: source and target share replica.self -> failed",
			fault:       func(w *world) { w.dst.st.Self = "db-gke" },
			failStep:    StepPrecheck,
			outcome:     OutcomeFailed,
			mustNotCall: []string{"gw.hold"},
		},
		// --- prepare: the target must become a healthy, streaming replica ---
		{
			name:        "prepare: manifest apply refused -> failed",
			fault:       func(w *world) { w.dst.exists = false; w.dst.applyErr = errors.New("admission webhook denied") },
			failStep:    StepPrepare,
			outcome:     OutcomeFailed,
			mustNotCall: []string{"gw.hold"},
		},
		{
			name:        "prepare: target never healthy -> failed after catchupTimeout",
			fault:       func(w *world) { w.dst.st.Phase = "Setting up primary" },
			failStep:    StepPrepare,
			outcome:     OutcomeFailed,
			mustNotCall: []string{"gw.hold"},
		},
		{
			// A writable target would be a second primary: never migrate into it.
			name:        "prepare: target is writable (not a replica) -> failed",
			fault:       func(w *world) { w.dstDB.recovery = false },
			failStep:    StepPrepare,
			outcome:     OutcomeFailed,
			mustNotCall: []string{"gw.hold", "src.setPrimary"},
		},
		// --- presync/delta: lag must come down before writes are held ---
		{
			name:        "presync: target lag never comes down -> failed, writes never held",
			fault:       func(w *world) { w.dstDB.pos = 1000 - 64<<20 },
			failStep:    StepPresync,
			outcome:     OutcomeFailed,
			mustNotCall: []string{"gw.hold"},
		},
		// --- switchover ---
		{
			name:        "switchover: target never replays the fence LSN -> rolled back before demotion",
			fault:       func(w *world) { w.dstDB.pos = 10 }, // within maxLagBytes, but behind the fence
			failStep:    StepSwitchover,
			outcome:     OutcomeRolledBack,
			mustCall:    []string{"gw.route gke", "gw.release"},
			mustNotCall: []string{"src.setPrimary", "dst.setPrimary"},
		},
		{
			// After the promotion request the target may already be primary: no automatic rollback.
			name:        "switchover: promotion request fails -> halted (split-brain guard)",
			fault:       func(w *world) { w.dst.setErr = errors.New("conflict") },
			failStep:    StepSwitchover,
			outcome:     OutcomeHalted,
			mustCall:    []string{`dst.setPrimary db-eks token="tok-1000"`},
			mustNotCall: []string{"src.setPrimary db-gke", "gw.route", "gw.release"},
		},
		{
			name:        "switchover: promoted target never settles -> halted, writes stay held",
			fault:       func(w *world) { w.restartGets = 1 << 30 },
			failStep:    StepSwitchover,
			outcome:     OutcomeHalted,
			mustNotCall: []string{"gw.route", "gw.release", "src.setPrimary db-gke"},
		},
		// --- cutover ---
		{
			name:        "cutover: release fails after the route -> halted, traffic on target",
			fault:       func(w *world) { w.gw.releaseErr = errors.New("gateway lost") },
			failStep:    StepCutover,
			outcome:     OutcomeHalted,
			mustCall:    []string{"gw.route eks", "gw.release"},
			mustNotCall: []string{"gw.route gke", "src.setPrimary db-gke"},
		},
		// --- verify ---
		{
			name:        "verify: old source never comes back as a replica -> halted, traffic stays on target",
			fault:       func(w *world) { w.sourceDownCalls = 1 << 30 },
			failStep:    StepVerify,
			outcome:     OutcomeHalted,
			mustCall:    []string{"gw.route eks", "gw.release"},
			mustNotCall: []string{"gw.route gke"},
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
				if slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, c) }) {
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

// The production run of 2026-11-05 halted although the target had promoted:
// the engine's link to it dropped every few seconds, and every failed read
// reset the settle count. A failed read is not a restart; the server start
// time of the next read says whether one happened (ADR 0013).
func TestSettleToleratesDroppedConnections(t *testing.T) {
	h := newHarness(t)
	h.w.flakyConnects = true // every other connection fails from promotion to cutover
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v (outcome %s)", err, e.State().Outcome)
	}
	rep, ok := e.State().Steps[StepSwitchover].Evidence["settle"].(SettleReport)
	if !ok || rep.Unobserved == 0 || rep.Restarts != 0 {
		t.Fatalf("settle evidence %+v", e.State().Steps[StepSwitchover].Evidence["settle"])
	}
	timing, _ := e.State().Steps[StepSwitchover].Evidence["timingMillis"].(map[string]int64)
	for _, k := range []string{"replayFence", "fenceGate", "demote", "promoteToWritable", "settle"} {
		if _, ok := timing[k]; !ok {
			t.Errorf("timing lacks %s: %v", k, timing)
		}
	}
}

// A restart that shows nothing but a new server start time still restarts
// the settle count.
func TestSettleSeesASilentRestart(t *testing.T) {
	h := newHarness(t)
	h.w.dstDB.silentRestartAt = 2
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rep := e.State().Steps[StepSwitchover].Evidence["settle"].(SettleReport); rep.Restarts != 1 {
		t.Fatalf("restarts %d, want 1 (%+v)", rep.Restarts, rep)
	}
}

func TestSettleRequiresFreshObservationAfterDroppedConnections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.w.dstDB.recovery = false
		e := h.engine(t, "")
		connects := 0
		e.connect = func(ctx context.Context, dsn string) (Database, error) {
			connects++
			if connects > 2 {
				return nil, errors.New("connection reset by peer")
			}
			return h.w.connect(ctx, dsn)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*settleMin)
		defer cancel()
		rep, err := e.waitSettled(ctx, e.dst, h.p.Target.DSN, &e.dstDB)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("settle after two good reads then persistent connection errors: %v; report %+v", err, rep)
		}
		if h.w.dstDB.pmCalls != 2 || rep.Unobserved == 0 {
			t.Fatalf("want two observed samples followed by unobserved polls: reads=%d report=%+v", h.w.dstDB.pmCalls, rep)
		}
	})
}

// A run that halted because the promoted target did not settle in time (the
// production case: it settled right after) is resumed: the engine checks both
// sides, counts the switchover as done and cuts over to the target.
func TestResumeHaltedAfterTargetSettles(t *testing.T) {
	h := newHarness(t)
	h.w.restartGets = 1 << 30 // never settles during the run
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err == nil || e.State().Outcome != OutcomeHalted {
		t.Fatalf("want halted, got %v %s", err, e.State().Outcome)
	}
	if err := e.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "is halted") {
		t.Fatalf("a halted run must not resume by itself: %v", err)
	}
	// The target settles later.
	h.w.mu.Lock()
	h.w.restartGets = 1
	h.w.mu.Unlock()

	// A fresh engine on the same state file, as the CLI does with -run.
	e2 := h.engine(t, e.RunID())
	if err := e2.ResumeHalted(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := e2.Run(context.Background()); err != nil {
		t.Fatalf("run after resume: %v", err)
	}
	st := e2.State()
	if st.Outcome != OutcomeSucceeded || !st.TrafficOnTarget || st.WritesHeld {
		t.Fatalf("state %+v", st)
	}
	if _, ok := st.Steps[StepSwitchover].Evidence["resumedAfterHalt"]; !ok {
		t.Fatalf("switchover evidence lacks the resume: %v", st.Steps[StepSwitchover].Evidence)
	}
	calls := h.w.Calls()
	if n := strings.Count(strings.Join(calls, "\n"), `dst.setPrimary db-eks token="tok-1000"`); n != 1 {
		t.Fatalf("target promoted %d times: %v", n, calls)
	}
	assertOrder(t, calls, "dst.restart done", "gw.route eks", "gw.release")
}

// Resume refuses when it cannot prove there is one primary.
func TestResumeHaltedRefusesWithoutProof(t *testing.T) {
	for name, tc := range map[string]struct {
		fault func(w *world)
		want  string
	}{
		"halted before promotion": {func(w *world) { w.noDemotionToken = true }, "before the target's promotion"},
		"target not primary": {func(w *world) {
			w.restartGets = 1 << 30
			w.afterHalt = func(w *world) { w.dst.st.ReplicaPrimary = "db-gke" }
		}, "target is not the topology primary"},
		"source still writable": {func(w *world) {
			w.restartGets = 1 << 30
			w.afterHalt = func(w *world) { w.restartGets = 1; w.srcDB.recovery = false }
		}, "cannot confirm the source runs as a replica"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			tc.fault(h.w)
			e := h.engine(t, "")
			if err := e.Run(context.Background()); err == nil || e.State().Outcome != OutcomeHalted {
				t.Fatalf("want halted, got %v %s", err, e.State().Outcome)
			}
			if f := h.w.afterHalt; f != nil {
				h.w.mu.Lock()
				f(h.w)
				h.w.mu.Unlock()
			}
			err := e.ResumeHalted(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resume: %v, want %q", err, tc.want)
			}
			if e.State().Outcome != OutcomeHalted || contains(h.w.Calls(), "gw.release") {
				t.Fatalf("a refused resume changed the run: %s %v", e.State().Outcome, h.w.Calls())
			}
		})
	}
}

// A fence gate that would hold writes longer than maxFenceGate (a big table)
// is found in delta, while writes still flow; they are never held.
func TestSlowFenceGateFailsBeforeTheHold(t *testing.T) {
	h := newHarness(t)
	h.p.Thresholds.MaxFenceGate.Duration = 20 * time.Millisecond
	h.w.ver.delay = 60 * time.Millisecond
	e := h.engine(t, "")
	err := e.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "over thresholds.maxFenceGate") {
		t.Fatalf("err = %v", err)
	}
	st := e.State()
	if st.Outcome != OutcomeFailed || st.FailureStep != StepDelta || st.WritesHeld || contains(h.w.Calls(), "gw.hold") {
		t.Fatalf("outcome %s step %s held %t calls %v", st.Outcome, st.FailureStep, st.WritesHeld, h.w.Calls())
	}
}

// A new target that refuses its first connections (service not ready yet) is
// retried in prepare, not a failed run.
func TestPrepareRetriesTargetConnection(t *testing.T) {
	h := newHarness(t)
	h.w.dstConnectErrs = 3
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// runThrough runs the steps before stop, so a test can break something
// between steps (a fault that would also break an earlier step).
func runThrough(t *testing.T, e *Engine, stop string) {
	t.Helper()
	for _, s := range Steps {
		if s == stop {
			return
		}
		if err := e.RunStep(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func TestTargetMissingWithoutManifestFailsPrecheck(t *testing.T) {
	h := newHarness(t)
	h.w.dst.exists = false
	h.p.Target.Manifest = ""
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "target.manifest is not set") {
		t.Fatalf("err = %v", err)
	}
	if e.State().Outcome != OutcomeFailed || contains(h.w.Calls(), "gw.hold") {
		t.Fatalf("outcome %s, calls %v", e.State().Outcome, h.w.Calls())
	}
}

// A connection blip while measuring lag is retried, not a failure.
func TestLagCheckRetriesTransientErrors(t *testing.T) {
	h := newHarness(t)
	h.w.dstDB.posErrCalls = 3
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if e.State().Outcome != OutcomeSucceeded {
		t.Fatalf("outcome %s", e.State().Outcome)
	}
}

// A file sync failure in delta happens before writes are held: nothing to undo.
func TestDeltaFileSyncFailureDoesNotHoldWrites(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")
	runThrough(t, e, StepDelta)
	h.w.files.syncErr = errors.New("bucket unreachable")
	if err := e.RunStep(context.Background(), StepDelta); err == nil {
		t.Fatal("expected failure")
	}
	if st := e.State(); st.Outcome != OutcomeFailed || st.WritesHeld {
		t.Fatalf("outcome %s writesHeld %t", st.Outcome, st.WritesHeld)
	}
	if contains(h.w.Calls(), "gw.hold") {
		t.Fatal("writes were held")
	}
}

// Writes are already held when the fence LSN cannot be read: release them on
// the source.
func TestFenceLSNReadFailureReleasesWrites(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")
	runThrough(t, e, StepFence)
	h.w.srcDB.posErr = errors.New("connection reset by peer")
	if err := e.RunStep(context.Background(), StepFence); err == nil {
		t.Fatal("expected failure")
	}
	st := e.State()
	if st.Outcome != OutcomeRolledBack || st.WritesHeld || st.SourceDemoted {
		t.Fatalf("state %+v", st)
	}
	assertOrder(t, h.w.Calls(), "gw.hold", "gw.route gke", "gw.release")
	if slices.ContainsFunc(h.w.Calls(), func(c string) bool { return strings.HasPrefix(c, "src.setPrimary") }) {
		t.Fatal("the source was touched")
	}
}

// The demotion request itself fails (API error, webhook): CNPG never started
// demoting, so the source is still primary. Roll back right away instead of
// waiting for a demotion token that will never come and halting.
func TestDemotionRequestFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	h.w.src.setErr = errors.New("the server rejected our request")
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	st := e.State()
	if st.Outcome != OutcomeRolledBack || st.SourceDemoted || st.WritesHeld {
		t.Fatalf("outcome %s demoted %t held %t (rollbackError %q)", st.Outcome, st.SourceDemoted, st.WritesHeld, st.RollbackError)
	}
	assertOrder(t, h.w.Calls(), `src.setPrimary db-eks token=""`, "gw.route gke", "gw.release")
	if slices.ContainsFunc(h.w.Calls(), func(c string) bool { return strings.HasPrefix(c, "dst.setPrimary") }) {
		t.Fatal("the target was promoted")
	}
}

// The rollback cannot move traffic back: halted, with the reason recorded.
func TestRollbackRouteFailureHalts(t *testing.T) {
	h := newHarness(t)
	h.w.ver.gateOK = false
	h.w.gw.routeErr, h.w.gw.routeErrTo = errors.New("503"), "gke"
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("err = %v", err)
	}
	st := e.State()
	if st.Outcome != OutcomeHalted || st.RollbackError == "" || !st.WritesHeld {
		t.Fatalf("outcome %s rollbackError %q writesHeld %t", st.Outcome, st.RollbackError, st.WritesHeld)
	}
	if contains(h.w.Calls(), "gw.release") {
		t.Fatal("writes released without a route to the source")
	}
}

// The re-promoted source never settles: the rollback cannot finish safely, so
// writes stay held and the run halts for an operator.
func TestRollbackHaltsWhenSourceNeverSettles(t *testing.T) {
	h := newHarness(t)
	h.w.tokenAfter = 1200 * time.Millisecond
	h.w.srcRestartGets = 1 << 30
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	st := e.State()
	if st.Outcome != OutcomeHalted || !strings.Contains(st.RollbackError, "did not settle") || !st.WritesHeld {
		t.Fatalf("outcome %s rollbackError %q writesHeld %t", st.Outcome, st.RollbackError, st.WritesHeld)
	}
	if contains(h.w.Calls(), "gw.release") {
		t.Fatal("writes released to a source that is restarting")
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
	var resumed, warm int
	for _, ev := range h.kinds(EventStepSkipped) {
		switch {
		case ev.Message == "already succeeded":
			resumed++
		case ev.Step == StepStartTarget:
			warm++
		default:
			t.Errorf("unexpected skip: %+v", ev)
		}
	}
	if resumed != 3 || warm != 1 {
		t.Fatalf("skipped %d succeeded steps (want 3) and start-target %d times (want 1 in warm mode)", resumed, warm)
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

// CNPG restarts a new primary in place right after promotion (seen on a real
// cluster under load). Writes must stay held until that restart is over, or
// the released writes hit a primary that is shutting down.
func TestWritesHeldUntilPromotedTargetSettles(t *testing.T) {
	h := newHarness(t)
	h.w.restartGets = 5
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertOrder(t, h.w.Calls(), `dst.setPrimary db-eks token="tok-1000"`, "dst.restart done", "gw.route eks", "gw.release")
}

// A rollback re-promotes the source, and CNPG restarts it in place the same
// way it restarts a promoted target (seen in the gateway rehearsal: writes
// released right after the re-promotion failed with "the database system is
// shutting down"). Held writes must wait for that restart too.
func TestRollbackHoldsWritesUntilSourceSettles(t *testing.T) {
	h := newHarness(t)
	h.w.tokenAfter = 1200 * time.Millisecond
	h.w.srcRestartGets = 5
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err == nil {
		t.Fatal("expected a rollback")
	}
	if got := e.State().Outcome; got != OutcomeRolledBack {
		t.Fatalf("outcome %q, want rolled_back", got)
	}
	assertOrder(t, h.w.Calls(), `src.setPrimary db-gke token="tok-1000"`, "src.restart done", "gw.route gke", "gw.release")
}

// The old source restarts as a replica after demotion. A connection error
// while it does must not fail verification (which would halt the run).
func TestVerifyWaitsForOldSourceRestart(t *testing.T) {
	h := newHarness(t)
	h.w.sourceDownCalls = 3
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if got := e.State().Outcome; got != OutcomeSucceeded {
		t.Fatalf("outcome %q, want succeeded", got)
	}
}
