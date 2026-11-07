// Package engine executes a migration Plan as a fixed sequence of steps:
//
//	precheck → prepare → presync → delta → fence → switchover → start-target → cutover → verify → finalize
//
// Safety rule (split-brain prevention): a failure is rolled back automatically
// only while the target has not been asked to promote. Once promotion was
// requested the engine halts and leaves the decision to an operator. The one
// exception (ADR 0012) is a cold plan whose target app fails to start: the
// gateway still holds every user write and routes to the source, so the
// promoted target has taken no user write and is demoted again.
package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/files"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/kube"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/loadguard"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// Step names in execution order.
const (
	StepPrecheck   = "precheck"
	StepPrepare    = "prepare"
	StepPresync    = "presync"
	StepDelta      = "delta"
	StepFence      = "fence"
	StepSwitchover = "switchover"
	// StepStartTarget starts the target app after promotion (cold plans only;
	// skipped for warm plans).
	StepStartTarget = "start-target"
	StepCutover     = "cutover"
	StepVerify      = "verify"
	StepFinalize    = "finalize"
)

// Steps lists all steps in order.
var Steps = []string{StepPrecheck, StepPrepare, StepPresync, StepDelta, StepFence, StepSwitchover, StepStartTarget, StepCutover, StepVerify, StepFinalize}

// Event kinds.
const (
	EventStepStarted   = "step_started"
	EventStepSucceeded = "step_succeeded"
	EventStepFailed    = "step_failed"
	EventStepSkipped   = "step_skipped"
	EventLog           = "log"
	EventMetric        = "metric"
	EventRollback      = "rollback"
	EventHalt          = "halt"
	EventDone          = "done"
)

// Event is streamed to the caller (CLI prints it; the platform API can forward it as SSE).
type Event struct {
	Time    time.Time      `json:"time"`
	RunID   string         `json:"runId"`
	Step    string         `json:"step,omitempty"`
	Kind    string         `json:"kind"`
	Message string         `json:"message,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// Options configure an Engine.
type Options struct {
	// StateDir holds <runID>.json state files. Default ".hopcharlie/runs".
	StateDir string
	// RunID resumes an existing run when its state file exists; empty starts a new run.
	RunID string
	// Emit receives events. Must be safe for concurrent use. Optional.
	Emit func(Event)
	// Gateway overrides the gateway built from the plan (tests, embedding).
	Gateway gateway.Gateway

	// The fields below replace production adapters (tests, embedding).
	// Nil means: build from the plan.
	SourceCluster, TargetCluster Cluster
	Connect                      Connector
	Files                        FileSyncer
	Verifier                     Verifier
	// App and Prober serve a cold plan's target app (ADR 0012).
	App    App
	Prober Prober
	// External replaces the adapter of a source.kind external plan (ADR 0019).
	External ExternalSource
	// PollInterval is the base polling period (default 2s). Shorter waits scale from it.
	PollInterval      time.Duration
	SourceWriteFencer SourceWriteFencer
}

// Engine runs one migration.
type Engine struct {
	sourceLoad, targetLoad *loadguard.Controller
	plan                   *plan.Plan
	state                  *State
	statePath              string
	emitFn                 func(Event)
	mu                     sync.Mutex

	src, dst     Cluster        // src is nil for an external source
	ext          ExternalSource // nil unless the source is external (ADR 0019)
	srcDB, dstDB Database
	connect      Connector
	gw           gateway.Gateway
	rclone       FileSyncer // nil when the plan has no files
	verifier     Verifier
	app          App // nil unless the plan is cold
	prober       Prober
	poll         time.Duration
	writeFencer  SourceWriteFencer

	// tokenBeforeDemote is the source demotion token seen just before this run
	// demoted it. CNPG may keep an old token in status, so a demotion counts as
	// finished only when a different token appears.
	tokenBeforeDemote string
}

// New builds an Engine, creating or resuming run state. Database connections are opened lazily.
func New(p *plan.Plan, opt Options) (*Engine, error) {
	if opt.StateDir == "" {
		opt.StateDir = filepath.Join(".hopcharlie", "runs")
	}
	e := &Engine{plan: p, emitFn: opt.Emit, connect: opt.Connect, poll: opt.PollInterval}
	e.writeFencer = opt.SourceWriteFencer
	if f := p.SourceWriteFence; f != nil {
		if err := f.Validate(); err != nil {
			return nil, err
		}
		if p.Gateway.Mode != "http" || p.Target.App.Cold() {
			return nil, errors.New("sourceWriteFence requires an HTTP gateway and a warm target")
		}
		if p.Source.Kind != "" && p.Source.Kind != plan.SourceCNPG || p.Target.Kind != "" && p.Target.Kind != plan.SourceCNPG {
			return nil, errors.New("sourceWriteFence only supports CNPG endpoints with a database")
		}
	}
	if err := e.configureLoad(p); err != nil {
		return nil, err
	}
	if e.connect == nil {
		e.connect = connectPG
	}
	if e.poll <= 0 {
		e.poll = 2 * time.Second
	}

	// Adapters first, so a bad kubeconfig does not leave an orphan state file.
	e.src, e.dst = opt.SourceCluster, opt.TargetCluster
	if p.Stateless() {
		// No database on either side (ADR 0020).
		e.src, e.dst = nil, nil
	} else if p.Source.External() {
		e.src, e.ext = nil, opt.External
		if e.ext == nil {
			e.ext = pgExternal{dsn: p.Source.DSN}
		}
	} else if e.src == nil {
		dyn, err := kube.Dynamic(p.Source.Kubeconfig, p.Source.Context)
		if err != nil {
			return nil, fmt.Errorf("source: %w", err)
		}
		e.src = cnpg.New(dyn, p.Source.Namespace, p.Source.Cluster)
	}
	if e.dst == nil && !p.Stateless() {
		dyn, err := kube.Dynamic(p.Target.Kubeconfig, p.Target.Context)
		if err != nil {
			return nil, fmt.Errorf("target: %w", err)
		}
		e.dst = cnpg.New(dyn, p.Target.Namespace, p.Target.Cluster)
	}
	switch {
	case opt.Gateway != nil:
		e.gw = opt.Gateway
	case p.Gateway.Mode == "http":
		e.gw = gateway.NewHTTP(p.Gateway.URL, p.Gateway.Token)
	default:
		e.gw = gateway.None{}
	}
	if p.Files != nil {
		if opt.Files != nil {
			e.rclone = opt.Files
		} else {
			e.rclone = files.New(p.Files.Rclone, p.Files.Flags)
		}
	}
	e.verifier = opt.Verifier
	if e.verifier == nil {
		e.verifier = sqlVerifier{plan: p, rc: e.rclone, options: scanOptions(e.targetLoad)}
	}
	if a := p.Target.App; a.Cold() {
		e.app, e.prober = opt.App, opt.Prober
		if e.app == nil {
			dyn, err := kube.Dynamic(p.Target.Kubeconfig, p.Target.Context)
			if err != nil {
				return nil, fmt.Errorf("target app: %w", err)
			}
			e.app = kubeApp{inv: &kube.Inventory{Dyn: dyn, Namespace: p.Target.Namespace}, res: kube.Resource{Kind: a.Kind, Name: a.Name}}
		}
		if e.prober == nil {
			e.prober = httpProber{client: &http.Client{Timeout: 10 * time.Second}}
		}
	}
	if p.Stateless() && e.prober == nil {
		// A warm stateless plan may smoke-test the target app (ADR 0020).
		e.prober = opt.Prober
		if e.prober == nil {
			e.prober = httpProber{client: &http.Client{Timeout: 10 * time.Second}}
		}
	}

	runID := opt.RunID
	if runID == "" {
		runID = fmt.Sprintf("%s-%s", p.ID, time.Now().UTC().Format("20060102-150405"))
	}
	e.statePath = filepath.Join(opt.StateDir, runID+".json")
	st, err := loadState(e.statePath)
	switch {
	case err == nil:
		if st.PlanID != p.ID {
			return nil, fmt.Errorf("run %s belongs to plan %q, not %q", runID, st.PlanID, p.ID)
		}
		e.state = st
	case errors.Is(err, os.ErrNotExist):
		if opt.RunID != "" {
			return nil, fmt.Errorf("run %s not found in %s", runID, opt.StateDir)
		}
		e.state = newState(runID, p.ID)
		if err := e.state.save(e.statePath); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return e, nil
}

// RunID returns the run identifier.
func (e *Engine) RunID() string { return e.state.RunID }

// StatePath returns the state file path.
func (e *Engine) StatePath() string { return e.statePath }

// State returns a snapshot of the run state that is safe to read from any
// goroutine, including while a step is running. Step records and their
// evidence maps are copied; evidence values are never mutated after a step
// stores them, so they are shared.
func (e *Engine) State() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := *e.state
	if s.SourceWriteFence != nil {
		proof := *s.SourceWriteFence
		proof.WriterRoles = slices.Clone(proof.WriterRoles)
		s.SourceWriteFence = &proof
	}
	s.Steps = make(map[string]*StepRecord, len(e.state.Steps))
	for name, rec := range e.state.Steps {
		if rec == nil {
			continue
		}
		r := *rec
		r.Evidence = maps.Clone(rec.Evidence)
		s.Steps[name] = &r
	}
	return s
}

// Close releases database connections.
func (e *Engine) Close() {
	if e.srcDB != nil {
		e.srcDB.Close()
	}
	if e.dstDB != nil {
		e.dstDB.Close()
	}
}

// Run executes every remaining step. Succeeded steps from a previous attempt are skipped.
func (e *Engine) Run(ctx context.Context) error {
	if err := e.checkRunnable(); err != nil {
		return err
	}
	for _, name := range Steps {
		if e.state.Steps[name].Status == StatusSucceeded {
			e.emit(name, EventStepSkipped, "already succeeded", nil)
			continue
		}
		if err := e.runStep(ctx, name); err != nil {
			return e.onFailure(name, err)
		}
	}
	e.mu.Lock()
	e.state.Outcome = OutcomeSucceeded
	e.state.FailureStep, e.state.FailureError = "", ""
	e.mu.Unlock()
	e.save()
	e.emit("", EventDone, "migration succeeded", map[string]any{"downtimeMillis": e.state.DowntimeMillis})
	return nil
}

// RunStep executes a single step (manual rehearsal). All earlier steps must have succeeded.
func (e *Engine) RunStep(ctx context.Context, name string) error {
	idx := slices.Index(Steps, name)
	if idx < 0 {
		return fmt.Errorf("unknown step %q (steps: %v)", name, Steps)
	}
	if err := e.checkRunnable(); err != nil {
		return err
	}
	for _, prev := range Steps[:idx] {
		if e.state.Steps[prev].Status != StatusSucceeded {
			return fmt.Errorf("step %q requires %q to have succeeded (status %s)", name, prev, e.state.Steps[prev].Status)
		}
	}
	if err := e.runStep(ctx, name); err != nil {
		return e.onFailure(name, err)
	}
	if name == StepFinalize {
		e.mu.Lock()
		e.state.Outcome = OutcomeSucceeded
		e.state.FailureStep, e.state.FailureError = "", ""
		e.mu.Unlock()
		e.save()
	}
	return nil
}

// ResumeHalted lets an operator continue a run that halted after the target's
// promotion was requested (ADR 0013). The engine halts there because it cannot
// tell, by itself, which side is primary. Here it checks that the promotion
// went through and nothing else is primary: the target is the topology
// primary, writable and settled; the source follows the target and runs in
// recovery. Then the switchover counts as done and Run carries on from the
// first unfinished step (cutover releases the held writes on the target).
// Any doubt keeps the run halted with the reason.
func (e *Engine) ResumeHalted(ctx context.Context) error {
	st := e.State()
	if f := st.SourceWriteFence; f != nil && f.Intent && (!f.Released || e.writeFencer == nil) {
		return errors.New("source-local SQL fence remains owned by this run; reconcile it explicitly before resume")
	}
	if st.Outcome != OutcomeHalted {
		return fmt.Errorf("run %s is %s, not halted", st.RunID, st.Outcome)
	}
	if e.plan.Stateless() {
		return e.resumeHaltedStateless(ctx, st)
	}
	if e.plan.Source.External() {
		return e.resumeHaltedExternal(ctx, st)
	}
	for _, step := range []string{StepFence, StepStartTarget, StepCutover} {
		if st.Steps[step].Status == StatusRunning {
			return fmt.Errorf("interrupted %s requires operator reconciliation; refusing automatic resume", step)
		}
	}
	if !st.PromoteRequested || !st.SourceDemoted {
		return fmt.Errorf("run %s halted before the target's promotion (%s: %s); this needs an operator, not a resume", st.RunID, st.FailureStep, st.FailureError)
	}
	cctx, cancel := context.WithTimeout(ctx, e.plan.Thresholds.SwitchoverTimeout.Duration)
	defer cancel()
	ev := map[string]any{"haltedAt": st.FailureStep, "haltError": st.FailureError}

	d, err := e.dst.Get(cctx)
	if err != nil {
		return fmt.Errorf("read target cluster: %w", err)
	}
	if !d.IsPrimary() {
		return fmt.Errorf("target is not the topology primary (replica.primary=%q, self=%q); not resuming", d.ReplicaPrimary, d.Self)
	}
	s, err := e.src.Get(cctx)
	if err != nil {
		return fmt.Errorf("read source cluster: %w", err)
	}
	if s.ReplicaPrimary != d.Self {
		return fmt.Errorf("source does not follow the target (replica.primary=%q, want %q); not resuming", s.ReplicaPrimary, d.Self)
	}
	if err := e.waitRecovery(cctx, e.plan.Source.DSN, &e.srcDB, true); err != nil {
		return fmt.Errorf("cannot confirm the source runs as a replica: %w", err)
	}
	if err := e.waitWritable(cctx, e.plan.Target.DSN, &e.dstDB); err != nil {
		return fmt.Errorf("target is not writable: %w", err)
	}
	rep, err := e.waitSettled(cctx, e.dst, e.plan.Target.DSN, &e.dstDB)
	ev["settle"] = rep
	if err != nil {
		return fmt.Errorf("target did not settle: %w", err)
	}
	e.markResumed(st, ev, "resumed after halt: target is primary and settled, source follows it")
	return nil
}

// markResumed records a confirmed resume: the switchover counts as done and
// the run carries on.
func (e *Engine) markResumed(st State, ev map[string]any, msg string) {
	now := time.Now().UTC()
	e.mu.Lock()
	if rec := e.state.Steps[StepSwitchover]; rec.Status != StatusSucceeded {
		prev := rec.Evidence
		rec.Status, rec.Error, rec.EndedAt = StatusSucceeded, "", &now
		rec.Evidence = map[string]any{"resumedAfterHalt": ev, "before": prev}
	}
	e.state.Outcome = OutcomeRunning
	e.state.FailureStep, e.state.FailureError = "", ""
	e.mu.Unlock()
	e.save()
	e.log(st.FailureStep, msg, map[string]any{"haltedAt": st.FailureStep})
}

func (e *Engine) checkRunnable() error {
	if f := e.state.SourceWriteFence; f != nil && f.Intent && e.writeFencer == nil && e.state.Outcome != OutcomeSucceeded {
		e.mu.Lock()
		e.state.Outcome = OutcomeHalted
		e.state.FailureStep = StepFence
		e.state.FailureError = "source write fence survived a process restart; operator reconciliation required"
		e.mu.Unlock()
		e.save()
		return errors.New(e.state.FailureError)
	}
	// A process may die after an external request was applied but before its
	// result was saved. Never replay an interrupted step across the fence.
	if e.state.Outcome == OutcomeRunning {
		for _, step := range Steps {
			if e.state.Steps[step].Status == StatusRunning && slices.Contains([]string{StepFence, StepSwitchover, StepStartTarget, StepCutover}, step) {
				e.mu.Lock()
				e.state.Outcome = OutcomeHalted
				e.state.FailureStep = step
				e.state.FailureError = "process interrupted during safety-critical step; reconcile observed cluster and gateway state before continuing"
				e.mu.Unlock()
				e.save()
				e.emit(step, EventHalt, e.state.FailureError, nil)
				return fmt.Errorf("run %s halted: %s", e.state.RunID, e.state.FailureError)
			}
		}
	}
	switch e.state.Outcome {
	case OutcomeRunning:
		return nil
	case OutcomeFailed:
		// Nothing irreversible happened; allow retrying failed steps.
		e.mu.Lock()
		e.state.Outcome = OutcomeRunning
		e.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("run %s is %s; start a new run", e.state.RunID, e.state.Outcome)
	}
}

type stepFunc func(*Engine, context.Context) (map[string]any, error)

var stepFuncs = map[string]stepFunc{
	StepPrecheck:    (*Engine).precheck,
	StepPrepare:     (*Engine).prepare,
	StepPresync:     (*Engine).presync,
	StepDelta:       (*Engine).delta,
	StepFence:       (*Engine).fence,
	StepSwitchover:  (*Engine).switchover,
	StepStartTarget: (*Engine).startTarget,
	StepCutover:     (*Engine).cutover,
	StepVerify:      (*Engine).verify,
	StepFinalize:    (*Engine).finalize,
}

// stepFunc is the function of a step for this plan: an external source
// replaces some steps (ADR 0019), the others are shared.
func (e *Engine) stepFunc(name string) stepFunc {
	if e.plan.Stateless() {
		if f, ok := statelessStepFuncs[name]; ok {
			return f
		}
	}
	if e.plan.Source.External() {
		if f, ok := externalStepFuncs[name]; ok {
			return f
		}
	}
	return stepFuncs[name]
}

// skipReason says why a step has nothing to do for this plan ("" = run it).
func (e *Engine) skipReason(name string) string {
	if name == StepStartTarget && !e.plan.Target.App.Cold() {
		return "warm mode: the target app already runs"
	}
	if e.plan.Stateless() {
		return statelessSkip[name]
	}
	return ""
}

func (e *Engine) runStep(ctx context.Context, name string) error {
	rec := e.state.Steps[name]
	now := time.Now().UTC()
	if reason := e.skipReason(name); reason != "" {
		e.mu.Lock()
		rec.Status, rec.StartedAt, rec.EndedAt, rec.Error, rec.Evidence = StatusSucceeded, &now, &now, "", map[string]any{"skipped": reason}
		e.mu.Unlock()
		e.save()
		e.emit(name, EventStepSkipped, reason, nil)
		return nil
	}
	e.mu.Lock()
	rec.Status, rec.StartedAt, rec.EndedAt, rec.Error, rec.Evidence = StatusRunning, &now, nil, "", nil
	e.mu.Unlock()
	e.save()
	e.emit(name, EventStepStarted, "", nil)

	ev, err := e.guardedStep(ctx, name)

	end := time.Now().UTC()
	e.mu.Lock()
	rec.EndedAt = &end
	rec.Evidence = ev
	if err != nil {
		rec.Status, rec.Error = StatusFailed, err.Error()
	} else {
		rec.Status = StatusSucceeded
	}
	e.mu.Unlock()
	e.save()
	if err != nil {
		e.emit(name, EventStepFailed, err.Error(), ev)
		return err
	}
	e.emit(name, EventStepSucceeded, "", map[string]any{"durationMillis": end.Sub(*rec.StartedAt).Milliseconds(), "evidence": ev})
	return nil
}

// onFailure applies the rollback boundary.
func (e *Engine) onFailure(step string, cause error) error {
	e.mu.Lock()
	e.state.FailureStep, e.state.FailureError = step, cause.Error()
	e.mu.Unlock()
	if e.plan.Stateless() {
		return e.onStatelessFailure(step, cause)
	}
	if e.plan.Source.External() {
		return e.onExternalFailure(step, cause)
	}

	// A fresh context: the caller's may already be cancelled (e.g. Ctrl-C).
	budget := 2*e.plan.Thresholds.SwitchoverTimeout.Duration + demotionGrace + rollbackSlack
	if a := e.plan.Target.App; a.Cold() {
		budget += a.StartTimeout.Duration + e.plan.Thresholds.CatchupTimeout.Duration
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	// Once CNPG has started source demotion, restoring read admission does not
	// restore permission to re-promote it. In particular, a failed persistence
	// after HBA/include cleanup may already have changed Released in memory.
	if e.plan.SourceWriteFence != nil && e.state.SourceDemoted && !e.state.PromoteRequested {
		e.mu.Lock()
		e.state.Outcome = OutcomeHalted
		e.mu.Unlock()
		e.save()
		e.emit(step, EventHalt, "source demotion crossed the SQL fence boundary; leave HTTP writes held for reconciliation", nil)
		return fmt.Errorf("halted after source SQL demotion: %w", cause)
	}
	if f := e.state.SourceWriteFence; f != nil && f.Intent && !f.Released && !e.state.PromoteRequested {
		if e.writeFencer == nil || e.state.SourceDemoted {
			e.mu.Lock()
			e.state.Outcome = OutcomeHalted
			e.mu.Unlock()
			e.save()
			return fmt.Errorf("halted with source SQL fence after uncertain connection or demotion: %w", cause)
		}
		if err := e.writeFencer.ObserverAlive(ctx); err != nil {
			e.mu.Lock()
			e.state.Outcome = OutcomeHalted
			e.mu.Unlock()
			e.save()
			return fmt.Errorf("halted without reopening source admission: %w", err)
		}
	}

	switch {
	case e.canRevertPromotion(step):
		e.emit(step, EventRollback, "target app did not start; demoting the promoted target while the gateway still holds every write", nil)
		rbErr := e.rollbackPromoted(ctx)
		e.mu.Lock()
		if rbErr != nil {
			e.state.RollbackError = rbErr.Error()
			e.state.Outcome = OutcomeHalted
		} else {
			e.state.Outcome = OutcomeRolledBack
		}
		e.mu.Unlock()
		e.save()
		if rbErr != nil {
			e.emit(step, EventHalt, "rollback after promotion failed; operator action required: "+rbErr.Error(),
				map[string]any{"writesHeld": e.state.WritesHeld, "trafficOnTarget": e.state.TrafficOnTarget})
			return fmt.Errorf("%s failed (%v) and rollback failed: %w", step, cause, rbErr)
		}
		e.emit(step, EventRollback, "rolled back; source is primary and serving traffic", nil)
		return fmt.Errorf("rolled back after %s: %w", step, cause)

	case e.state.PromoteRequested:
		e.mu.Lock()
		e.state.Outcome = OutcomeHalted
		e.mu.Unlock()
		e.save()
		e.emit(step, EventHalt, "target promotion was requested; not rolling back automatically (split-brain guard). Operator action required.",
			map[string]any{"writesHeld": e.state.WritesHeld, "trafficOnTarget": e.state.TrafficOnTarget})
		return fmt.Errorf("halted at %s: %w", step, cause)

	case e.state.WritesHeld || e.state.SourceDemoted:
		e.emit(step, EventRollback, "rolling back to source", nil)
		rbErr := e.rollback(ctx)
		e.mu.Lock()
		if rbErr != nil {
			e.state.RollbackError = rbErr.Error()
			e.state.Outcome = OutcomeHalted
		} else {
			e.state.Outcome = OutcomeRolledBack
		}
		e.mu.Unlock()
		e.save()
		if rbErr != nil {
			e.emit(step, EventHalt, "rollback failed: "+rbErr.Error(), nil)
			return fmt.Errorf("%s failed (%v) and rollback failed: %w", step, cause, rbErr)
		}
		e.emit(step, EventRollback, "rolled back; source is primary and serving traffic", nil)
		return fmt.Errorf("rolled back after %s: %w", step, cause)

	default:
		e.mu.Lock()
		e.state.Outcome = OutcomeFailed
		e.mu.Unlock()
		e.save()
		return fmt.Errorf("%s: %w", step, cause)
	}
}

// demotionGrace is how much longer than switchoverTimeout a rollback waits for
// an in-flight demotion to finish. A variable so tests can shorten it.
var demotionGrace = 30 * time.Second

// rollbackSlack is the rollback's time beyond the demotion wait: re-promotion,
// settling and the gateway calls. A variable so tests can shorten it.
var rollbackSlack = time.Minute

// rollback restores the source as primary and releases held writes to it.
// Only valid while the target has not been asked to promote.
func (e *Engine) rollback(ctx context.Context) error {
	if e.plan.Source.External() {
		return errNoDemotionRollback
	}
	if e.state.SourceDemoted {
		// CNPG demotes by fencing and stopping the instances, publishing a token
		// and restarting them as replicas. A promotion requested before that ends
		// is lost: reverting replica.primary mid-demotion left the instances fenced,
		// and promoting right after the token appeared left the instance a standby,
		// both while the Cluster reported healthy (seen on a real cluster). So wait
		// until the source runs as a replica, then promote it with its own token,
		// the same path a normal promotion takes. If the demotion does not finish,
		// the state is unknown: leave it to an operator.
		wctx, cancel := context.WithTimeout(ctx, e.plan.Thresholds.SwitchoverTimeout.Duration+demotionGrace)
		demoted, err := e.src.WaitFor(wctx, e.poll, e.demotionFinished)
		if err == nil {
			err = e.waitRecovery(wctx, e.plan.Source.DSN, &e.srcDB, true)
		}
		cancel()
		if err != nil {
			return fmt.Errorf("source demotion did not finish, not re-promoting it mid-transition: %w", err)
		}
		srcSelf, err := e.selfName(ctx, e.src)
		if err != nil {
			return err
		}
		if err := e.src.SetPrimary(ctx, srcSelf, demoted.DemotionToken); err != nil {
			return err
		}
		e.log(StepSwitchover, "re-promoting source", map[string]any{"primary": srcSelf})
		if err := e.waitWritable(ctx, e.plan.Source.DSN, &e.srcDB); err != nil {
			return fmt.Errorf("source did not become writable again: %w", err)
		}
		// CNPG restarts a re-promoted source in place just as it restarts a
		// promoted target (ADR 0009); releasing writes before that restart
		// sends them to a primary that is shutting down.
		start := time.Now()
		rep, err := e.waitSettled(ctx, e.src, e.plan.Source.DSN, &e.srcDB)
		if err != nil {
			return fmt.Errorf("source did not settle after re-promotion: %w", err)
		}
		e.log(StepSwitchover, "source settled", map[string]any{"settleMillis": time.Since(start).Milliseconds(), "unobservedPolls": rep.Unobserved, "restarts": rep.Restarts})
		e.mu.Lock()
		e.state.SourceDemoted = false
		e.mu.Unlock()
		e.save()
	}
	if e.state.WritesHeld {
		if err := e.restoreSourceWriteFence(ctx, true); err != nil {
			return fmt.Errorf("restore source SQL write fence: %w", err)
		}
		if err := e.gw.Route(ctx, e.plan.Gateway.SourceBackend); err != nil {
			return err
		}
		if err := e.gw.ReleaseWrites(ctx); err != nil {
			return err
		}
		e.mu.Lock()
		e.state.WritesHeld = false
		if e.state.WritesHeldAt != nil {
			e.state.DowntimeMillis = time.Since(*e.state.WritesHeldAt).Milliseconds()
		}
		e.mu.Unlock()
		e.save()
	}
	return nil
}

// canRevertPromotion is the one place a promoted target may be demoted again
// (ADR 0012): a cold plan's start-target failed while the gateway still holds
// every user write and routes to the source, so no user write reached the
// target. Anything less certain halts as before.
func (e *Engine) canRevertPromotion(step string) bool {
	return e.plan.SourceWriteFence == nil && step == StepStartTarget && e.plan.Target.App.Cold() &&
		e.state.PromoteRequested && e.state.SourceDemoted && e.state.WritesHeld && !e.state.TrafficOnTarget
}

// rollbackPromoted undoes a switchover whose target app did not start: stop
// the target app, demote the target, promote the source with the target's
// demotion token, check the target follows the source again, and release the
// held writes to the source. Any failure leaves the rest to an operator.
func (e *Engine) rollbackPromoted(ctx context.Context) error {
	if e.plan.Source.External() {
		return errNoDemotionRollback
	}
	const step = StepStartTarget
	a := e.plan.Target.App

	// 1. No target app pods: their start-up writes stop, and their sessions do
	// not delay the demotion's shutdown.
	if err := e.app.Stop(ctx); err != nil {
		return fmt.Errorf("stop target app: %w", err)
	}
	if err := e.waitAppStopped(ctx, a.StartTimeout.Duration); err != nil {
		return err
	}
	e.log(step, "target app stopped", nil)

	// 2. Demote the target; CNPG publishes a token once it runs as a replica.
	srcSelf, err := e.selfName(ctx, e.src)
	if err != nil {
		return err
	}
	dst, err := e.dst.Get(ctx)
	if err != nil {
		return err
	}
	before := dst.DemotionToken
	if err := e.dst.SetPrimary(ctx, srcSelf, ""); err != nil {
		return fmt.Errorf("demote target: %w", err)
	}
	e.log(step, "target demotion requested", map[string]any{"newPrimary": srcSelf})
	wctx, cancel := context.WithTimeout(ctx, e.plan.Thresholds.SwitchoverTimeout.Duration+demotionGrace)
	demoted, err := e.dst.WaitFor(wctx, e.poll, func(s *cnpg.Status) bool { return s.DemotionToken != "" && s.DemotionToken != before })
	if err == nil {
		err = e.waitRecovery(wctx, e.plan.Target.DSN, &e.dstDB, true)
	}
	cancel()
	if err != nil {
		return fmt.Errorf("target demotion did not finish, not re-promoting the source mid-transition: %w", err)
	}
	e.setFlags(func(s *State) { s.PromoteRequested = false })

	// 3. Promote the source with the target's token, then wait for it to settle
	// (ADR 0009) before any write is released to it.
	if err := e.src.SetPrimary(ctx, srcSelf, demoted.DemotionToken); err != nil {
		return fmt.Errorf("re-promote source: %w", err)
	}
	e.log(step, "re-promoting source", map[string]any{"primary": srcSelf})
	if err := e.waitWritable(ctx, e.plan.Source.DSN, &e.srcDB); err != nil {
		return fmt.Errorf("source did not become writable again: %w", err)
	}
	start := time.Now()
	rep, err := e.waitSettled(ctx, e.src, e.plan.Source.DSN, &e.srcDB)
	if err != nil {
		return fmt.Errorf("source did not settle after re-promotion: %w", err)
	}
	e.log(step, "source settled", map[string]any{"settleMillis": time.Since(start).Milliseconds(), "unobservedPolls": rep.Unobserved, "restarts": rep.Restarts})
	e.setFlags(func(s *State) { s.SourceDemoted = false })

	// 4. The target follows the source again, so the next attempt starts from
	// the usual topology.
	if lag, err := e.waitCaughtUp(ctx, step, e.plan.Thresholds.CatchupTimeout.Duration); err != nil {
		return fmt.Errorf("target does not follow the source again (lag %d): %w", lag, err)
	}

	// 5. Release the held writes to the source.
	if err := e.gw.Route(ctx, e.plan.Gateway.SourceBackend); err != nil {
		return err
	}
	if err := e.gw.ReleaseWrites(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	e.state.WritesHeld = false
	if e.state.WritesHeldAt != nil {
		e.state.DowntimeMillis = time.Since(*e.state.WritesHeldAt).Milliseconds()
	}
	e.mu.Unlock()
	e.save()
	return nil
}

func (e *Engine) emit(step, kind, msg string, data map[string]any) {
	if e.emitFn == nil {
		return
	}
	e.emitFn(Event{Time: time.Now().UTC(), RunID: e.state.RunID, Step: step, Kind: kind, Message: msg, Data: data})
}

func (e *Engine) log(step, msg string, data map[string]any) { e.emit(step, EventLog, msg, data) }

func (e *Engine) save() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.state.save(e.statePath); err != nil {
		e.emit("", EventLog, "WARNING: failed to save state: "+err.Error(), nil)
	}
}

func (e *Engine) setFlags(f func(*State)) {
	e.mu.Lock()
	f(e.state)
	e.mu.Unlock()
	e.save()
}
