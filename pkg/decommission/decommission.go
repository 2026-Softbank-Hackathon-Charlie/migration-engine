// Package decommission retires the source environment after a successful
// migration and decides, with evidence, whether the migration is complete:
//
//	inventory → stop → observe → block → smoke → reconcile
//
// It stops the old app, watches for anything still using the source, blocks
// the source, checks the new environment still serves users, and reports what
// is left: stopped, intentionally retained, awaiting deletion approval, or
// unprocessed. Nothing is deleted. The old database stays a replica of the new
// primary so a reverse migration remains possible; Undo lifts the block and
// restarts the old app. See docs/adr/0010.
package decommission

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/kube"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/pg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// Steps, in order.
const (
	StepInventory = "inventory"
	StepStop      = "stop"
	StepObserve   = "observe"
	StepBlock     = "block"
	StepSmoke     = "smoke"
	StepReconcile = "reconcile"
)

// Steps is the fixed order.
var Steps = []string{StepInventory, StepStop, StepObserve, StepBlock, StepSmoke, StepReconcile}

// Outcomes.
const (
	OutcomeRunning   = "running"
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed" // block lifted and old app restored, unless UndoError says otherwise
	OutcomeUndone    = "undone"
)

// Planned actions and final statuses of a resource.
const (
	ActionStop             = "stop"
	ActionRetain           = "retain"
	ActionAwaitApproval    = "await_approval"
	StatusStopped          = "stopped"
	StatusRetained         = "retained"
	StatusAwaitingApproval = "awaiting_approval"
	StatusRemoved          = "removed"
	StatusUnprocessed      = "unprocessed"
)

// NetworkPolicy names made by the block step.
const (
	PolicyApp = "hopcharlie-decommission-app"
	PolicyDB  = "hopcharlie-decommission-db"
)

// Inventory is the source namespace (*kube.Inventory).
type Inventory interface {
	List(ctx context.Context, selector string) ([]kube.Resource, error)
	Exists(ctx context.Context, r kube.Resource) (bool, error)
	Stop(ctx context.Context, r kube.Resource) error
	Restore(ctx context.Context, r kube.Resource) error
	Running(ctx context.Context, r kube.Resource) (int64, error)
	Block(ctx context.Context, name, podSelector string) error
	Unblock(ctx context.Context, name string) error
}

// SourceDB is a connection to the old database (*pg.DB).
type SourceDB interface {
	ClientSessions(ctx context.Context) (int, error)
	Close()
}

// Connector opens the old database.
type Connector func(ctx context.Context, dsn string) (SourceDB, error)

// Traffic reports per-backend request totals (*gateway.HTTP, gateway.None).
type Traffic interface {
	Totals(ctx context.Context) (map[string]gateway.Totals, error)
}

// Prober sends one smoke request and returns the status code.
type Prober func(ctx context.Context, method, url string) (int, error)

// Options configure a Runner. Zero values use the real adapters.
type Options struct {
	StateDir     string // where the migration run state lives; default .hopcharlie/runs
	RunID        string // the migration run to decommission
	Emit         func(engine.Event)
	Inventory    Inventory
	Connect      Connector
	Traffic      Traffic
	Probe        Prober
	PollInterval time.Duration // default 2s
	// BlockCheckTimeout bounds how long the block step waits for the source
	// database to refuse connections. Default 30s.
	BlockCheckTimeout time.Duration
}

// Runner executes the decommission of one migration run.
type Runner struct {
	plan    *plan.Plan
	spec    *plan.DecommissionSpec
	opt     Options
	path    string
	mu      sync.Mutex
	state   *State
	emitFn  func(engine.Event)
	inv     Inventory
	connect Connector
	traffic Traffic
	probe   Prober
}

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// New prepares the decommission of a run that migrated successfully.
// It resumes an earlier decommission of the same run if one exists.
func New(p *plan.Plan, o Options) (*Runner, error) {
	if p.Decommission == nil {
		return nil, errors.New("plan has no decommission section")
	}
	if !runIDPattern.MatchString(o.RunID) || strings.Contains(o.RunID, "..") {
		return nil, fmt.Errorf("invalid run id %q", o.RunID)
	}
	if o.StateDir == "" {
		o.StateDir = filepath.Join(".hopcharlie", "runs")
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 2 * time.Second
	}
	if o.BlockCheckTimeout <= 0 {
		o.BlockCheckTimeout = 30 * time.Second
	}
	mig, err := engine.ReadState(o.StateDir, o.RunID)
	if err != nil {
		return nil, fmt.Errorf("migration run %s: %w", o.RunID, err)
	}
	if mig.PlanID != p.ID {
		return nil, fmt.Errorf("run %s belongs to plan %q, not %q", o.RunID, mig.PlanID, p.ID)
	}
	if mig.Outcome != engine.OutcomeSucceeded {
		return nil, fmt.Errorf("run %s is %q; only a succeeded migration can be decommissioned", o.RunID, mig.Outcome)
	}

	r := &Runner{plan: p, spec: p.Decommission, opt: o, emitFn: o.Emit,
		inv: o.Inventory, connect: o.Connect, traffic: o.Traffic, probe: o.Probe}
	if r.inv == nil {
		dyn, err := kube.Dynamic(p.Source.Kubeconfig, p.Source.Context)
		if err != nil {
			return nil, fmt.Errorf("source cluster: %w", err)
		}
		r.inv = &kube.Inventory{Dyn: dyn, Namespace: p.Source.Namespace, OperatorNamespace: "cnpg-system"}
	}
	if r.connect == nil {
		r.connect = func(ctx context.Context, dsn string) (SourceDB, error) { return pg.Connect(ctx, dsn) }
	}
	if r.traffic == nil {
		if p.Gateway.Mode == "http" {
			r.traffic = gateway.NewHTTP(p.Gateway.URL, p.Gateway.Token)
		} else {
			r.traffic = gateway.None{}
		}
	}
	if r.probe == nil {
		client := &http.Client{Timeout: 10 * time.Second}
		r.probe = func(ctx context.Context, method, u string) (int, error) {
			req, err := http.NewRequestWithContext(ctx, method, u, nil)
			if err != nil {
				return 0, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return 0, err
			}
			resp.Body.Close()
			return resp.StatusCode, nil
		}
	}

	r.path = filepath.Join(o.StateDir, o.RunID+".decommission.json")
	st, err := loadState(r.path)
	switch {
	case err == nil:
		r.state = st
	case errors.Is(err, os.ErrNotExist):
		r.state = newState(o.RunID, p.ID)
	default:
		return nil, err
	}
	return r, nil
}

// State returns a copy of the decommission state.
func (r *Runner) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.clone()
}

// Run executes the remaining steps. On failure it lifts the block and restarts
// the old app before returning.
func (r *Runner) Run(ctx context.Context) error {
	switch r.state.Outcome {
	case OutcomeSucceeded:
		return nil
	case OutcomeFailed, OutcomeUndone:
		r.update(func(s *State) { s.reset() }) // start over; everything was put back
	}
	for _, step := range Steps {
		if r.state.Steps[step].Status == engine.StatusSucceeded {
			continue
		}
		if err := r.runStep(ctx, step); err != nil {
			return r.fail(step, err)
		}
	}
	r.update(func(s *State) { s.Outcome = OutcomeSucceeded })
	rep := r.State().Report
	r.emit("", engine.EventDone, "decommission complete", map[string]any{"passed": rep.Passed, "counts": rep.Counts})
	return nil
}

// Undo lifts the block and restores the old app, e.g. before migrating back.
func (r *Runner) Undo(ctx context.Context) error {
	err := r.undo(ctx)
	r.update(func(s *State) {
		s.Outcome = OutcomeUndone
		if err != nil {
			s.UndoError = err.Error()
		}
	})
	return err
}

func (r *Runner) fail(step string, cause error) error {
	r.update(func(s *State) { s.FailureStep, s.FailureError = step, cause.Error() })
	// A fresh context: the caller's may be cancelled already.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	undoErr := r.undo(ctx)
	r.update(func(s *State) {
		s.Outcome = OutcomeFailed
		if undoErr != nil {
			s.UndoError = undoErr.Error()
		}
	})
	if undoErr != nil {
		r.emit(step, engine.EventHalt, "putting the source back failed: "+undoErr.Error(), nil)
		return fmt.Errorf("%s failed (%v) and putting the source back failed: %w", step, cause, undoErr)
	}
	r.emit(step, engine.EventRollback, "source unblocked and old app restored", nil)
	return fmt.Errorf("%s: %w", step, cause)
}

func (r *Runner) undo(ctx context.Context) error {
	var errs []error
	for _, name := range r.State().Blocked {
		if err := r.inv.Unblock(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("unblock %s: %w", name, err))
			continue
		}
		r.update(func(s *State) { s.Blocked = without(s.Blocked, name) })
	}
	for _, res := range r.State().Stopped {
		if err := r.inv.Restore(ctx, res); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", res, err))
			continue
		}
		r.update(func(s *State) { s.Stopped = withoutRes(s.Stopped, res) })
	}
	return errors.Join(errs...)
}

func (r *Runner) runStep(ctx context.Context, name string) error {
	now := time.Now().UTC()
	r.update(func(s *State) {
		rec := s.Steps[name]
		rec.Status, rec.StartedAt, rec.EndedAt, rec.Error, rec.Evidence = engine.StatusRunning, &now, nil, "", nil
	})
	r.emit(name, engine.EventStepStarted, "", nil)
	var ev map[string]any
	var err error
	switch name {
	case StepInventory:
		ev, err = r.inventory(ctx)
	case StepStop:
		ev, err = r.stop(ctx)
	case StepObserve:
		ev, err = r.observe(ctx)
	case StepBlock:
		ev, err = r.block(ctx)
	case StepSmoke:
		ev, err = r.smoke(ctx)
	case StepReconcile:
		ev, err = r.reconcile(ctx)
	}
	end := time.Now().UTC()
	r.update(func(s *State) {
		rec := s.Steps[name]
		rec.EndedAt, rec.Evidence = &end, ev
		if err != nil {
			rec.Status, rec.Error = engine.StatusFailed, err.Error()
		} else {
			rec.Status = engine.StatusSucceeded
		}
	})
	if err != nil {
		r.emit(name, engine.EventStepFailed, err.Error(), ev)
		return err
	}
	r.emit(name, engine.EventStepSucceeded, "", map[string]any{"durationMillis": end.Sub(now).Milliseconds(), "evidence": ev})
	return nil
}

// --- steps ---

// inventory lists what belongs to the app and decides what to do with each.
func (r *Runner) inventory(ctx context.Context) (map[string]any, error) {
	found, err := r.inv.List(ctx, r.spec.Selector)
	if err != nil {
		return nil, err
	}
	hasWorkload := false
	for _, res := range found {
		if action(res, r.plan.Source.Cluster) == ActionStop {
			hasWorkload = true
		}
	}
	if !hasWorkload {
		return map[string]any{"found": len(found)}, fmt.Errorf("selector %q matched no Deployment, StatefulSet or CronJob in %s; is it the old app's selector?",
			r.spec.Selector, r.plan.Source.Namespace)
	}
	// The old database and its volumes may not carry the app label.
	db, err := r.inv.List(ctx, "cnpg.io/cluster="+r.plan.Source.Cluster)
	if err != nil {
		return nil, err
	}
	found = append(found, db...)
	cl := kube.Resource{Kind: "Cluster", Name: r.plan.Source.Cluster}
	if ok, err := r.inv.Exists(ctx, cl); err == nil && ok {
		found = append(found, cl)
	}

	seen := map[kube.Resource]bool{}
	var items []Item
	for _, res := range found {
		if seen[res] {
			continue
		}
		seen[res] = true
		act := action(res, r.plan.Source.Cluster)
		if act == "" {
			continue // e.g. the CNPG-managed Secrets and Services are retained with the cluster below
		}
		items = append(items, Item{Resource: res, Action: act, Reason: reason(res, act, r.plan.Source.Cluster)})
	}
	r.update(func(s *State) { s.Inventory = items })
	return map[string]any{"items": items, "counts": countActions(items)}, nil
}

// stop scales the old app to zero and suspends its jobs, then waits until no
// pod or job of it is left.
func (r *Runner) stop(ctx context.Context) (map[string]any, error) {
	var stopped []string
	for _, it := range r.State().Inventory {
		if it.Action != ActionStop {
			continue
		}
		if err := r.inv.Stop(ctx, it.Resource); err != nil {
			return map[string]any{"stopped": stopped}, fmt.Errorf("stop %s: %w", it.Resource, err)
		}
		res := it.Resource
		r.update(func(s *State) { s.Stopped = append(withoutRes(s.Stopped, res), res) })
		stopped = append(stopped, res.String())
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		left := map[string]int64{}
		for _, res := range r.State().Stopped {
			n, err := r.inv.Running(wctx, res)
			if err != nil || n > 0 {
				left[res.String()] = n
			}
		}
		if len(left) == 0 {
			return map[string]any{"stopped": stopped}, nil
		}
		select {
		case <-wctx.Done():
			return map[string]any{"stopped": stopped, "stillRunning": left}, fmt.Errorf("old app still running: %v", left)
		case <-time.After(r.opt.PollInterval):
		}
	}
}

// observe counts what still reaches the source once the old app is stopped.
func (r *Runner) observe(ctx context.Context) (map[string]any, error) {
	res := ObserveResult{WindowSeconds: int(r.spec.Observe.Seconds())}
	db, err := r.connect(ctx, r.plan.Source.DSN)
	if err != nil {
		return map[string]any{"result": res}, fmt.Errorf("connect to the old database: %w", err)
	}
	defer db.Close()

	before, terr := r.traffic.Totals(ctx)
	backend := r.plan.Gateway.SourceBackend
	measureTraffic := terr == nil
	if !measureTraffic {
		res.NotMeasured = append(res.NotMeasured, "gateway requests to the source backend: "+terr.Error())
	} else if _, ok := before[backend]; !ok {
		measureTraffic = false
		res.NotMeasured = append(res.NotMeasured, fmt.Sprintf("gateway has no backend %q", backend))
	}

	maxSessions := 0
	deadline := time.Now().Add(r.spec.Observe.Duration)
	for {
		n, err := db.ClientSessions(ctx)
		if err != nil {
			return map[string]any{"result": res}, fmt.Errorf("count sessions on the old database: %w", err)
		}
		res.Samples++
		if n > maxSessions {
			maxSessions = n
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return map[string]any{"result": res}, ctx.Err()
		case <-time.After(min(r.opt.PollInterval, time.Until(deadline))):
		}
	}
	res.MaxSourceSessions = &maxSessions
	if measureTraffic {
		after, err := r.traffic.Totals(ctx)
		if err != nil {
			return map[string]any{"result": res}, fmt.Errorf("gateway totals: %w", err)
		}
		d := after[backend].Requests - before[backend].Requests
		res.SourceBackendRequests = &d
	}
	ev := map[string]any{"result": res}
	r.update(func(s *State) { s.Observe = &res })
	var problems []string
	if maxSessions > 0 {
		problems = append(problems, fmt.Sprintf("up to %d client sessions on the old database", maxSessions))
	}
	if res.SourceBackendRequests != nil && *res.SourceBackendRequests > 0 {
		problems = append(problems, fmt.Sprintf("%d gateway requests went to the source backend", *res.SourceBackendRequests))
	}
	if len(problems) > 0 {
		return ev, fmt.Errorf("the source is still in use: %s", strings.Join(problems, "; "))
	}
	return ev, nil
}

// block drops ingress to the old app and the old database, and checks that the
// old database really refuses connections.
func (r *Runner) block(ctx context.Context) (map[string]any, error) {
	for _, p := range []struct{ name, sel string }{
		{PolicyApp, r.spec.Selector},
		{PolicyDB, "cnpg.io/cluster=" + r.plan.Source.Cluster},
	} {
		if err := r.inv.Block(ctx, p.name, p.sel); err != nil {
			return nil, fmt.Errorf("block %s: %w", p.sel, err)
		}
		name := p.name
		r.update(func(s *State) { s.Blocked = append(without(s.Blocked, name), name) })
	}
	res := BlockResult{Policies: []string{PolicyApp, PolicyDB}}
	wctx, cancel := context.WithTimeout(ctx, r.opt.BlockCheckTimeout)
	defer cancel()
	for {
		cctx, ccancel := context.WithTimeout(wctx, 5*time.Second)
		db, err := r.connect(cctx, r.plan.Source.DSN)
		ccancel()
		res.Attempts++
		if err != nil {
			res.Verified = true
			res.LastError = err.Error()
			r.update(func(s *State) { s.Block = &res })
			return map[string]any{"result": res}, nil
		}
		db.Close()
		select {
		case <-wctx.Done():
			r.update(func(s *State) { s.Block = &res })
			return map[string]any{"result": res}, errors.New("the old database still accepts connections with the block in place; does the cluster's network plugin enforce NetworkPolicy?")
		case <-time.After(r.opt.PollInterval):
		}
	}
}

// smoke sends the plan's requests to the user entry point while the source is blocked.
func (r *Runner) smoke(ctx context.Context) (map[string]any, error) {
	base := strings.TrimRight(r.spec.SmokeURL, "/")
	var results []SmokeResult
	failed := 0
	for _, q := range r.spec.Smoke {
		u := base + q.Path
		start := time.Now()
		code, err := r.probe(ctx, q.Method, u)
		sr := SmokeResult{Method: q.Method, Path: q.Path, Status: code, Millis: time.Since(start).Milliseconds()}
		switch {
		case err != nil:
			sr.Error = redact(err.Error())
		case q.Expect != 0:
			sr.OK = code == q.Expect
		default:
			sr.OK = code/100 == 2
		}
		if !sr.OK {
			failed++
		}
		results = append(results, sr)
	}
	r.update(func(s *State) { s.Smoke = results })
	ev := map[string]any{"results": results}
	if failed > 0 {
		return ev, fmt.Errorf("%d of %d smoke requests failed with the source blocked", failed, len(results))
	}
	return ev, nil
}

// reconcile checks every inventoried resource again and builds the report.
func (r *Runner) reconcile(ctx context.Context) (map[string]any, error) {
	st := r.State()
	rep := &Report{Observe: st.Observe, Block: st.Block, Smoke: st.Smoke}
	stopped := map[kube.Resource]bool{}
	for _, res := range st.Stopped {
		stopped[res] = true
	}
	known := map[kube.Resource]bool{}
	for _, it := range st.Inventory {
		known[it.Resource] = true
		res := ItemResult{Kind: it.Kind, Name: it.Name, Reason: it.Reason}
		exists, err := r.inv.Exists(ctx, it.Resource)
		switch {
		case err != nil:
			res.Status, res.Reason = StatusUnprocessed, "could not read: "+err.Error()
		case !exists:
			res.Status = StatusRemoved
		case it.Action == ActionStop:
			n, err := r.inv.Running(ctx, it.Resource)
			if err != nil || n > 0 || !stopped[it.Resource] {
				res.Status, res.Reason = StatusUnprocessed, fmt.Sprintf("expected stopped, %d running", n)
			} else {
				res.Status = StatusStopped
			}
		case it.Action == ActionRetain:
			res.Status = StatusRetained
		default:
			res.Status = StatusAwaitingApproval
		}
		rep.Items = append(rep.Items, res)
	}
	// Anything of the app that appeared since the inventory was not handled.
	now, err := r.inv.List(ctx, r.spec.Selector)
	if err != nil {
		return nil, err
	}
	for _, res := range now {
		if !known[res] && action(res, r.plan.Source.Cluster) != "" {
			rep.Items = append(rep.Items, ItemResult{Kind: res.Kind, Name: res.Name, Status: StatusUnprocessed, Reason: "appeared during decommission"})
		}
	}
	for _, it := range rep.Items {
		switch it.Status {
		case StatusStopped:
			rep.Counts.Stopped++
		case StatusRetained:
			rep.Counts.Retained++
		case StatusAwaitingApproval:
			rep.Counts.AwaitingApproval++
		case StatusRemoved:
			rep.Counts.Removed++
		default:
			rep.Counts.Unprocessed++
		}
	}
	if rep.Counts.Unprocessed > 0 {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%d resources unprocessed", rep.Counts.Unprocessed))
	}
	if rep.Observe == nil || rep.Block == nil || !rep.Block.Verified {
		rep.Problems = append(rep.Problems, "observe or block evidence missing")
	}
	rep.Passed = len(rep.Problems) == 0
	r.update(func(s *State) { s.Report = rep })
	ev := map[string]any{"report": rep}
	if !rep.Passed {
		return ev, errors.New(strings.Join(rep.Problems, "; "))
	}
	return ev, nil
}

// --- policy ---

// action decides what decommission does with a resource ("" = not tracked).
func action(res kube.Resource, sourceCluster string) string {
	switch res.Kind {
	case "Deployment", "StatefulSet", "CronJob":
		return ActionStop
	case "Cluster", "PersistentVolumeClaim":
		return ActionRetain
	case "Service", "Ingress", "ConfigMap", "Secret":
		if strings.HasPrefix(res.Name, sourceCluster+"-") {
			return "" // made by CNPG for the retained database
		}
		return ActionAwaitApproval
	}
	return ""
}

func reason(res kube.Resource, act, sourceCluster string) string {
	switch {
	case act == ActionStop:
		return "old app; scaled to zero, restorable"
	case res.Kind == "Cluster" && res.Name == sourceCluster:
		return "old database kept as a replica of the new primary, for migrating back"
	case act == ActionRetain:
		return "data kept with the old database"
	default:
		return "deleting needs an operator's approval"
	}
}

// --- helpers ---

func (r *Runner) update(f func(*State)) {
	r.mu.Lock()
	f(r.state)
	err := r.state.save(r.path)
	r.mu.Unlock()
	if err != nil {
		r.emit("", engine.EventLog, "WARNING: failed to save decommission state: "+err.Error(), nil)
	}
}

func (r *Runner) emit(step, kind, msg string, data map[string]any) {
	if r.emitFn == nil {
		return
	}
	r.emitFn(engine.Event{Time: time.Now().UTC(), RunID: r.state.RunID, Step: step, Kind: kind, Message: msg, Data: data})
}

func countActions(items []Item) map[string]int {
	m := map[string]int{}
	for _, it := range items {
		m[it.Action]++
	}
	return m
}

func without(s []string, v string) []string {
	out := s[:0:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func withoutRes(s []kube.Resource, v kube.Resource) []kube.Resource {
	out := s[:0:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// redact drops userinfo from URLs in an error message.
func redact(msg string) string {
	for _, f := range strings.Fields(msg) {
		f = strings.Trim(f, `"':,`)
		if u, err := url.Parse(f); err == nil && u.User != nil {
			msg = strings.ReplaceAll(msg, f, u.Redacted())
		}
	}
	return msg
}
