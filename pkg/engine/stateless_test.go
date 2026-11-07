package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// Stateless plans (ADR 0018): an app without a database, such as docker-doom,
// moves from eks to gke by the gateway route and its files only.

const statelessSmoke = "probe GET http://doom.gke.example:8080/"

func statelessPlan(t *testing.T, app string) *plan.Plan {
	t.Helper()
	p, err := plan.Parse([]byte(`{
	  "id": "doom-eks-to-gke",
	  "source": {"kind":"none","env":"eks"},
	  "target": {"kind":"none","env":"gke"` + app + `},
	  "gateway": {"mode":"http","url":"http://gw:9901"},
	  "files": {"source":"src:b","target":"dst:b"},
	  "thresholds": {"switchoverTimeout":"200ms","maxDowntime":"3m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const warmSmokeApp = `,"app":{"smokeUrl":"http://doom.gke.example:8080","smoke":[{"method":"GET","path":"/"}]}`

func newStatelessHarness(t *testing.T, app string) *harness {
	w := newWorld()
	// No database and no CNPG Cluster may be touched.
	w.connectErr = errors.New("no database")
	w.src.getErr, w.src.setErr = errors.New("no CNPG source"), errors.New("no CNPG source")
	w.dst.getErr, w.dst.setErr = errors.New("no CNPG target"), errors.New("no CNPG target")
	// The fake app and prober answer only when the target database is out of
	// recovery; a stateless app has none.
	w.dstDB.recovery = false
	return &harness{w: w, p: statelessPlan(t, app), dir: t.TempDir()}
}

func assertNoDatabaseCalls(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		if strings.HasPrefix(c, "src.") || strings.HasPrefix(c, "dst.") || strings.HasPrefix(c, "verify.") || strings.HasPrefix(c, "reindex") {
			t.Fatalf("database call %q in a stateless run: %v", c, calls)
		}
	}
}

func count(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

func TestStatelessMigration(t *testing.T) {
	h := newStatelessHarness(t, warmSmokeApp)
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := e.State()
	if st.Outcome != OutcomeSucceeded {
		t.Fatalf("outcome = %s", st.Outcome)
	}
	calls := h.w.Calls()
	assertOrder(t, calls,
		"gw.health",
		statelessSmoke, // precheck: the target app answers
		"files.copy",   // prepare
		"gw.hold",
		"gw.route gke",
		"gw.release",
	)
	if n := count(calls, "files.copy"); n != 3 {
		t.Errorf("files.copy %d times, want prepare, delta and fence: %v", n, calls)
	}
	if i := slicesIndexAfter(calls, "gw.hold", "files.copy"); i < 0 || i > indexOf(t, calls, "gw.route gke") {
		t.Fatalf("no last file copy between the hold and the route: %v", calls)
	}
	if i := slicesIndexAfter(calls, "gw.release", statelessSmoke); i < 0 {
		t.Fatalf("verify did not smoke the target app: %v", calls)
	}
	if slices.Contains(calls, "gw.route eks") || slices.Contains(calls, "app.stop") {
		t.Fatalf("unexpected call: %v", calls)
	}
	assertNoDatabaseCalls(t, calls)
	if !st.TrafficOnTarget || st.WritesHeld || st.PromoteRequested || st.SourceDemoted || st.FenceLSN != "" {
		t.Errorf("flags: %+v", st)
	}
	for _, s := range []string{StepPresync, StepSwitchover, StepStartTarget} {
		if st.Steps[s].Evidence["skipped"] == nil {
			t.Errorf("%s not skipped: %v", s, st.Steps[s].Evidence)
		}
	}
	if rep, ok := st.Steps[StepVerify].Evidence["report"].(*Report); !ok || !rep.OK || rep.Files == nil {
		t.Errorf("verify evidence: %v", st.Steps[StepVerify].Evidence)
	}
}

// A cold target app is stopped in prepare and started while writes are held.
func TestStatelessCold(t *testing.T) {
	h := newStatelessHarness(t, `,"namespace":"doom","app":{"mode":"cold","kind":"Deployment","name":"doom","replicas":2,"startTimeout":"500ms",
	  "smokeUrl":"http://doom.gke.example:8080","smoke":[{"method":"GET","path":"/"}]}`)
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := e.State(); st.Outcome != OutcomeSucceeded {
		t.Fatalf("outcome = %s", st.Outcome)
	}
	calls := h.w.Calls()
	assertOrder(t, calls, "app.stop", "gw.hold", "app.scale 2", statelessSmoke, "gw.route gke", "gw.release")
	assertNoDatabaseCalls(t, calls)
}

// Before anything is held a failure only fails the run.
func TestStatelessFailureBeforeFence(t *testing.T) {
	h := newStatelessHarness(t, warmSmokeApp)
	h.w.prober.status = 502
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err == nil {
		t.Fatal("run succeeded with a target app that does not answer")
	}
	st := e.State()
	if st.Outcome != OutcomeFailed || st.FailureStep != StepPrecheck {
		t.Fatalf("outcome %s at %s", st.Outcome, st.FailureStep)
	}
	for _, c := range h.w.Calls() {
		if strings.HasPrefix(c, "gw.hold") || strings.HasPrefix(c, "gw.route") {
			t.Fatalf("gateway changed by a failed precheck: %v", h.w.Calls())
		}
	}

	h = newStatelessHarness(t, "")
	h.w.files.checkErr = errors.New("bucket unreachable")
	e = h.engine(t, "")
	if err := e.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "files source") {
		t.Fatalf("err = %v", err)
	}
}

// From the hold up to the cutover a failure routes back to the source and
// releases the held writes there.
func TestStatelessFailureBeforeReleaseRollsBack(t *testing.T) {
	cases := map[string]struct {
		fault   func(w *world)
		failsAt string
	}{
		"last file copy":  {func(w *world) { w.files.syncErr = errors.New("bucket unreachable") }, StepFence},
		"route to target": {func(w *world) { w.gw.routeErr, w.gw.routeErrTo = errors.New("502"), "gke" }, StepCutover},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newStatelessHarness(t, warmSmokeApp)
			e := h.engine(t, "")
			for _, s := range Steps {
				if s == tc.failsAt {
					break
				}
				if err := e.RunStep(context.Background(), s); err != nil {
					t.Fatal(s, err)
				}
			}
			tc.fault(h.w)
			if err := e.Run(context.Background()); err == nil {
				t.Fatal("run succeeded")
			}
			st := e.State()
			if st.Outcome != OutcomeRolledBack || st.FailureStep != tc.failsAt {
				t.Fatalf("outcome %s at %s (%s)", st.Outcome, st.FailureStep, st.RollbackError)
			}
			calls := h.w.Calls()
			assertOrder(t, calls, "gw.hold", "gw.route eks", "gw.release")
			if st.WritesHeld || st.TrafficOnTarget {
				t.Errorf("flags: %+v", st)
			}
			if h.w.gw.routedTo != "eks" {
				t.Errorf("gateway routes to %q", h.w.gw.routedTo)
			}
			assertNoDatabaseCalls(t, calls)
		})
	}
}

// runUntil runs every step before stop.
func runUntil(t *testing.T, e *Engine, stop string) {
	t.Helper()
	for _, s := range Steps[:slices.Index(Steps, stop)] {
		if err := e.RunStep(context.Background(), s); err != nil {
			t.Fatal(s, err)
		}
	}
}

// After the cutover released the writes, a failed verify of a plan without
// files routes back: nothing but the route moved.
func TestStatelessRollbackAfterRelease(t *testing.T) {
	h := newStatelessHarness(t, warmSmokeApp)
	h.p.Files = nil
	e := h.engine(t, "")
	runUntil(t, e, StepVerify)
	h.w.prober.status = 500
	if err := e.Run(context.Background()); err == nil {
		t.Fatal("run succeeded")
	}
	st := e.State()
	if st.Outcome != OutcomeRolledBack || st.FailureStep != StepVerify {
		t.Fatalf("outcome %s at %s (%s)", st.Outcome, st.FailureStep, st.RollbackError)
	}
	calls := h.w.Calls()
	assertOrder(t, calls, "gw.route gke", "gw.release", "gw.route eks")
	if n := count(calls, "gw.release"); n != 1 {
		t.Errorf("gw.release %d times; nothing was held at the rollback: %v", n, calls)
	}
	if st.TrafficOnTarget || h.w.gw.routedTo != "eks" {
		t.Errorf("traffic still on target: %+v", st)
	}
	var msgs []string
	for _, ev := range h.kinds(EventRollback) {
		msgs = append(msgs, ev.Message)
	}
	if !strings.Contains(strings.Join(msgs, "|"), "second traffic flip") {
		t.Errorf("rollback events do not name the second flip: %v", msgs)
	}
}

// With files, a failure after the cutover halts with traffic on the target:
// users may have written files there. The resume routes back.
func TestStatelessFilesHaltAfterRelease(t *testing.T) {
	cases := map[string]func(w *world){
		"files missing on target": func(w *world) { w.files.checkMissing = 2 },
		"target app fails smoke":  func(w *world) { w.prober.status = 500 },
	}
	for name, fault := range cases {
		t.Run(name, func(t *testing.T) {
			h := newStatelessHarness(t, warmSmokeApp)
			e := h.engine(t, "")
			runUntil(t, e, StepVerify)
			fault(h.w)
			if err := e.Run(context.Background()); err == nil {
				t.Fatal("run succeeded")
			}
			st := e.State()
			if st.Outcome != OutcomeHalted || !st.TrafficOnTarget || st.WritesHeld || h.w.gw.routedTo != "gke" {
				t.Fatalf("not halted on the target: %+v routed %s", st, h.w.gw.routedTo)
			}
			if contains(h.w.Calls(), "gw.route eks") {
				t.Fatalf("routed back without the operator: %v", h.w.Calls())
			}
			h.w.files.checkMissing, h.w.prober.status = 0, 0
			resumed := h.engine(t, e.RunID())
			if err := resumed.ResumeHalted(context.Background()); !errors.Is(err, ErrResumeRolledBackStateless) || !errors.Is(err, ErrResumeRolledBack) {
				t.Fatalf("resume: %v", err)
			}
			if st := resumed.State(); st.Outcome != OutcomeRolledBack || st.TrafficOnTarget || h.w.gw.routedTo != "eks" {
				t.Fatalf("after resume: %+v", st)
			}
		})
	}
}

// Cancelling the caller's context during verify does not undo a migration
// that went through; the resume repeats verify and finishes.
func TestStatelessCancelAfterCutoverHalts(t *testing.T) {
	h := newStatelessHarness(t, warmSmokeApp)
	h.p.Files = nil
	e := h.engine(t, "")
	runUntil(t, e, StepVerify)
	h.w.prober.status = 500 // verify keeps retrying until the cancel
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for deadline := time.Now().Add(5 * time.Second); count(h.w.Calls(), statelessSmoke) < 2 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if err := e.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	st := e.State()
	if st.Outcome != OutcomeHalted || !st.TrafficOnTarget || contains(h.w.Calls(), "gw.route eks") {
		t.Fatalf("cancel rolled back or did not halt: %+v %v", st, h.w.Calls())
	}
	h.w.prober.status = 0
	resumed := h.engine(t, e.RunID())
	if err := resumed.ResumeHalted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := resumed.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := resumed.State(); st.Outcome != OutcomeSucceeded || h.w.gw.routedTo != "gke" {
		t.Fatalf("after resume: %+v", st)
	}
}

// A failed rollback halts; the resume routes back and releases.
func TestStatelessHaltAndResume(t *testing.T) {
	h := newStatelessHarness(t, warmSmokeApp)
	e := h.engine(t, "")
	for _, s := range Steps[:slices.Index(Steps, StepCutover)] {
		if err := e.RunStep(context.Background(), s); err != nil {
			t.Fatal(s, err)
		}
	}
	h.w.gw.routeErr = errors.New("gateway down") // both directions
	if err := e.Run(context.Background()); err == nil {
		t.Fatal("run succeeded")
	}
	if st := e.State(); st.Outcome != OutcomeHalted || !st.WritesHeld || st.RollbackError == "" {
		t.Fatalf("state after failed rollback: %+v", st)
	}

	h.w.gw.routeErr = nil
	resumed := h.engine(t, e.RunID())
	err := resumed.ResumeHalted(context.Background())
	if !errors.Is(err, ErrResumeRolledBackStateless) {
		t.Fatalf("resume: %v", err)
	}
	st := resumed.State()
	if st.Outcome != OutcomeRolledBack || st.WritesHeld || st.TrafficOnTarget || st.RollbackError != "" {
		t.Fatalf("state after resume: %+v", st)
	}
	if h.w.gw.routedTo != "eks" || !slices.Contains(h.w.Calls(), "gw.release") {
		t.Fatalf("calls: %v", h.w.Calls())
	}
}

// An interrupted cutover halts (checkRunnable); the resume undoes a hold and
// a route that may not have been saved.
func TestStatelessInterruptedCutoverResumeRollsBack(t *testing.T) {
	h := newStatelessHarness(t, "")
	e := h.engine(t, "")
	for _, s := range Steps[:slices.Index(Steps, StepCutover)] {
		if err := e.RunStep(context.Background(), s); err != nil {
			t.Fatal(s, err)
		}
	}
	e.setFlags(func(s *State) { s.WritesHeld = false }) // the save of the hold was lost
	e.state.Steps[StepCutover].Status = StatusRunning
	e.save()

	resumed := h.engine(t, e.RunID())
	if err := resumed.Run(context.Background()); err == nil || resumed.State().Outcome != OutcomeHalted {
		t.Fatalf("interrupted cutover not halted: %v", err)
	}
	before := len(h.w.Calls())
	if err := resumed.ResumeHalted(context.Background()); !errors.Is(err, ErrResumeRolledBackStateless) {
		t.Fatalf("resume: %v", err)
	}
	after := h.w.Calls()[before:]
	assertOrder(t, after, "gw.route eks", "gw.release")
	st := resumed.State()
	if st.Outcome != OutcomeRolledBack || st.Steps[StepCutover].Status != StatusFailed {
		t.Fatalf("state: %+v", st)
	}
}
