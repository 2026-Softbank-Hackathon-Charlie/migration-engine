package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Cold plans (ADR 0012): the target app is kept at zero until the target is
// promoted, then started and smoke-tested while writes are still held.

const smokeCall = "probe GET http://web.shop.svc:8080/healthz"

func TestColdStartsAppAfterPromotionBeforeCutover(t *testing.T) {
	h := newColdHarness(t)
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
		"app.stop", // prepare: no app on the replica
		"gw.hold",
		`src.setPrimary db-eks token=""`,
		`dst.setPrimary db-eks token="tok-1000"`,
		"app.scale 2", // only after promotion
		smokeCall,
		"gw.route eks",
		"gw.release",
	)
	if rec := st.Steps[StepStartTarget]; rec.Status != StatusSucceeded || rec.Evidence["startMillis"] == nil {
		t.Fatalf("start-target record: %+v", rec)
	}
	if len(h.w.prober.urls) == 0 {
		t.Fatal("no smoke request was sent")
	}
	if st.WritesHeld || !st.TrafficOnTarget || !st.PromoteRequested {
		t.Errorf("flags: %+v", st)
	}
}

func TestWarmSkipsStartTarget(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.w.Calls() {
		if strings.HasPrefix(c, "app.") || strings.HasPrefix(c, "probe ") {
			t.Fatalf("warm plan touched the target app: %q", c)
		}
	}
	rec := e.State().Steps[StepStartTarget]
	if rec.Status != StatusSucceeded || rec.Evidence["skipped"] == nil {
		t.Fatalf("start-target record: %+v", rec)
	}
}

// A failed start-target is undone: the target is demoted, the source promoted
// with the target's token, and the held writes go to the source.
func TestColdStartFailureRollsBackPromotion(t *testing.T) {
	cases := map[string]func(h *harness){
		"pods never ready": func(h *harness) { h.w.app.neverUp = true },
		"smoke fails":      func(h *harness) { h.w.prober.status = 500 },
		"smoke unreachable": func(h *harness) {
			h.w.prober.err = errors.New("dial tcp: connection refused")
		},
		"scale rejected": func(h *harness) { h.w.app.scaleErr = errors.New("forbidden") },
	}
	for name, fault := range cases {
		t.Run(name, func(t *testing.T) {
			h := newColdHarness(t)
			fault(h)
			e := h.engine(t, "")
			err := e.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "rolled back after start-target") {
				t.Fatalf("err = %v", err)
			}
			st := e.State()
			if st.Outcome != OutcomeRolledBack || st.RollbackError != "" {
				t.Fatalf("outcome %s rollbackError %q", st.Outcome, st.RollbackError)
			}
			calls := h.w.Calls()
			promoted := indexOf(t, calls, `dst.setPrimary db-eks token="tok-1000"`)
			assertOrder(t, calls[promoted:],
				"app.stop",                       // the failed app is stopped again
				`dst.setPrimary db-gke token=""`, // target demoted
				`src.setPrimary db-gke token="tokd-1000"`,
				"gw.route gke",
				"gw.release",
			)
			if contains(calls, "gw.route eks") {
				t.Fatal("traffic was routed to the target")
			}
			if h.w.srcDB.recovery || !h.w.dstDB.recovery {
				t.Fatalf("roles after rollback: source in recovery %t, target in recovery %t", h.w.srcDB.recovery, h.w.dstDB.recovery)
			}
			if st.WritesHeld || st.SourceDemoted || st.PromoteRequested || st.TrafficOnTarget {
				t.Fatalf("flags after rollback: %+v", st)
			}
		})
	}
}

// Writes held past maxDowntime before the app could start: fail without
// starting it and roll back.
func TestColdHoldOverBudgetRollsBackWithoutStartingApp(t *testing.T) {
	h := newColdHarness(t)
	h.p.Thresholds.MaxDowntime.Duration = time.Millisecond
	e := h.engine(t, "")
	err := e.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "over maxDowntime") {
		t.Fatalf("err = %v", err)
	}
	if e.State().Outcome != OutcomeRolledBack {
		t.Fatalf("outcome = %s", e.State().Outcome)
	}
	if contains(h.w.Calls(), "app.scale 2") {
		t.Fatal("app started although the write hold was already over budget")
	}
}

// A rollback that cannot finish leaves writes held for an operator.
func TestColdRollbackFailureHaltsWithWritesHeld(t *testing.T) {
	cases := map[string]struct {
		fault func(h *harness)
		want  string
	}{
		"target demotion rejected": {func(h *harness) { h.w.dstDemoteErr = errors.New("conflict") }, "demote target"},
		"no target demotion token": {func(h *harness) { h.w.noTargetDemotionToken = true }, "demotion did not finish"},
		"app will not stop":        {func(h *harness) { h.w.app.stopErrFrom = 2 }, "stop target app"},
		"source never settles":     {func(h *harness) { h.w.srcRestartGets = 1 << 30 }, "did not settle"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newColdHarness(t)
			h.w.prober.status = 500
			c.fault(h)
			e := h.engine(t, "")
			err := e.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "rollback failed") {
				t.Fatalf("err = %v", err)
			}
			st := e.State()
			if st.Outcome != OutcomeHalted || !strings.Contains(st.RollbackError, c.want) || !st.WritesHeld {
				t.Fatalf("outcome %s rollbackError %q writesHeld %t", st.Outcome, st.RollbackError, st.WritesHeld)
			}
			if contains(h.w.Calls(), "gw.release") {
				t.Fatal("writes released although the rollback did not finish")
			}
			if len(h.kinds(EventHalt)) == 0 {
				t.Fatal("no halt event for the operator")
			}
		})
	}
}

// Past start-target the old rule holds: a promoted target is never demoted.
func TestColdFailureAfterStartTargetHalts(t *testing.T) {
	h := newColdHarness(t)
	h.w.gw.routeErr, h.w.gw.routeErrTo = errors.New("gateway lost"), "eks"
	e := h.engine(t, "")
	err := e.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "halted at cutover") {
		t.Fatalf("err = %v", err)
	}
	if e.State().Outcome != OutcomeHalted {
		t.Fatalf("outcome = %s", e.State().Outcome)
	}
	if contains(h.w.Calls(), `dst.setPrimary db-gke token=""`) {
		t.Fatal("target demoted after start-target had succeeded")
	}
}

// Before anything irreversible, cold-specific checks fail the run plainly.
func TestColdEarlyFailures(t *testing.T) {
	cases := map[string]struct {
		fault func(h *harness)
		step  string
	}{
		"app unreadable":          {func(h *harness) { h.w.app.runErr = errors.New("forbidden") }, StepPrecheck},
		"app pods do not go away": {func(h *harness) { h.w.app.neverDown = true }, StepPrepare},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newColdHarness(t)
			c.fault(h)
			e := h.engine(t, "")
			if err := e.Run(context.Background()); err == nil {
				t.Fatal("expected failure")
			}
			st := e.State()
			if st.Outcome != OutcomeFailed || st.FailureStep != c.step {
				t.Fatalf("outcome %s at %s", st.Outcome, st.FailureStep)
			}
			if contains(h.w.Calls(), "gw.hold") {
				t.Fatal("writes held before the app checks passed")
			}
		})
	}
}

// The rollback guard checks every flag, not only the step name.
func TestCanRevertPromotionNeedsEveryCondition(t *testing.T) {
	h := newColdHarness(t)
	e := h.engine(t, "")
	set := func(f func(*State)) {
		e.state.PromoteRequested, e.state.SourceDemoted, e.state.WritesHeld, e.state.TrafficOnTarget = true, true, true, false
		f(e.state)
	}
	set(func(*State) {})
	if !e.canRevertPromotion(StepStartTarget) {
		t.Fatal("all conditions met but not revertible")
	}
	for name, f := range map[string]func(*State){
		"traffic on target": func(s *State) { s.TrafficOnTarget = true },
		"writes released":   func(s *State) { s.WritesHeld = false },
		"not promoted":      func(s *State) { s.PromoteRequested = false },
	} {
		set(f)
		if e.canRevertPromotion(StepStartTarget) {
			t.Errorf("%s: revertible", name)
		}
	}
	set(func(*State) {})
	if e.canRevertPromotion(StepCutover) || e.canRevertPromotion(StepSwitchover) {
		t.Error("only start-target may revert a promotion")
	}
	warm := newHarness(t)
	w := warm.engine(t, "")
	w.state.PromoteRequested, w.state.SourceDemoted, w.state.WritesHeld = true, true, true
	if w.canRevertPromotion(StepStartTarget) {
		t.Error("a warm plan may not revert a promotion")
	}
}
