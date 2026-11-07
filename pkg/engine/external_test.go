package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// External source (ADR 0019): a PostgreSQL primary outside CNPG that the
// target Cluster streams from under the externalClusters name "legacy".

func externalPlan(t *testing.T) *plan.Plan {
	t.Helper()
	p, err := plan.Parse([]byte(`{
	  "id": "shop-legacy-to-eks",
	  "source": {"kind":"external","name":"legacy","env":"legacy","dsn":"src-dsn"},
	  "target": {"env":"eks","namespace":"shop","cluster":"db-eks","dsn":"dst-dsn",
	    "reindex": ["public.items_name_idx", "public.users_email_idx"],
	    "app": {"mode":"cold","kind":"Deployment","name":"web","replicas":2,"startTimeout":"500ms",
	      "smokeUrl":"http://web.shop.svc:8080","smoke":[{"method":"GET","path":"/healthz"}]}},
	  "gateway": {"mode":"http","url":"http://gw:9901"},
	  "files": {"source":"legacy:b","target":"dst:b"},
	  "thresholds": {"catchupTimeout":"2s","switchoverTimeout":"1s","maxDowntime":"3m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newExternalHarness(t *testing.T) *harness {
	w := newWorld()
	w.external = &fakeExternal{w: w}
	w.dst.st.ReplicaPrimary, w.dst.st.ReplicaSource = "legacy", "legacy"
	// The CNPG source fake must never be touched: make every call fail.
	w.src.getErr = errors.New("no CNPG source")
	w.src.setErr = errors.New("no CNPG source")
	return &harness{w: w, p: externalPlan(t), dir: t.TempDir()}
}

const externalPromote = `dst.setPrimary db-eks token=""`

func TestExternalSwitchover(t *testing.T) {
	h := newExternalHarness(t)
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
		"src.fence",
		"verify.gate",
		externalPromote, // no promotion token
		"reindex public.items_name_idx",
		"reindex public.users_email_idx",
		"app.scale 2",
		smokeCall,
		"gw.route eks",
		"gw.release",
		"verify.report",
	)
	if i := slicesIndexAfter(calls, "src.fence", "files.sync"); i < 0 || i > indexOf(t, calls, "verify.gate") {
		t.Fatalf("no last file copy between the fence and the gate: %v", calls)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "src.setPrimary") || c == "src.unfence" || c == "gw.route legacy" {
			t.Fatalf("unexpected call %q: %v", c, calls)
		}
	}
	if !h.w.external.readOnly {
		t.Error("the source must stay read-only after cutover")
	}
	if st.FenceLSN != "1000" || !st.SourceFenced || !st.PromoteRequested || !st.TrafficOnTarget || st.WritesHeld || st.SourceDemoted {
		t.Errorf("flags: %+v", st)
	}
	if ev := st.Steps[StepFence].Evidence; ev["terminatedSessions"] != int64(3) {
		t.Errorf("fence evidence: %v", ev)
	}
	if ev := st.Steps[StepStartTarget].Evidence; ev["reindex"] == nil || ev["startMillis"] == nil {
		t.Errorf("start-target evidence: %v", ev)
	}
}

// Before the promotion request the source is made writable again and the
// held writes go back to it.
func TestExternalFailureBeforePromotionUnfences(t *testing.T) {
	cases := map[string]struct {
		fault   func(w *world)
		failsAt string
	}{
		"fence fails":                {func(w *world) { w.external.fenceErr = errors.New("permission denied") }, StepFence},
		"last file copy":             {func(w *world) { w.files.syncErr = errors.New("bucket unreachable") }, StepFence},
		"fence LSN never replayed":   {func(w *world) { w.dstDB.posErr = errors.New("replay stuck") }, StepSwitchover},
		"fence gate mismatch":        {func(w *world) { w.ver.gateOK = false }, StepSwitchover},
		"fence applied, answer lost": {func(w *world) { w.external.fenceAppliedErr = errors.New("connection reset by peer") }, StepFence},
		"write after the fence":      {func(w *world) { w.external.nextXid++ }, StepSwitchover},
		"open write session":         {func(w *world) { w.external.writeSessions = 1 }, StepSwitchover},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newExternalHarness(t)
			e := h.engine(t, "")
			// The steps before the failing one succeed; the fault starts there.
			for _, s := range Steps {
				if s == tc.failsAt {
					break
				}
				if err := e.RunStep(context.Background(), s); err != nil {
					t.Fatal(s, err)
				}
			}
			h.w.mu.Lock()
			tc.fault(h.w)
			h.w.mu.Unlock()
			err := e.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "rolled back after "+tc.failsAt) {
				t.Fatalf("err = %v", err)
			}
			st := e.State()
			if st.Outcome != OutcomeRolledBack || st.RollbackError != "" {
				t.Fatalf("outcome %s rollbackError %q", st.Outcome, st.RollbackError)
			}
			calls := h.w.Calls()
			assertOrder(t, calls, "gw.hold", "src.fence", "src.unfence", "gw.route legacy", "gw.release")
			if contains(calls, externalPromote) || contains(calls, "gw.route eks") {
				t.Fatalf("promoted or routed to the target: %v", calls)
			}
			if h.w.external.readOnly || st.SourceFenced || st.WritesHeld || st.PromoteRequested {
				t.Fatalf("after rollback: readOnly %t, state %+v", h.w.external.readOnly, st)
			}
		})
	}
}

// After the promotion request nothing is undone: the source stays read-only,
// the writes stay held, the target is never demoted.
func TestExternalFailureAfterPromotionHalts(t *testing.T) {
	cases := map[string]func(w *world){
		"target never writable": func(w *world) { w.targetNeverWakes = true },
		"reindex fails":         func(w *world) { w.reindexErr = errors.New("could not create unique index") },
		"app never ready":       func(w *world) { w.app.neverUp = true },
		"cutover route fails":   func(w *world) { w.gw.routeErr, w.gw.routeErrTo = errors.New("gateway 500"), "eks" },
	}
	for name, fault := range cases {
		t.Run(name, func(t *testing.T) {
			h := newExternalHarness(t)
			fault(h.w)
			e := h.engine(t, "")
			err := e.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "halted at") {
				t.Fatalf("err = %v", err)
			}
			st := e.State()
			if st.Outcome != OutcomeHalted || !st.WritesHeld || !st.SourceFenced {
				t.Fatalf("state %+v", st)
			}
			calls := h.w.Calls()
			for _, c := range calls {
				if c == "src.unfence" || c == "gw.release" || c == "gw.route legacy" || strings.HasPrefix(c, "dst.setPrimary legacy") {
					t.Fatalf("undid something after promotion: %q in %v", c, calls)
				}
			}
			if slicesIndexAfter(calls, externalPromote, "app.stop") >= 0 {
				t.Fatalf("target app stopped after promotion: %v", calls)
			}
			if !h.w.external.readOnly {
				t.Fatal("source writable again after the promotion")
			}
			if len(h.kinds(EventRollback)) != 0 {
				t.Fatal("rollback event after promotion")
			}
		})
	}
}

// slicesIndexAfter is the index of the first want after the first after, or -1.
func slicesIndexAfter(calls []string, after, want string) int {
	seen := false
	for i, c := range calls {
		if c == after {
			seen = true
		}
		if seen && c == want {
			return i
		}
	}
	return -1
}

// A run that halted after promotion carries on on the target once the
// operator resumes it; REINDEX runs again, the target is promoted once.
func TestExternalResumeHalted(t *testing.T) {
	h := newExternalHarness(t)
	h.w.reindexErr = errors.New("deadlock detected")
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err == nil || e.State().Outcome != OutcomeHalted {
		t.Fatalf("want halted, got %v %s", err, e.State().Outcome)
	}
	h.w.mu.Lock()
	h.w.reindexErr = nil
	h.w.mu.Unlock()

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
	calls := h.w.Calls()
	if n := strings.Count(strings.Join(calls, "\n"), externalPromote); n != 1 {
		t.Fatalf("target promoted %d times: %v", n, calls)
	}
	if len(h.w.reindex) != 2 {
		t.Fatalf("reindexed %v", h.w.reindex)
	}
	assertOrder(t, calls, "reindex public.users_email_idx", "app.scale 2", "gw.route eks", "gw.release")
}

func TestExternalResumeHaltedRefusesWithoutProof(t *testing.T) {
	for name, tc := range map[string]struct {
		fault, afterHalt func(w *world)
		want             string
	}{
		// Before promotion a resume rolls back; while the unfence still fails
		// it refuses and the run stays halted.
		"halted before promotion, unfence still failing": {func(w *world) { w.external.unfenceErr = errors.New("connection refused"); w.ver.gateOK = false }, nil,
			"rollback of halted run"},
		"source writable again": {func(w *world) { w.app.neverUp = true }, func(w *world) { w.external.readOnly = false },
			"takes writes again"},
		"source unreachable": {func(w *world) { w.app.neverUp = true }, func(w *world) { w.external.statusErr = errors.New("timeout") },
			"cannot confirm the external source is still read-only"},
		"target not primary": {func(w *world) { w.app.neverUp = true }, func(w *world) { w.dst.st.ReplicaPrimary = "legacy" },
			"target is not the topology primary"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newExternalHarness(t)
			tc.fault(h.w)
			e := h.engine(t, "")
			if err := e.Run(context.Background()); err == nil || e.State().Outcome != OutcomeHalted {
				t.Fatalf("want halted, got %v %s", err, e.State().Outcome)
			}
			if tc.afterHalt != nil {
				h.w.mu.Lock()
				tc.afterHalt(h.w)
				h.w.mu.Unlock()
			}
			err := e.ResumeHalted(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resume: %v, want %q", err, tc.want)
			}
			if e.State().Outcome != OutcomeHalted {
				t.Fatalf("a refused resume changed the run: %s", e.State().Outcome)
			}
		})
	}
}

// The unfence fails: the run halts with the writes held, as a CNPG rollback
// failure does.
func TestExternalUnfenceFailureHalts(t *testing.T) {
	for name, fault := range map[string]func(x *fakeExternal){
		"unfence error":         func(x *fakeExternal) { x.unfenceErr = errors.New("connection refused") },
		"still read-only after": func(x *fakeExternal) { x.stillReadOnlyAfterUnfence = true },
	} {
		t.Run(name, func(t *testing.T) {
			h := newExternalHarness(t)
			h.w.ver.gateOK = false
			fault(h.w.external)
			e := h.engine(t, "")
			err := e.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "rollback failed") {
				t.Fatalf("err = %v", err)
			}
			st := e.State()
			if st.Outcome != OutcomeHalted || st.RollbackError == "" || !st.WritesHeld || !st.SourceFenced {
				t.Fatalf("state %+v", st)
			}
			if contains(h.w.Calls(), "gw.release") {
				t.Fatal("writes released to a source that may be read-only")
			}
		})
	}
}

func TestExternalPrecheck(t *testing.T) {
	for name, tc := range map[string]struct {
		fault func(w *world)
		want  string
	}{
		"source read-only": {func(w *world) { w.external.readOnly = true }, "default_transaction_read_only on"},
		"not superuser":    {func(w *world) { w.external.notSuperuser = true }, "superuser"},
		"alter system off": {func(w *world) { w.external.noAlterSystem = true }, "allow_alter_system"},
		"role override": {func(w *world) { w.external.overrides = []string{"app@*: default_transaction_read_only=off"} },
			"overrides default_transaction_read_only per role"},
		"reindex not an index": {func(w *world) { w.external.unknownIndex = true }, "is not an index on the target"},
		"source in recovery":   {func(w *world) { w.srcDB.recovery = true }, "in recovery"},
		"source unreachable":   {func(w *world) { w.external.statusErr = errors.New("no route to host") }, "source db"},
		"target follows another primary": {func(w *world) { w.dst.st.ReplicaPrimary = "db-gke" },
			`replica.primary="db-gke", want external source "legacy"`},
		"target streams from elsewhere": {func(w *world) { w.dst.st.ReplicaSource = "db-gke" }, "replica.source"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newExternalHarness(t)
			tc.fault(h.w)
			e := h.engine(t, "")
			err := e.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) || e.State().Outcome != OutcomeFailed {
				t.Fatalf("err = %v, outcome %s", err, e.State().Outcome)
			}
			if contains(h.w.Calls(), "gw.hold") {
				t.Fatal("writes held after a failed precheck")
			}
		})
	}
}

// No automatic rollback can promote a CNPG source that does not exist, and
// start-target does not give up on the write budget: failing there halts
// with the writes held, which only makes the hold longer.
func TestExternalNoDemotionRollbackAndNoStartBudget(t *testing.T) {
	h := newExternalHarness(t)
	h.p.Thresholds.MaxDowntime.Duration = 1
	e := h.engine(t, "")
	if err := e.rollback(context.Background()); !errors.Is(err, errNoDemotionRollback) {
		t.Fatalf("rollback: %v", err)
	}
	if err := e.rollbackPromoted(context.Background()); !errors.Is(err, errNoDemotionRollback) {
		t.Fatalf("rollbackPromoted: %v", err)
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ev := e.State().Steps[StepCutover].Evidence; ev["withinBudget"] != false {
		t.Fatalf("cutover evidence: %v", ev)
	}
}

// Before promotion, a resume of a halted run finishes the rollback.
func TestExternalResumeHaltedBeforePromotionRollsBack(t *testing.T) {
	h := newExternalHarness(t)
	h.w.ver.gateOK = false
	h.w.external.unfenceErr = errors.New("connection refused")
	e := h.engine(t, "")
	if err := e.Run(context.Background()); err == nil || e.State().Outcome != OutcomeHalted {
		t.Fatalf("want halted, got %v %s", err, e.State().Outcome)
	}
	h.w.mu.Lock()
	h.w.external.unfenceErr = nil
	h.w.mu.Unlock()
	e2 := h.engine(t, e.RunID())
	if err := e2.ResumeHalted(context.Background()); !errors.Is(err, ErrResumeRolledBack) {
		t.Fatalf("resume: %v", err)
	}
	st := e2.State()
	if st.Outcome != OutcomeRolledBack || st.WritesHeld || st.SourceFenced || st.RollbackError != "" || h.w.external.readOnly {
		t.Fatalf("state %+v readOnly %t", st, h.w.external.readOnly)
	}
	assertOrder(t, h.w.Calls(), "src.fence", "src.unfence", "gw.route legacy", "gw.release")
	if err := e2.Run(context.Background()); err == nil {
		t.Fatal("a rolled-back run ran again")
	}
}

// A process that died inside the fence (writes held and the source fenced,
// neither saved) halts on restart; the resume undoes both.
func TestExternalInterruptedFence(t *testing.T) {
	h := newExternalHarness(t)
	e := h.engine(t, "")
	for _, s := range []string{StepPrecheck, StepPrepare, StepPresync, StepDelta} {
		if err := e.RunStep(context.Background(), s); err != nil {
			t.Fatal(s, err)
		}
	}
	h.w.mu.Lock()
	h.w.gw.held, h.w.external.readOnly = true, true
	h.w.mu.Unlock()
	e.state.Steps[StepFence].Status = StatusRunning
	e.save()

	e2 := h.engine(t, e.RunID())
	if err := e2.Run(context.Background()); err == nil || e2.State().Outcome != OutcomeHalted {
		t.Fatalf("want halted, got %v %s", err, e2.State().Outcome)
	}
	if err := e2.ResumeHalted(context.Background()); !errors.Is(err, ErrResumeRolledBack) {
		t.Fatalf("resume: %v", err)
	}
	if e2.State().Outcome != OutcomeRolledBack || h.w.external.readOnly {
		t.Fatalf("outcome %s readOnly %t", e2.State().Outcome, h.w.external.readOnly)
	}
	calls := h.w.Calls()
	assertOrder(t, calls, "src.unfence", "gw.route legacy", "gw.release")
	if contains(calls, externalPromote) {
		t.Fatal("promoted")
	}
}

// A process that died in start-target after promotion halts on restart; the
// resume repeats REINDEX and the app start on the target.
func TestExternalInterruptedStartTarget(t *testing.T) {
	h := newExternalHarness(t)
	e := h.engine(t, "")
	for _, s := range []string{StepPrecheck, StepPrepare, StepPresync, StepDelta, StepFence, StepSwitchover} {
		if err := e.RunStep(context.Background(), s); err != nil {
			t.Fatal(s, err)
		}
	}
	e.state.Steps[StepStartTarget].Status = StatusRunning
	e.save()

	e2 := h.engine(t, e.RunID())
	if err := e2.Run(context.Background()); err == nil || e2.State().Outcome != OutcomeHalted {
		t.Fatalf("want halted, got %v %s", err, e2.State().Outcome)
	}
	if err := e2.ResumeHalted(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := e2.Run(context.Background()); err != nil {
		t.Fatalf("run after resume: %v", err)
	}
	if e2.State().Outcome != OutcomeSucceeded || len(h.w.reindex) != 2 {
		t.Fatalf("outcome %s reindex %v", e2.State().Outcome, h.w.reindex)
	}
	calls := h.w.Calls()
	assertOrder(t, calls, externalPromote, "reindex public.items_name_idx", "app.scale 2", "gw.route eks", "gw.release")
	if contains(calls, "src.unfence") {
		t.Fatal("unfenced after promotion")
	}
}

// The caller's context ends while the fence hangs: the rollback runs on its
// own context and undoes the fence.
func TestExternalCancelDuringFence(t *testing.T) {
	h := newExternalHarness(t)
	h.w.external.fenceBlocks = true
	e := h.engine(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Cancel at the fence, or after a deadline so the goroutine ends.
		for deadline := time.Now().Add(5 * time.Second); !contains(h.w.Calls(), "src.fence") && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	err := e.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "rolled back after fence") {
		t.Fatalf("err = %v", err)
	}
	if e.State().Outcome != OutcomeRolledBack {
		t.Fatalf("outcome %s", e.State().Outcome)
	}
	assertOrder(t, h.w.Calls(), "gw.hold", "src.fence", "src.unfence", "gw.route legacy", "gw.release")
}
