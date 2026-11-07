package engine

import (
	"context"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/loadguard"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/verify"
)

func endpointScope(e plan.Endpoint) string { return e.Env + "/" + e.Namespace + "/" + e.Cluster }

func (e *Engine) configureLoad(p *plan.Plan) error {
	if p.Stateless() {
		return nil // no database to protect (ADR 0018)
	}
	policy, err := loadguard.FromEnv()
	if err != nil {
		return err
	}
	if policy == nil {
		return nil
	}
	callback := func(s loadguard.Snapshot) {
		e.emit("", EventMetric, "DB load budget: "+s.Status+" "+s.Reason, map[string]any{"loadBudget": s})
	}
	e.sourceLoad, err = loadguard.New(policy, endpointScope(p.Source), callback)
	if err != nil {
		return err
	}
	e.targetLoad, err = loadguard.New(policy, endpointScope(p.Target), callback)
	return err
}

func scanOptions(c *loadguard.Controller) verify.ScanOptions {
	if c == nil {
		return verify.ScanOptions{}
	}
	return verify.ScanOptions{BatchSize: c.Policy.BatchSize, BatchPause: time.Duration(c.Policy.BatchPauseMillis) * time.Millisecond, QueryTimeout: time.Duration(c.Policy.QueryTimeoutMillis) * time.Millisecond, BeforeBatch: c.Wait}
}

func (e *Engine) guardedStep(ctx context.Context, name string) (map[string]any, error) {
	var guard *loadguard.Controller
	switch name {
	case StepPrecheck, StepPrepare, StepPresync, StepDelta, StepFence:
		guard = e.sourceLoad
	case StepVerify:
		guard = e.targetLoad
	}
	step := e.stepFunc(name)
	if guard == nil {
		return step(e, ctx)
	}
	var evidence map[string]any
	var err error
	if name == StepFence {
		// Admission happens BEFORE HoldWrites. Never wait for metrics recovery
		// in the critical promotion/cutover sequence or extend a write hold.
		admit, cancel := context.WithTimeout(ctx, time.Duration(guard.Policy.MaxWaitSeconds)*time.Second)
		err = guard.Wait(admit)
		if err == nil && e.targetLoad != nil {
			err = e.targetLoad.Wait(admit)
		}
		if err == nil {
			err = guard.CheckNow(admit)
		}
		if err == nil && e.targetLoad != nil {
			err = e.targetLoad.CheckNow(admit)
		}
		cancel()
		if err == nil {
			evidence, err = step(e, ctx)
		}
	} else {
		err = guard.Do(ctx, func(work context.Context) error {
			var stepErr error
			evidence, stepErr = step(e, work)
			return stepErr
		})
	}
	if evidence == nil {
		evidence = map[string]any{}
	}
	evidence["loadBudget"] = guard.Last()
	if name == StepFence && e.targetLoad != nil {
		evidence["targetLoadBudget"] = e.targetLoad.Last()
	}
	return evidence, err
}
