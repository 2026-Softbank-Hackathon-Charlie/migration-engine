package engine

// A stateless plan (ADR 0020) moves an app without a database: source.kind
// and target.kind are both "none". Only the gateway route moves, plus the
// files when the plan has them. The steps keep their names:
//
//	precheck    gateway reachable, files source readable, target app answers
//	prepare     stop a cold target app, first file copy
//	presync     skipped (no database to catch up)
//	delta       copy the files changed since prepare
//	fence       hold writes at the gateway, last file copy (within maxDowntime)
//	switchover  skipped (no database to promote)
//	start-target  start a cold target app (shared, ADR 0012)
//	cutover     route to the target and release the held writes (shared)
//	verify      files one way, smoke the target app when the plan has smoke
//	finalize    record the end state
//
// Files are copied with rclone copy, never sync: a copy does not delete
// target objects missing on the source, such as uploads written to the
// target after an earlier cutover.
//
// Rollback: no data lives in a database, so a failure from the fence up to
// the cutover routes the gateway back to the source and releases held writes
// there. After the cutover moved traffic, a plan without files is routed back
// too; a plan with files halts instead (users may have written files to the
// target that the source lacks) and waits for -resume-halted. A cancelled
// context after the cutover also halts: the migration itself succeeded.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

var statelessStepFuncs = map[string]stepFunc{
	StepPrecheck: (*Engine).statelessPrecheck,
	StepPrepare:  (*Engine).statelessPrepare,
	StepDelta:    (*Engine).statelessDelta,
	StepFence:    (*Engine).statelessFence,
	StepVerify:   (*Engine).statelessVerify,
	StepFinalize: (*Engine).statelessFinalize,
}

// statelessSkip says why a step has nothing to do for a stateless plan.
var statelessSkip = map[string]string{
	StepPresync:    "no database: the first file copy ran in prepare",
	StepSwitchover: "no database to promote: cutover moves the route",
}

// ErrResumeRolledBackStateless is returned by ResumeHalted when it finished a
// halted stateless run by routing it back to the source: the run is over.
// errors.Is matches ErrResumeRolledBack too.
var ErrResumeRolledBackStateless error = statelessRolledBack{}

type statelessRolledBack struct{}

func (statelessRolledBack) Error() string { return "halted stateless run rolled back to the source" }
func (statelessRolledBack) Unwrap() error { return ErrResumeRolledBack }

// statelessPrecheck: the gateway answers, the files remotes can be read, and
// a warm target app with smoke requests answers them.
func (e *Engine) statelessPrecheck(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	if err := e.gw.Health(ctx); err != nil {
		return ev, fmt.Errorf("gateway: %w", err)
	}
	if e.rclone != nil {
		if err := e.rclone.Available(); err != nil {
			return ev, err
		}
		// Only the source: the target path may not exist before the first
		// migration, and the first copy creates it.
		if err := e.rclone.Readable(ctx, e.plan.Files.Source); err != nil {
			return ev, fmt.Errorf("files source: %w", err)
		}
	}
	a := e.plan.Target.App
	switch {
	case a.Cold():
		// Reading the workload proves it exists and the engine may see it.
		n, err := e.app.Running(ctx)
		if err != nil {
			return ev, fmt.Errorf("target app %s/%s: %w", a.Kind, a.Name, err)
		}
		ev["targetApp"] = map[string]any{"mode": a.Mode, "kind": a.Kind, "name": a.Name, "podsNow": n}
	case a != nil && len(a.Smoke) > 0:
		results, failed := e.smoke(ctx, a)
		ev["smoke"] = results
		if failed > 0 {
			return ev, fmt.Errorf("%d of %d smoke requests to the target app failed", failed, len(results))
		}
	default:
		ev["targetApp"] = "not probed: the plan has no target.app.smoke"
	}
	return ev, nil
}

// statelessPrepare stops a cold target app and makes the first file copy.
func (e *Engine) statelessPrepare(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	if a := e.plan.Target.App; a.Cold() {
		if err := e.app.Stop(ctx); err != nil {
			return ev, fmt.Errorf("stop target app: %w", err)
		}
		if err := e.waitAppStopped(ctx, a.StartTimeout.Duration); err != nil {
			return ev, err
		}
		ev["targetApp"] = "stopped until start-target"
	}
	return ev, e.statelessSync(ctx, ev)
}

// statelessDelta copies the files changed since prepare, so the last copy
// under the write hold is short.
func (e *Engine) statelessDelta(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	return ev, e.statelessSync(ctx, ev)
}

// statelessFence holds writes at the gateway and makes the last file copy.
// Downtime for writes starts here.
func (e *Engine) statelessFence(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	if err := e.gw.HoldWrites(ctx); err != nil {
		return ev, fmt.Errorf("hold writes: %w", err)
	}
	now := time.Now().UTC()
	e.setFlags(func(s *State) { s.WritesHeld, s.WritesHeldAt = true, &now })
	ev["writesHeldAt"] = now
	e.log(StepFence, "writes held at gateway", nil)
	// Writes stay held for this copy: bound it by the downtime budget.
	sctx, cancel := context.WithTimeout(ctx, e.plan.Thresholds.MaxDowntime.Duration)
	defer cancel()
	return ev, e.statelessSync(sctx, ev)
}

// statelessSync copies new and changed files and never deletes from the
// target (ADR 0020).
func (e *Engine) statelessSync(ctx context.Context, ev map[string]any) error {
	if e.rclone == nil {
		return nil
	}
	r, err := e.rclone.Copy(ctx, e.plan.Files.Source, e.plan.Files.Target)
	ev["filesCopy"] = r
	return err
}

// statelessVerify checks that every source object is on the target and, when
// the plan has target.app.smoke, that the target app answers.
func (e *Engine) statelessVerify(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	rep := verifyStateless(ctx, e.plan, e.rclone)
	ev["report"] = rep
	if a := e.plan.Target.App; a != nil && len(a.Smoke) > 0 {
		results, err := e.smokeUntil(ctx, a, e.plan.Thresholds.SwitchoverTimeout.Duration)
		ev["smoke"] = results
		if err != nil {
			rep.Problems = append(rep.Problems, err.Error())
			rep.OK = false
		}
	}
	if err := ctx.Err(); err != nil {
		// Cancelled, not a verification result (onStatelessFailure halts).
		return ev, err
	}
	if !rep.OK {
		return ev, fmt.Errorf("verification failed: %s", strings.Join(rep.Problems, "; "))
	}
	return ev, nil
}

// smokeUntil repeats the smoke set until every request passes or timeout ends.
func (e *Engine) smokeUntil(ctx context.Context, a *plan.AppSpec, timeout time.Duration) ([]SmokeResult, error) {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		results, failed := e.smoke(tctx, a)
		if failed == 0 {
			return results, nil
		}
		select {
		case <-tctx.Done():
			return results, fmt.Errorf("%d of %d smoke requests to the target app failed", failed, len(results))
		case <-time.After(e.poll):
		}
	}
}

// statelessFinalize records the end state: the gateway routes to the target.
func (e *Engine) statelessFinalize(context.Context) (map[string]any, error) {
	return map[string]any{"downtimeMillis": e.state.DowntimeMillis, "backend": e.plan.Gateway.TargetBackend}, nil
}

// onStatelessFailure applies the rollback of a stateless plan: whenever the
// gateway holds writes or routes to the target, route back and release,
// except where routing back after the cutover could lose users' work.
func (e *Engine) onStatelessFailure(step string, cause error) error {
	if !e.state.WritesHeld && !e.state.TrafficOnTarget {
		e.mu.Lock()
		e.state.Outcome = OutcomeFailed
		e.mu.Unlock()
		e.save()
		return fmt.Errorf("%s: %w", step, cause)
	}
	if e.state.TrafficOnTarget {
		reason := ""
		switch {
		case afterCutover(step) && errors.Is(cause, context.Canceled):
			reason = "cancelled after the cutover; traffic stays on the target. -resume-halted repeats the remaining steps"
		case e.plan.Files != nil:
			reason = "failed after the cutover with files: users may have written files to the target that the source lacks, so traffic stays on the target. -resume-halted routes back to the source; copy the target's new files to the source first if they matter"
		}
		if reason != "" {
			e.mu.Lock()
			e.state.Outcome = OutcomeHalted
			e.mu.Unlock()
			e.save()
			e.emit(step, EventHalt, reason, map[string]any{"writesHeld": e.state.WritesHeld, "trafficOnTarget": true})
			return fmt.Errorf("halted at %s: %w", step, cause)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.plan.Thresholds.SwitchoverTimeout.Duration+rollbackSlack)
	defer cancel()
	msg := "routing back to the source"
	if e.state.TrafficOnTarget {
		msg = "failed after the cutover: moving traffic back to the source (a second traffic flip)"
	}
	e.emit(step, EventRollback, msg, nil)
	rbErr := e.rollbackStateless(ctx)
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
		e.emit(step, EventHalt, "rollback failed: "+rbErr.Error(),
			map[string]any{"writesHeld": e.state.WritesHeld, "trafficOnTarget": e.state.TrafficOnTarget})
		return fmt.Errorf("%s failed (%v) and rollback failed: %w", step, cause, rbErr)
	}
	e.emit(step, EventRollback, "rolled back; source is serving traffic", nil)
	return fmt.Errorf("rolled back after %s: %w", step, cause)
}

// rollbackStateless routes the gateway to the source and releases the held
// writes there. Both calls are safe to repeat.
func (e *Engine) rollbackStateless(ctx context.Context) error {
	if err := e.gw.Route(ctx, e.plan.Gateway.SourceBackend); err != nil {
		return fmt.Errorf("route to source: %w", err)
	}
	e.setFlags(func(s *State) { s.TrafficOnTarget = false })
	if e.state.WritesHeld {
		if err := e.gw.ReleaseWrites(ctx); err != nil {
			return fmt.Errorf("release writes: %w", err)
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

// afterCutover reports whether step runs after the cutover released writes.
func afterCutover(step string) bool {
	return slices.Index(Steps, step) > slices.Index(Steps, StepCutover)
}

// resumeHaltedStateless finishes a halted stateless run. A run cancelled
// after the cutover carries on: Run repeats its remaining steps. Any other
// halt is rolled back: an interrupted fence, start-target or cutover may have
// held writes or moved the route without saving it, so both are undone.
func (e *Engine) resumeHaltedStateless(ctx context.Context, st State) error {
	if afterCutover(st.FailureStep) && st.TrafficOnTarget && !st.WritesHeld &&
		strings.Contains(st.FailureError, context.Canceled.Error()) {
		e.mu.Lock()
		e.state.Outcome = OutcomeRunning
		e.state.FailureStep, e.state.FailureError = "", ""
		e.mu.Unlock()
		e.save()
		e.log(st.FailureStep, "resumed after a cancel: traffic is on the target, repeating the remaining steps", nil)
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, e.plan.Thresholds.SwitchoverTimeout.Duration+rollbackSlack)
	defer cancel()
	for _, step := range []string{StepFence, StepStartTarget, StepCutover} {
		if st.Steps[step].Status == StatusRunning {
			e.setFlags(func(s *State) { s.WritesHeld = true })
		}
	}
	e.emit(st.FailureStep, EventRollback, "resume of a halted stateless run: routing back to the source", nil)
	if err := e.rollbackStateless(cctx); err != nil {
		e.setFlags(func(s *State) { s.RollbackError = err.Error() })
		return fmt.Errorf("rollback of halted run %s failed: %w", st.RunID, err)
	}
	e.mu.Lock()
	e.state.Outcome, e.state.RollbackError = OutcomeRolledBack, ""
	for _, rec := range e.state.Steps {
		if rec.Status == StatusRunning {
			rec.Status, rec.Error = StatusFailed, "interrupted; rolled back on resume"
		}
	}
	e.mu.Unlock()
	e.save()
	e.emit(st.FailureStep, EventRollback, "rolled back; source is serving traffic", nil)
	return fmt.Errorf("%w (halted at %s: %s)", ErrResumeRolledBackStateless, st.FailureStep, st.FailureError)
}
