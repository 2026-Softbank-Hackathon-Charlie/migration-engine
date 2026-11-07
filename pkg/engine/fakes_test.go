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
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/pg"
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
	srcRestartGets   int // source cluster is unhealthy for this many Gets after a rollback re-promotes it
	sourceDownCalls  int // old source is unreachable for this many calls after cutover
	// flakyConnects makes every other connection fail between the target's
	// promotion and the cutover (a port-forward that kept dropping, 2026-11-05).
	flakyConnects bool
	flakyCount    int
	promoted      bool
	// dstConnectErrs fails this many connections to the target first (a new
	// cluster's service not ready yet, platform E2E 2026-11-06).
	dstConnectErrs int
	// afterHalt changes the world between a halt and a resume attempt.
	afterHalt func(w *world)

	// Cold plans (ADR 0012).
	app                   *fakeApp
	prober                *fakeProber
	dstDemoteErr          error // demoting the promoted target fails
	noTargetDemotionToken bool  // the demoted target never publishes a token

	// External source (ADR 0019): the target promotes without a token.
	external   *fakeExternal
	reindex    []string // indexes rebuilt on the target, in order
	reindexErr error

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
	w.app = &fakeApp{w: w, replicas: 2, ready: 0} // deployed, crash-looping on the replica
	w.prober = &fakeProber{w: w}
	return w
}

func (w *world) connect(_ context.Context, dsn string) (Database, error) {
	if w.connectErr != nil {
		return nil, w.connectErr
	}
	w.mu.Lock()
	flaky := w.flakyConnects && w.promoted && w.gw.routedTo != "eks"
	if flaky {
		w.flakyCount++
		flaky = w.flakyCount%2 == 1
	}
	if dsn == "dst-dsn" && w.dstConnectErrs > 0 {
		w.dstConnectErrs--
		flaky = true
	}
	w.mu.Unlock()
	if flaky {
		return nil, errors.New("connection reset by peer")
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
	w        *world
	role     string
	st       cnpg.Status
	exists   bool
	getErr   error
	setErr   error
	applyErr error
	gets     int
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
			c.w.dstDB.restarts++
			c.w.calls = append(c.w.calls, "dst.restart done")
		}
	}
	if c.role == "src" && c.w.srcRestartGets > 0 && c.st.Phase != cnpg.HealthyPhase {
		c.w.srcRestartGets--
		if c.w.srcRestartGets == 0 {
			c.st.Phase = cnpg.HealthyPhase
			c.w.srcDB.pendingRestart = false
			c.w.srcDB.restarts++
			c.w.calls = append(c.w.calls, "src.restart done")
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
	defer c.w.mu.Unlock()
	if c.applyErr != nil {
		return c.applyErr
	}
	c.exists = true
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
		if w.srcRestartGets > 0 {
			c.st.Phase = "Primary instance is being restarted in-place"
			w.srcDB.pendingRestart = true
		}
	case c.role == "dst" && primary == c.st.Self && (token != "" || w.external != nil): // promote (an external source's target needs no token)
		w.promoted = true
		if !w.targetNeverWakes {
			w.dstDB.recovery = false
		}
		if w.restartGets > 0 {
			c.st.Phase = "Primary instance is being restarted in-place"
			w.dstDB.pendingRestart = true
		}
	case c.role == "dst" && primary != c.st.Self: // demote the promoted target (cold rollback)
		if c.w.dstDemoteErr != nil {
			return c.w.dstDemoteErr
		}
		w.dstDB.recovery = true
		if !w.noTargetDemotionToken {
			c.st.DemotionToken = "tokd-" + strconv.FormatInt(w.dstDB.pos, 10)
		}
	}
	return nil
}

// --- target app (cold plans) ---

type fakeApp struct {
	w         *world
	replicas  int64
	ready     int64
	stopErr   error
	scaleErr  error
	neverUp   bool // pods never become ready
	neverDown bool // pods never go away
	runErr    error
	// stopErrFrom makes the n-th and later Stop calls fail (0 = never):
	// 2 lets prepare stop the app and fails the rollback's stop.
	stopErrFrom int
	stops       int
}

func (a *fakeApp) Stop(context.Context) error {
	a.w.record("app.stop")
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	a.stops++
	if a.stopErr != nil {
		return a.stopErr
	}
	if a.stopErrFrom > 0 && a.stops >= a.stopErrFrom {
		return errors.New("deployments.apps \"web\" is forbidden")
	}
	a.replicas, a.ready = 0, 0
	return nil
}

func (a *fakeApp) Scale(_ context.Context, n int64) error {
	a.w.record(fmt.Sprintf("app.scale %d", n))
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	if a.scaleErr != nil {
		return a.scaleErr
	}
	a.replicas = n
	if !a.neverUp {
		a.ready = n
	}
	return nil
}

// Ready: like an app that migrates its schema on start, pods are never ready
// while the target database is still a read-only replica.
func (a *fakeApp) Ready(context.Context) (int64, error) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	if a.w.dstDB.recovery {
		return 0, nil
	}
	return a.ready, nil
}

func (a *fakeApp) Running(context.Context) (int64, error) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	if a.runErr != nil {
		return 0, a.runErr
	}
	if a.neverDown {
		return 1, nil
	}
	return a.replicas, nil
}

type fakeProber struct {
	w      *world
	status int // returned for every request; 0 means 200
	err    error
	// The target app's answers depend on the database: it fails while the
	// target is still a replica (the app's start-up schema change cannot run).
	urls []string
}

func (p *fakeProber) Probe(_ context.Context, method, url string) (int, error) {
	p.w.record("probe " + method + " " + url)
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	p.urls = append(p.urls, url)
	if p.err != nil {
		return 0, p.err
	}
	if p.w.dstDB.recovery {
		return 503, nil
	}
	if p.status != 0 {
		return p.status, nil
	}
	return 200, nil
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
	// posErrCalls makes this many WALPosition calls fail before they succeed
	// (a connection blip while measuring lag).
	posErrCalls int

	pendingRestart bool
	restarts       int // the server start time moves with each restart
	// silentRestartAt restarts the server (a new start time, nothing else
	// visible) on this PostmasterStart call; 0 never.
	silentRestartAt int
	pmCalls         int
}

func (d *fakeDB) PostmasterStart(context.Context) (time.Time, error) {
	d.w.mu.Lock()
	defer d.w.mu.Unlock()
	d.pmCalls++
	if d.silentRestartAt > 0 && d.pmCalls == d.silentRestartAt {
		d.restarts++
	}
	return time.Date(2026, 11, 6, 0, 0, d.restarts, 0, time.UTC), nil
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
	if d.posErrCalls > 0 {
		d.posErrCalls--
		return "", errors.New("connection reset by peer")
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

func (d *fakeDB) IsIndex(context.Context, string) (bool, error) {
	d.w.mu.Lock()
	defer d.w.mu.Unlock()
	return d.w.external == nil || !d.w.external.unknownIndex, nil
}

func (d *fakeDB) ReindexIndex(_ context.Context, name string) error {
	d.w.record("reindex " + name)
	d.w.mu.Lock()
	defer d.w.mu.Unlock()
	if d.w.reindexErr != nil {
		return d.w.reindexErr
	}
	d.w.reindex = append(d.w.reindex, name)
	return nil
}

func (d *fakeDB) Close() {}

// --- external source (ADR 0019) ---

// fakeExternal is a PostgreSQL primary outside CNPG; its WAL position is the
// world's srcDB.
type fakeExternal struct {
	w          *world
	readOnly   bool
	nextXid    int
	statusErr  error
	fenceErr   error
	unfenceErr error
	// fenceAppliedErr: the fence is applied but its answer is an error.
	fenceAppliedErr error
	// fenceBlocks: the fence hangs until its context ends.
	fenceBlocks bool
	// writeSessions: client sessions holding an xid at the re-check.
	writeSessions int
	// stillReadOnlyAfterUnfence: the unfence never reaches new sessions.
	stillReadOnlyAfterUnfence   bool
	notSuperuser, noAlterSystem bool
	overrides                   []string
	unknownIndex                bool // IsIndex is false on the target
}

func (x *fakeExternal) Status(context.Context) (ExternalStatus, error) {
	x.w.mu.Lock()
	defer x.w.mu.Unlock()
	if x.statusErr != nil {
		return ExternalStatus{}, x.statusErr
	}
	return ExternalStatus{InRecovery: x.w.srcDB.recovery, ReadOnly: x.readOnly, NextXid: strconv.Itoa(x.nextXid),
		WriteSessions: x.writeSessions, ReadOnlyOverrides: x.overrides,
		Superuser: !x.notSuperuser, AllowAlterSystem: !x.noAlterSystem}, nil
}

func (x *fakeExternal) Fence(ctx context.Context) (pg.Fence, error) {
	x.w.record("src.fence")
	if x.fenceBlocks {
		<-ctx.Done()
		return pg.Fence{}, ctx.Err()
	}
	x.w.mu.Lock()
	defer x.w.mu.Unlock()
	if x.fenceErr != nil {
		return pg.Fence{}, x.fenceErr
	}
	x.readOnly = true
	f := pg.Fence{LSN: strconv.FormatInt(x.w.srcDB.pos, 10), NextXid: strconv.Itoa(x.nextXid), Terminated: 3}
	if x.fenceAppliedErr != nil {
		return pg.Fence{}, x.fenceAppliedErr
	}
	return f, nil
}

func (x *fakeExternal) Unfence(context.Context) error {
	x.w.record("src.unfence")
	x.w.mu.Lock()
	defer x.w.mu.Unlock()
	if x.unfenceErr != nil {
		return x.unfenceErr
	}
	if !x.stillReadOnlyAfterUnfence {
		x.readOnly = false
	}
	return nil
}

// --- gateway ---

type fakeGateway struct {
	w          *world
	healthErr  error
	holdErr    error
	releaseErr error
	routeErr   error  // returned for every route call
	routeErrTo string // only fail routes to this backend
	routedTo   string // last successful route, guarded by w.mu
	held       bool   // writes held (no release since), guarded by w.mu
}

func (g *fakeGateway) Health(context.Context) error { g.w.record("gw.health"); return g.healthErr }
func (g *fakeGateway) HoldWrites(context.Context) error {
	g.w.record("gw.hold")
	if g.holdErr == nil {
		g.w.mu.Lock()
		g.held = true
		g.w.mu.Unlock()
	}
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
	// checkErr fails rclone check (a remote cannot be read); checkMissing
	// reports objects missing on the target.
	checkErr     error
	checkMissing int
}

func (f *fakeFiles) Available() error { return nil }
func (f *fakeFiles) Sync(context.Context, string, string) (files.SyncResult, error) {
	f.w.record("files.sync")
	return files.SyncResult{}, f.syncErr
}
func (f *fakeFiles) Check(context.Context, string, string) (files.CheckResult, error) {
	if f.checkErr != nil {
		return files.CheckResult{}, f.checkErr
	}
	return files.CheckResult{OK: f.checkMissing == 0, Missing: f.checkMissing}, nil
}

// --- verifier ---

type fakeVerifier struct {
	w        *world
	gateOK   bool
	reportOK bool
	delay    time.Duration // how long the fence gate rehearsal takes
}

func (v *fakeVerifier) FenceGate(context.Context, Database, Database) ([]verify.TableResult, bool, error) {
	v.w.mu.Lock()
	held := v.w.gw.held
	v.w.mu.Unlock()
	if !held { // delta's timing rehearsal (ADR 0013)
		v.w.record("verify.gate.rehearsal")
		time.Sleep(v.delay)
		return []verify.TableResult{{Table: "public.orders", Match: true}}, true, nil
	}
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

func init() {
	demotionGrace, settleMin, rollbackSlack = 300*time.Millisecond, 20*time.Millisecond, 2*time.Second
}

func newHarness(t *testing.T) *harness {
	return &harness{w: newWorld(), p: testPlan(t, true), dir: t.TempDir()}
}

// coldPlan is testPlan with the gateway and a cold target app (ADR 0012).
func coldPlan(t *testing.T) *plan.Plan {
	t.Helper()
	p, err := plan.Parse([]byte(`{
	  "id": "shop-gke-to-eks",
	  "source": {"env":"gke","namespace":"shop","cluster":"db-gke","dsn":"src-dsn"},
	  "target": {"env":"eks","namespace":"shop","cluster":"db-eks","dsn":"dst-dsn","manifest":"target.yaml",
	    "app": {"mode":"cold","kind":"Deployment","name":"web","replicas":2,"startTimeout":"500ms",
	      "smokeUrl":"http://web.shop.svc:8080","smoke":[{"method":"GET","path":"/healthz"}]}},
	  "gateway": {"mode":"http","url":"http://gw:9901"},
	  "files": {"source":"src:b","target":"dst:b"},
	  "thresholds": {"catchupTimeout":"2s","switchoverTimeout":"1s","maxDowntime":"3m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newColdHarness(t *testing.T) *harness {
	return &harness{w: newWorld(), p: coldPlan(t), dir: t.TempDir()}
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
		App:           h.w.app,
		Prober:        h.w.prober,
		External:      h.external(),
		PollInterval:  8 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// external keeps the Options field a nil interface when the world has no
// external source.
func (h *harness) external() ExternalSource {
	if h.w.external == nil {
		return nil
	}
	return h.w.external
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
