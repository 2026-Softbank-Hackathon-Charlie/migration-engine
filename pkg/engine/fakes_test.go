package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/files"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/verify"
)

// world is a fake two-cluster CNPG topology with databases that react to
// demote/promote the way CNPG does: demoting the primary publishes a token,
// promoting the replica with that token makes its database writable.
type world struct {
	mu sync.Mutex

	src, dst *fakeCluster
	srcDB    *fakeDB
	dstDB    *fakeDB
	gw       *fakeGateway
	files    *fakeFiles
	ver      *fakeVerifier

	// Fault switches.
	noDemotionToken  bool          // source never publishes a token
	tokenAfter       time.Duration // source publishes the token this long after demotion
	demotedAt        time.Time
	targetNeverWakes bool // target stays in recovery after promotion
	connectErr       error
	restartGets      int // target cluster is unhealthy for this many Gets after promotion
	sourceDownCalls  int // old source is unreachable for this many calls after cutover

	calls []string // ordered log of every side effect, for order assertions
}

func (w *world) record(s string) {
	w.mu.Lock()
	w.calls = append(w.calls, s)
	w.mu.Unlock()
}

func (w *world) Calls() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.calls...)
}

func newWorld() *world {
	w := &world{}
	w.src = &fakeCluster{w: w, role: "src", st: cnpg.Status{
		Phase: cnpg.HealthyPhase, Instances: 2, ReadyInstances: 2,
		Self: "db-gke", ReplicaPrimary: "db-gke", ReplicaSource: "db-eks",
	}, exists: true}
	w.dst = &fakeCluster{w: w, role: "dst", st: cnpg.Status{
		Phase: cnpg.HealthyPhase, Instances: 2, ReadyInstances: 2,
		Self: "db-eks", ReplicaPrimary: "db-gke", ReplicaSource: "db-gke",
	}, exists: true}
	w.srcDB = &fakeDB{w: w, name: "src", pos: 1000}
	w.dstDB = &fakeDB{w: w, name: "dst", pos: 1000, recovery: true}
	w.gw = &fakeGateway{w: w}
	w.files = &fakeFiles{w: w}
	w.ver = &fakeVerifier{w: w, gateOK: true, reportOK: true}
	return w
}

func (w *world) connect(_ context.Context, dsn string) (Database, error) {
	if w.connectErr != nil {
		return nil, w.connectErr
	}
	switch dsn {
	case "src-dsn":
		return w.srcDB, nil
	case "dst-dsn":
		return w.dstDB, nil
	}
	return nil, fmt.Errorf("unknown dsn %q", dsn)
}

// --- cluster ---

type fakeCluster struct {
	w      *world
	role   string
	st     cnpg.Status
	exists bool
	getErr error
	setErr error
	gets   int
}

func (c *fakeCluster) Get(context.Context) (*cnpg.Status, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.gets++
	if c.getErr != nil {
		return nil, c.getErr
	}
	if !c.exists {
		return nil, fmt.Errorf("x: %w", cnpg.ErrNotFound)
	}
	if c.role == "dst" && c.w.restartGets > 0 && c.st.Phase != cnpg.HealthyPhase {
		c.w.restartGets--
		if c.w.restartGets == 0 {
			c.st.Phase = cnpg.HealthyPhase
			c.w.dstDB.pendingRestart = false
			c.w.calls = append(c.w.calls, "dst.restart done")
		}
	}
	if c.role == "src" && c.w.tokenAfter > 0 && !c.w.demotedAt.IsZero() && c.st.ReplicaPrimary != c.st.Self &&
		time.Since(c.w.demotedAt) >= c.w.tokenAfter {
		c.st.DemotionToken = "tok-" + strconv.FormatInt(c.w.srcDB.pos, 10)
	}
	s := c.st
	return &s, nil
}

func (c *fakeCluster) ApplyManifest(_ context.Context, path string) error {
	c.w.record(c.role + ".apply " + path)
	c.w.mu.Lock()
	c.exists = true
	c.w.mu.Unlock()
	return nil
}

func (c *fakeCluster) SetPrimary(_ context.Context, primary, token string) error {
	c.w.record(fmt.Sprintf("%s.setPrimary %s token=%q", c.role, primary, token))
	w := c.w
	w.mu.Lock()
	defer w.mu.Unlock()
	if c.setErr != nil {
		return c.setErr
	}
	c.st.ReplicaPrimary = primary
	switch {
	case c.role == "src" && primary != c.st.Self: // demote
		w.srcDB.recovery = true
		w.demotedAt = time.Now()
		if !w.noDemotionToken && w.tokenAfter == 0 {
			c.st.DemotionToken = "tok-" + strconv.FormatInt(w.srcDB.pos, 10)
		}
	case c.role == "src" && primary == c.st.Self: // re-promote (rollback)
		w.srcDB.recovery = false
		c.st.DemotionToken = ""
	case c.role == "dst" && primary == c.st.Self && token != "": // promote with token
		if !w.targetNeverWakes {
			w.dstDB.recovery = false
		}
		if w.restartGets > 0 {
			c.st.Phase = "Primary instance is being restarted in-place"
			w.dstDB.pendingRestart = true
		}
	}
	return nil
}

func (c *fakeCluster) WaitFor(ctx context.Context, interval time.Duration, cond func(*cnpg.Status) bool) (*cnpg.Status, error) {
	for {
		s, err := c.Get(ctx)
		if err == nil && cond(s) {
			return s, nil
		}
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// --- database ---

// LSNs are plain decimal numbers in the fake.
type fakeDB struct {
	w        *world
	name     string
	pos      int64
	recovery bool
	posErr   error

	pendingRestart bool
}

func (d *fakeDB) InRecovery(context.Context) (bool, error) {
	d.w.mu.Lock()
	defer d.w.mu.Unlock()
	if d.name == "src" && d.w.sourceDownCalls > 0 && d.w.gw.routedTo == "eks" {
		d.w.sourceDownCalls--
		return false, errors.New("the database system is starting up")
	}
	return d.recovery, nil
}

func (d *fakeDB) PendingRestart(context.Context) (bool, error) {
	d.w.mu.Lock()
	defer d.w.mu.Unlock()
	return d.pendingRestart, nil
}

func (d *fakeDB) WALPosition(context.Context) (string, error) {
	d.w.mu.Lock()
	defer d.w.mu.Unlock()
	if d.posErr != nil {
		return "", d.posErr
	}
	return strconv.FormatInt(d.pos, 10), nil
}

func (d *fakeDB) LSNDiff(_ context.Context, a, b string) (int64, error) {
	x, err1 := strconv.ParseInt(a, 10, 64)
	y, err2 := strconv.ParseInt(b, 10, 64)
	if err := errors.Join(err1, err2); err != nil {
		return 0, err
	}
	return x - y, nil
}

func (d *fakeDB) Close() {}

// --- gateway ---

type fakeGateway struct {
	w          *world
	holdErr    error
	releaseErr error
	routeErr   error  // returned for every route call
	routeErrTo string // only fail routes to this backend
	routedTo   string // last successful route, guarded by w.mu
}

func (g *fakeGateway) Health(context.Context) error { g.w.record("gw.health"); return nil }
func (g *fakeGateway) HoldWrites(context.Context) error {
	g.w.record("gw.hold")
	return g.holdErr
}
func (g *fakeGateway) ReleaseWrites(context.Context) error {
	g.w.record("gw.release")
	return g.releaseErr
}
func (g *fakeGateway) Route(_ context.Context, b string) error {
	g.w.record("gw.route " + b)
	if g.routeErr != nil && (g.routeErrTo == "" || g.routeErrTo == b) {
		return g.routeErr
	}
	g.w.mu.Lock()
	g.routedTo = b
	g.w.mu.Unlock()
	return nil
}

// --- files ---

type fakeFiles struct {
	w       *world
	syncErr error
}

func (f *fakeFiles) Available() error { return nil }
func (f *fakeFiles) Sync(context.Context, string, string) (files.SyncResult, error) {
	f.w.record("files.sync")
	return files.SyncResult{}, f.syncErr
}
func (f *fakeFiles) Check(context.Context, string, string) (files.CheckResult, error) {
	return files.CheckResult{OK: true}, nil
}

// --- verifier ---

type fakeVerifier struct {
	w        *world
	gateOK   bool
	reportOK bool
}

func (v *fakeVerifier) FenceGate(context.Context, Database, Database) ([]verify.TableResult, bool, error) {
	v.w.record("verify.gate")
	return []verify.TableResult{{Table: "public.orders", Match: v.gateOK}}, v.gateOK, nil
}

func (v *fakeVerifier) Report(context.Context, Database, Database) (*Report, error) {
	v.w.record("verify.report")
	v.w.mu.Lock()
	down := v.w.sourceDownCalls > 0
	v.w.mu.Unlock()
	if down {
		return nil, errors.New("list tables: source: the database system is starting up")
	}
	r := &Report{OK: v.reportOK}
	if !v.reportOK {
		r.Problems = []string{"3 source rows missing on target"}
	}
	return r, nil
}

// --- helpers ---

func testPlan(t *testing.T, withFiles bool) *plan.Plan {
	t.Helper()
	js := `{
	  "id": "shop-gke-to-eks",
	  "source": {"env":"gke","namespace":"shop","cluster":"db-gke","dsn":"src-dsn"},
	  "target": {"env":"eks","namespace":"shop","cluster":"db-eks","dsn":"dst-dsn","manifest":"target.yaml"},
	  "thresholds": {"catchupTimeout":"2s","switchoverTimeout":"1s","maxDowntime":"3m"}`
	if withFiles {
		js += `, "files": {"source":"src:b","target":"dst:b"}`
	}
	p, err := plan.Parse([]byte(js + "}"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type harness struct {
	w      *world
	p      *plan.Plan
	dir    string
	events []Event
	mu     sync.Mutex
}

func init() { demotionGrace, settleMin = 300*time.Millisecond, 20*time.Millisecond }

func newHarness(t *testing.T) *harness {
	return &harness{w: newWorld(), p: testPlan(t, true), dir: t.TempDir()}
}

func (h *harness) engine(t *testing.T, runID string) *Engine {
	t.Helper()
	e, err := New(h.p, Options{
		StateDir:      h.dir,
		RunID:         runID,
		Emit:          func(ev Event) { h.mu.Lock(); h.events = append(h.events, ev); h.mu.Unlock() },
		Gateway:       h.w.gw,
		SourceCluster: h.w.src,
		TargetCluster: h.w.dst,
		Connect:       h.w.connect,
		Files:         h.w.files,
		Verifier:      h.w.ver,
		PollInterval:  8 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (h *harness) kinds(kind string) []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Event
	for _, ev := range h.events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}
