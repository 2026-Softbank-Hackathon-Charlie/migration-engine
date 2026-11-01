// Package engine executes a migration Plan as a fixed sequence of steps:
//
//	precheck → prepare → presync → delta → fence → switchover → cutover → verify → finalize
//
// Safety rule (split-brain prevention): a failure is rolled back automatically
// only while the target has not been asked to promote. Once promotion was
// requested the engine halts and leaves the decision to an operator.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/files"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/kube"
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
	StepCutover    = "cutover"
	StepVerify     = "verify"
	StepFinalize   = "finalize"
)

// Steps lists all steps in order.
var Steps = []string{StepPrecheck, StepPrepare, StepPresync, StepDelta, StepFence, StepSwitchover, StepCutover, StepVerify, StepFinalize}

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
	// PollInterval is the base polling period (default 2s). Shorter waits scale from it.
	PollInterval time.Duration
}

// Engine runs one migration.
type Engine struct {
	plan      *plan.Plan
	state     *State
	statePath string
	emitFn    func(Event)
	mu        sync.Mutex

	src, dst     Cluster
	srcDB, dstDB Database
	connect      Connector
	gw           gateway.Gateway
	rclone       FileSyncer // nil when the plan has no files
	verifier     Verifier
	poll         time.Duration
}

// New builds an Engine, creating or resuming run state. Database connections are opened lazily.
func New(p *plan.Plan, opt Options) (*Engine, error) {
	if opt.StateDir == "" {
		opt.StateDir = filepath.Join(".hopcharlie", "runs")
	}
	e := &Engine{plan: p, emitFn: opt.Emit, connect: opt.Connect, poll: opt.PollInterval}
	if e.connect == nil {
		e.connect = connectPG
	}
	if e.poll <= 0 {
		e.poll = 2 * time.Second
	}

	// Adapters first, so a bad kubeconfig does not leave an orphan state file.
	e.src, e.dst = opt.SourceCluster, opt.TargetCluster
	if e.src == nil {
		dyn, err := kube.Dynamic(p.Source.Kubeconfig, p.Source.Context)
		if err != nil {
			return nil, fmt.Errorf("source: %w", err)
		}
		e.src = cnpg.New(dyn, p.Source.Namespace, p.Source.Cluster)
	}
	if e.dst == nil {
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
		e.verifier = sqlVerifier{plan: p, rc: e.rclone}
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

// State returns a snapshot copy of the run state.
func (e *Engine) State() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return *e.state
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
		e.mu.Unlock()
		e.save()
	}
	return nil
}

func (e *Engine) checkRunnable() error {
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
	StepPrecheck:   (*Engine).precheck,
	StepPrepare:    (*Engine).prepare,
	StepPresync:    (*Engine).presync,
	StepDelta:      (*Engine).delta,
	StepFence:      (*Engine).fence,
	StepSwitchover: (*Engine).switchover,
	StepCutover:    (*Engine).cutover,
	StepVerify:     (*Engine).verify,
	StepFinalize:   (*Engine).finalize,
}

func (e *Engine) runStep(ctx context.Context, name string) error {
	rec := e.state.Steps[name]
	now := time.Now().UTC()
	e.mu.Lock()
	rec.Status, rec.StartedAt, rec.EndedAt, rec.Error, rec.Evidence = StatusRunning, &now, nil, "", nil
	e.mu.Unlock()
	e.save()
	e.emit(name, EventStepStarted, "", nil)

	ev, err := stepFuncs[name](e, ctx)

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

	// A fresh context: the caller's may already be cancelled (e.g. Ctrl-C).
	ctx, cancel := context.WithTimeout(context.Background(), e.plan.Thresholds.SwitchoverTimeout.Duration+time.Minute)
	defer cancel()

	switch {
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

// rollback restores the source as primary and releases held writes to it.
// Only valid while the target has not been asked to promote.
func (e *Engine) rollback(ctx context.Context) error {
	if e.state.SourceDemoted {
		srcSelf, err := e.selfName(ctx, e.src)
		if err != nil {
			return err
		}
		if err := e.src.SetPrimary(ctx, srcSelf, ""); err != nil {
			return err
		}
		e.log(StepSwitchover, "re-promoting source", map[string]any{"primary": srcSelf})
		if err := e.waitWritable(ctx, e.plan.Source.DSN, &e.srcDB); err != nil {
			return fmt.Errorf("source did not become writable again: %w", err)
		}
		e.mu.Lock()
		e.state.SourceDemoted = false
		e.mu.Unlock()
		e.save()
	}
	if e.state.WritesHeld {
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
