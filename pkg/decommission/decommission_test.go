package decommission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/kube"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// --- fakes ---

type fakeObj struct {
	labels   string // "app" or "db"
	replicas int64
	running  int64
	stopped  bool
}

// world is a fake source namespace, old database and gateway.
type world struct {
	mu        sync.Mutex
	objs      map[kube.Resource]*fakeObj
	blocked   map[string]bool
	sessions  int  // client sessions on the old database while the old app is stopped
	enforced  bool // the network plugin enforces NetworkPolicy
	srcReqs   int64
	smokeCode int
	stopErr   error
	appear    *kube.Resource // created after the inventory
	listCalls int
}

func newWorld() *world {
	return &world{
		objs: map[kube.Resource]*fakeObj{
			{Kind: "Deployment", Name: "shop"}:                {labels: "app", replicas: 2, running: 2},
			{Kind: "CronJob", Name: "report"}:                 {labels: "app"},
			{Kind: "Service", Name: "shop"}:                   {labels: "app"},
			{Kind: "ConfigMap", Name: "shop-config"}:          {labels: "app"},
			{Kind: "Secret", Name: "pg-old-app"}:              {labels: "db"},
			{Kind: "Service", Name: "pg-old-rw"}:              {labels: "db"},
			{Kind: "PersistentVolumeClaim", Name: "pg-old-1"}: {labels: "db"},
			{Kind: "Cluster", Name: "pg-old"}:                 {},
		},
		blocked:   map[string]bool{},
		enforced:  true,
		smokeCode: 200,
	}
}

func (w *world) List(_ context.Context, selector string) ([]kube.Resource, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.listCalls++
	if w.appear != nil && w.listCalls > 2 {
		w.objs[*w.appear] = &fakeObj{labels: "app"}
	}
	want := "app"
	if strings.HasPrefix(selector, "cnpg.io/cluster=") {
		want = "db"
	}
	var out []kube.Resource
	for r, o := range w.objs {
		if o.labels == want {
			out = append(out, r)
		}
	}
	return out, nil
}

func (w *world) Exists(_ context.Context, r kube.Resource) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.objs[r]
	return ok, nil
}

func (w *world) Stop(_ context.Context, r kube.Resource) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopErr != nil {
		return w.stopErr
	}
	o := w.objs[r]
	o.stopped, o.running = true, 0
	return nil
}

func (w *world) Restore(_ context.Context, r kube.Resource) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	o := w.objs[r]
	o.stopped, o.running = false, o.replicas
	return nil
}

func (w *world) Running(_ context.Context, r kube.Resource) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.objs[r].running, nil
}

func (w *world) Block(_ context.Context, name, _ string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.blocked[name] = true
	return nil
}

func (w *world) Unblock(_ context.Context, name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.blocked, name)
	return nil
}

func (w *world) appStopped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.objs[kube.Resource{Kind: "Deployment", Name: "shop"}].stopped
}

type fakeDB struct{ w *world }

func (d fakeDB) ClientSessions(context.Context) (int, error) {
	d.w.mu.Lock()
	defer d.w.mu.Unlock()
	return d.w.sessions, nil
}
func (fakeDB) Close() {}

func (w *world) connect(context.Context, string) (SourceDB, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.enforced && w.blocked[PolicyDB] {
		return nil, errors.New("dial tcp 10.0.0.5:5432: i/o timeout")
	}
	return fakeDB{w}, nil
}

type fakeTraffic struct{ w *world }

func (t fakeTraffic) Totals(context.Context) (map[string]gateway.Totals, error) {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	n := t.w.srcReqs
	t.w.srcReqs *= 2 // grows between the two reads when non-zero
	return map[string]gateway.Totals{"old": {Requests: n}, "new": {Requests: 1000}}, nil
}

func (w *world) probe(context.Context, string, string) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.smokeCode, nil
}

// --- harness ---

func testPlan(t *testing.T) *plan.Plan {
	t.Helper()
	p, err := plan.Parse([]byte(`{
	  "id": "shop-old-to-new",
	  "source": {"env":"old","namespace":"shop","cluster":"pg-old","dsn":"postgres://app@old/app"},
	  "target": {"env":"new","namespace":"shop","cluster":"pg-new","dsn":"postgres://app@new/app"},
	  "decommission": {"selector":"app.kubernetes.io/part-of=shop","observe":"30ms",
	    "smokeUrl":"http://gw.example:8000","smoke":[{"method":"GET","path":"/"},{"method":"POST","path":"/orders"}]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func writeRun(t *testing.T, dir, outcome string) {
	t.Helper()
	b := `{"runId":"run-1","planId":"shop-old-to-new","outcome":"` + outcome + `","steps":{}}`
	if err := os.WriteFile(filepath.Join(dir, "run-1.json"), []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runner(t *testing.T, w *world, withTraffic bool) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	writeRun(t, dir, engine.OutcomeSucceeded)
	var tr Traffic = gateway.None{}
	if withTraffic {
		tr = fakeTraffic{w}
	}
	p := testPlan(t)
	p.Gateway.SourceBackend = "old"
	r, err := New(p, Options{StateDir: dir, RunID: "run-1", Inventory: w, Connect: w.connect, Traffic: tr,
		Probe: w.probe, PollInterval: 5 * time.Millisecond, BlockCheckTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return r, dir
}

// --- tests ---

func TestDecommissionSucceeds(t *testing.T) {
	w := newWorld()
	r, dir := runner(t, w, true)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := r.State()
	if st.Outcome != OutcomeSucceeded || st.Report == nil || !st.Report.Passed {
		t.Fatalf("outcome %s, report %+v", st.Outcome, st.Report)
	}
	want := Counts{Stopped: 2, Retained: 2, AwaitingApproval: 2}
	if st.Report.Counts != want {
		t.Errorf("counts = %+v, want %+v (items %+v)", st.Report.Counts, want, st.Report.Items)
	}
	if o := st.Report.Observe; o == nil || o.MaxSourceSessions == nil || *o.MaxSourceSessions != 0 ||
		o.SourceBackendRequests == nil || *o.SourceBackendRequests != 0 || len(o.NotMeasured) != 0 {
		t.Errorf("observe = %+v", o)
	}
	if b := st.Report.Block; b == nil || !b.Verified {
		t.Errorf("block = %+v", b)
	}
	if !w.blocked[PolicyApp] || !w.blocked[PolicyDB] || !w.appStopped() {
		t.Error("source should stay blocked and the old app stopped after success")
	}
	if _, err := os.Stat(filepath.Join(dir, "run-1.decommission.json")); err != nil {
		t.Errorf("state not saved: %v", err)
	}
	// CNPG-made objects of the retained database are not listed separately.
	for _, it := range st.Report.Items {
		if it.Name == "pg-old-app" || it.Name == "pg-old-rw" {
			t.Errorf("CNPG-managed %s/%s should not be an item", it.Kind, it.Name)
		}
	}
}

func TestFailuresPutTheSourceBack(t *testing.T) {
	cases := []struct {
		name  string
		fault func(*world)
		step  string
	}{
		{"new environment still uses the old database", func(w *world) { w.sessions = 3 }, StepObserve},
		{"gateway still sends traffic to the source", func(w *world) { w.srcReqs = 5 }, StepObserve},
		{"network plugin ignores NetworkPolicy", func(w *world) { w.enforced = false }, StepBlock},
		{"new environment fails without the source", func(w *world) { w.smokeCode = 502 }, StepSmoke},
		{"a resource appeared during decommission", func(w *world) { w.appear = &kube.Resource{Kind: "Deployment", Name: "shop-v2"} }, StepReconcile},
		{"old app cannot be stopped", func(w *world) { w.stopErr = errors.New("forbidden") }, StepStop},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld()
			c.fault(w)
			r, _ := runner(t, w, true)
			err := r.Run(context.Background())
			if err == nil {
				t.Fatal("expected failure")
			}
			st := r.State()
			if st.Outcome != OutcomeFailed || st.FailureStep != c.step || st.UndoError != "" {
				t.Fatalf("outcome %s at %s (undo %q), want failed at %s: %v", st.Outcome, st.FailureStep, st.UndoError, c.step, err)
			}
			if len(w.blocked) != 0 || len(st.Blocked) != 0 {
				t.Errorf("block left in place: %v", w.blocked)
			}
			if w.appStopped() || len(st.Stopped) != 0 {
				t.Error("old app not restored")
			}
		})
	}
}

// Without a gateway the request count is unknown, not zero.
func TestTrafficNotMeasuredWithoutGateway(t *testing.T) {
	w := newWorld()
	r, _ := runner(t, w, false)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	o := r.State().Report.Observe
	if o.SourceBackendRequests != nil || len(o.NotMeasured) == 0 {
		t.Errorf("observe = %+v; want requests unmeasured and said so", o)
	}
}

func TestUndoRestoresAfterSuccess(t *testing.T) {
	w := newWorld()
	r, _ := runner(t, w, true)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Undo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := r.State(); st.Outcome != OutcomeUndone || len(st.Blocked) != 0 || len(st.Stopped) != 0 {
		t.Fatalf("state after undo = %s blocked=%v stopped=%v", st.Outcome, st.Blocked, st.Stopped)
	}
	if len(w.blocked) != 0 || w.appStopped() {
		t.Error("source still blocked or old app still stopped")
	}
}

func TestOnlySucceededMigrations(t *testing.T) {
	for _, outcome := range []string{engine.OutcomeRunning, engine.OutcomeRolledBack, engine.OutcomeHalted, engine.OutcomeFailed} {
		dir := t.TempDir()
		writeRun(t, dir, outcome)
		w := newWorld()
		_, err := New(testPlan(t), Options{StateDir: dir, RunID: "run-1", Inventory: w, Connect: w.connect, Traffic: gateway.None{}, Probe: w.probe})
		if err == nil {
			t.Errorf("decommission allowed after a %s migration", outcome)
		}
	}
	w := newWorld()
	if _, err := New(testPlan(t), Options{StateDir: t.TempDir(), RunID: "../x", Inventory: w}); err == nil {
		t.Error("path-like run id accepted")
	}
}

func TestSelectorWithoutWorkloadFails(t *testing.T) {
	w := newWorld()
	for r, o := range w.objs {
		if r.Kind == "Deployment" || r.Kind == "CronJob" {
			o.labels = "other"
		}
	}
	r, _ := runner(t, w, true)
	if err := r.Run(context.Background()); err == nil || r.State().FailureStep != StepInventory {
		t.Fatalf("want inventory failure, got %v", err)
	}
}

func TestResumeSkipsFinishedSteps(t *testing.T) {
	w := newWorld()
	w.smokeCode = 0 // first attempt: smoke fails, everything is put back
	r, dir := runner(t, w, true)
	_ = r.Run(context.Background())
	w.smokeCode = 200
	r2, err := New(r.plan, Options{StateDir: dir, RunID: "run-1", Inventory: w, Connect: w.connect, Traffic: fakeTraffic{w},
		Probe: w.probe, PollInterval: 5 * time.Millisecond, BlockCheckTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.Run(context.Background()); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if !r2.State().Report.Passed {
		t.Error("retry did not pass")
	}
}

func TestRedact(t *testing.T) {
	got := redact(`Get "http://user:secret@gw:8000/x": dial error`)
	if strings.Contains(got, "secret") {
		t.Errorf("not redacted: %s", got)
	}
}
