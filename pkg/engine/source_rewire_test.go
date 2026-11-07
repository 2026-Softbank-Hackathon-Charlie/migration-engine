package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
)

type rewireTestCluster struct {
	*fakeCluster
	patches  int
	before   func()
	after    func()
	err      error
	errAfter error
}

func (c *rewireTestCluster) RewireSource(ctx context.Context, expected *cnpg.Status, target string) error {
	if c.before != nil {
		c.before()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.patches++
	if c.err != nil {
		return c.err
	}
	c.w.mu.Lock()
	if c.st.UID != expected.UID || c.st.ResourceVersion != expected.ResourceVersion || c.st.ReplicaPrimary != expected.ReplicaPrimary {
		c.w.mu.Unlock()
		return errors.New("source observation changed")
	}
	c.w.calls = append(c.w.calls, "src.rewire "+target)
	c.st.ReplicaSource = target
	c.st.ResourceVersion += "-next"
	c.w.mu.Unlock()
	if c.after != nil {
		c.after()
	}
	return c.errAfter
}

func newRewireHarness(t *testing.T) (*harness, *Engine, *rewireTestCluster) {
	t.Helper()
	h := newHarness(t)
	h.p.AllowSourceRewire = true
	h.p.Target.Env, h.p.Target.Cluster, h.p.Gateway.TargetBackend = "onprem", "db-onprem", "onprem"
	h.w.src.st.UID, h.w.src.st.ResourceVersion = "src-uid", "10"
	h.w.dst.st.UID, h.w.dst.st.ResourceVersion = "dst-uid", "20"
	h.w.src.st.ExternalClusters = []string{"db-eks", "db-onprem"}
	h.w.dst.st.Self = "db-onprem"
	h.w.dst.st.ExternalClusters = []string{"db-gke", "db-eks"}
	e := h.engine(t, "")
	c := &rewireTestCluster{fakeCluster: h.w.src}
	e.src = c
	return h, e, c
}

func TestSourceRewirePrecheckReadOnlyThenPrepareBeforeFence(t *testing.T) {
	h, e, c := newRewireHarness(t)
	if _, err := e.precheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.patches != 0 || h.w.src.st.ReplicaSource != "db-eks" {
		t.Fatal("precheck changed source topology")
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.patches != 1 || e.State().Outcome != OutcomeSucceeded {
		t.Fatalf("patches=%d state=%+v", c.patches, e.State())
	}
	assertOrder(t, h.w.Calls(), "src.rewire db-onprem", "gw.hold", `src.setPrimary db-onprem token=""`, `dst.setPrimary db-onprem token="tok-1000"`, "gw.route onprem", "gw.release")
	if e.State().Steps[StepPrepare].Evidence["sourceRewire"] == nil {
		t.Fatal("prepare omitted source selection evidence")
	}
}

func TestSourceRewirePreservesLegacyAndAlreadySelectedPlans(t *testing.T) {
	for _, tc := range []struct {
		name     string
		optIn    bool
		selected bool
		wantErr  bool
	}{
		{"legacy pair", false, true, false},
		{"legacy rejects third target", false, false, true},
		{"opt-in already selected", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, e, c := newRewireHarness(t)
			h.p.AllowSourceRewire = tc.optIn
			if tc.selected {
				h.w.src.st.ReplicaSource = h.w.dst.st.Self
			}
			err := e.Run(context.Background())
			if (err != nil) != tc.wantErr || c.patches != 0 {
				t.Fatalf("err=%v patches=%d", err, c.patches)
			}
		})
	}
}

func TestSourceRewireRejectsUnsafePrecheckWithoutMutation(t *testing.T) {
	for _, fault := range []string{"source unhealthy", "source replica", "source recovery", "missing source external", "missing target external", "target primary", "target cascading", "target writable", "source identity missing", "unsupported adapter"} {
		t.Run(fault, func(t *testing.T) {
			h, e, c := newRewireHarness(t)
			switch fault {
			case "source unhealthy":
				h.w.src.st.ReadyInstances = 0
			case "source replica":
				h.w.src.st.ReplicaPrimary = "db-eks"
			case "source recovery":
				h.w.srcDB.recovery = true
			case "missing source external":
				h.w.src.st.ExternalClusters = []string{"db-eks"}
			case "missing target external":
				h.w.dst.st.ExternalClusters = nil
			case "target primary":
				h.w.dst.st.ReplicaPrimary = "db-onprem"
			case "target cascading":
				h.w.dst.st.ReplicaSource = "db-eks"
			case "target writable":
				h.w.dstDB.recovery = false
			case "source identity missing":
				h.w.src.st.UID = ""
			case "unsupported adapter":
				e.src = h.w.src
			}
			if err := e.Run(context.Background()); err == nil {
				t.Fatal("unsafe plan succeeded")
			}
			if c.patches != 0 || contains(h.w.Calls(), "gw.hold") || e.State().FailureStep != StepPrecheck {
				t.Fatalf("mutation before refusal: patches=%d state=%+v calls=%v", c.patches, e.State(), h.w.Calls())
			}
		})
	}
}

func TestSourceRewireRechecksPrepareAndStopsBeforeFence(t *testing.T) {
	for _, fault := range []string{"source recovery", "target writable", "target cascading", "source role changed", "source version race", "API denial", "API unknown outcome", "target identity changed after patch", "target topology changed after patch", "target writable after patch", "source recovery after patch"} {
		t.Run(fault, func(t *testing.T) {
			h, e, c := newRewireHarness(t)
			if _, err := e.precheck(context.Background()); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "source recovery":
				h.w.srcDB.recovery = true
			case "target writable":
				h.w.dstDB.recovery = false
			case "target cascading":
				h.w.dst.st.ReplicaSource = "db-eks"
			case "source role changed":
				h.w.src.st.ReplicaPrimary = "db-eks"
			case "source version race":
				c.before = func() { h.w.src.st.ResourceVersion = "concurrent-version" }
			case "API denial":
				c.err = errors.New("admission denied")
			case "API unknown outcome":
				c.err = context.DeadlineExceeded
			case "target identity changed after patch":
				c.after = func() { h.w.dst.st.UID = "replacement" }
			case "target topology changed after patch":
				c.after = func() { h.w.dst.st.ReplicaSource = "db-eks" }
			case "target writable after patch":
				c.after = func() { h.w.dstDB.recovery = false }
			case "source recovery after patch":
				c.after = func() { h.w.srcDB.recovery = true }
			}
			if _, err := e.prepare(context.Background()); err == nil {
				t.Fatal("unsafe prepare succeeded")
			}
			if contains(h.w.Calls(), "gw.hold") || h.w.src.st.ReplicaPrimary != "db-gke" && fault != "source role changed" {
				t.Fatalf("prepare crossed role/fence boundary: %v", h.w.Calls())
			}
			for _, call := range h.w.Calls() {
				if strings.Contains(call, "setPrimary") {
					t.Fatalf("source selection changed primary: %s", call)
				}
			}
		})
	}
}

func TestSourceRewireCancellationAndRetry(t *testing.T) {
	for _, afterPatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelAfterPatch=%v", afterPatch), func(t *testing.T) {
			h, e, c := newRewireHarness(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if afterPatch {
				c.after = cancel
			} else {
				cancel()
			}
			if _, err := e.prepare(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled prepare err=%v", err)
			}
			if contains(h.w.Calls(), "gw.hold") {
				t.Fatal("cancelled preparation held writes")
			}
			if afterPatch {
				if c.patches != 1 || h.w.srcDB.recovery || h.w.src.st.ReplicaPrimary != "db-gke" {
					t.Fatal("cancel after selection changed source role")
				}
				c.after = nil
				if _, err := e.prepare(context.Background()); err != nil {
					t.Fatal(err)
				}
				if c.patches != 1 {
					t.Fatal("retry repeated an already observed metadata selection")
				}
			} else if c.patches != 0 {
				t.Fatal("cancel before preparation patched source")
			}
		})
	}
}

func TestSourceRewireUnknownAppliedOutcomeStopsBeforeFenceAndRetryObserves(t *testing.T) {
	h, e, c := newRewireHarness(t)
	c.errAfter = context.DeadlineExceeded
	if err := e.Run(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unknown patch outcome err=%v", err)
	}
	st := e.State()
	if st.Outcome != OutcomeFailed || st.FailureStep != StepPrepare || st.WritesHeld || st.PromoteRequested {
		t.Fatalf("crossed cutover boundary: %+v", st)
	}
	if c.patches != 1 || h.w.src.st.ReplicaSource != "db-onprem" || h.w.srcDB.recovery || contains(h.w.Calls(), "gw.hold") {
		t.Fatalf("unknown metadata choice changed roles or held writes: %v", h.w.Calls())
	}
	c.errAfter = nil
	if err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.patches != 1 || e.State().Outcome != OutcomeSucceeded {
		t.Fatal("retry did not observe already applied source choice")
	}
}
