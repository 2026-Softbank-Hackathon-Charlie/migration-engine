package engine

// An external source (ADR 0019) is a PostgreSQL primary outside CNPG, such as
// a compose app's database on a VM. The target CNPG Cluster was born from it
// with pg_basebackup and streams from it under the externalClusters name
// source.name. The steps below replace the ones that assume a CNPG source:
//
//	precheck    the source is a writable primary, the target follows source.name
//	fence       hold writes, make the source read-only, record its flush LSN
//	switchover  wait for that LSN, fence gate, promote the target without a token
//	start-target  REINDEX target.reindex, then start the cold app (ADR 0012)
//	verify      no source comparison (the source no longer follows), files one way
//	finalize    record both ends
//
// prepare, presync, delta and cutover are shared with the CNPG path.
//
// Rollback boundary: before the promotion request a failure makes the source
// writable again and releases the held writes to it. After it the run halts
// (ADR 0013) with the writes held and the source read-only; a resume carries
// on on the target. A promoted target is never demoted again: the source is
// not a CNPG Cluster and cannot follow it.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
)

var externalStepFuncs = map[string]stepFunc{
	StepPrecheck:    (*Engine).externalPrecheck,
	StepPrepare:     (*Engine).externalPrepare,
	StepFence:       (*Engine).externalFence,
	StepSwitchover:  (*Engine).externalSwitchover,
	StepStartTarget: (*Engine).externalStartTarget,
	StepVerify:      (*Engine).externalVerify,
	StepFinalize:    (*Engine).externalFinalize,
}

var errNoDemotionRollback = errors.New("rollback by demotion is not available for source.kind external (ADR 0019): the source is not a CNPG Cluster and cannot follow the promoted target")

func checkExternalTopology(source string, d *cnpg.Status) error {
	if d.Self == source {
		return fmt.Errorf("target replica.self %q is the external source's name", d.Self)
	}
	if d.ReplicaPrimary != source {
		return fmt.Errorf("target replica.primary=%q, want external source %q", d.ReplicaPrimary, source)
	}
	if d.ReplicaSource != source {
		return fmt.Errorf("target replica.source=%q, want external source %q", d.ReplicaSource, source)
	}
	return nil
}

// externalPrecheck: the source is a writable primary, the target exists (or
// has a manifest) and follows it, and the tools are available.
func (e *Engine) externalPrecheck(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	s, err := e.ext.Status(ctx)
	if err != nil {
		return ev, fmt.Errorf("source db: %w", err)
	}
	ev["source"] = s
	if s.InRecovery {
		return ev, errors.New("external source is in recovery (not writable)")
	}
	if s.ReadOnly {
		return ev, errors.New("external source has default_transaction_read_only on: an earlier fence may be left; run ALTER SYSTEM RESET default_transaction_read_only and pg_reload_conf() there once no run uses it")
	}
	if !s.Superuser {
		return ev, errors.New("source.dsn must log in as a superuser (the fence runs ALTER SYSTEM and ends other sessions)")
	}
	if !s.AllowAlterSystem {
		return ev, errors.New("external source has allow_alter_system off; the fence needs ALTER SYSTEM")
	}
	if len(s.ReadOnlyOverrides) > 0 {
		// A role or database setting beats the server-wide fence, so sessions
		// of that role would still write.
		return ev, fmt.Errorf("external source overrides default_transaction_read_only per role or database (%s); remove it with ALTER ROLE/DATABASE ... RESET default_transaction_read_only", strings.Join(s.ReadOnlyOverrides, ", "))
	}
	d, err := e.dst.Get(ctx)
	switch {
	case errors.Is(err, cnpg.ErrNotFound):
		if e.plan.Target.Manifest == "" {
			return ev, errors.New("target cluster does not exist and target.manifest is not set")
		}
		ev["target"] = "absent; manifest will be applied in prepare"
	case err != nil:
		return ev, err
	default:
		ev["target"] = d
		if err := checkExternalTopology(e.plan.Source.Name, d); err != nil {
			return ev, err
		}
	}
	if e.rclone != nil {
		if err := e.rclone.Available(); err != nil {
			return ev, err
		}
	}
	if err := e.gw.Health(ctx); err != nil {
		return ev, fmt.Errorf("gateway: %w", err)
	}
	a := e.plan.Target.App // cold (plan validation)
	n, err := e.app.Running(ctx)
	if err != nil {
		return ev, fmt.Errorf("target app %s/%s: %w", a.Kind, a.Name, err)
	}
	ev["targetApp"] = map[string]any{"mode": a.Mode, "kind": a.Kind, "name": a.Name, "podsNow": n}
	return ev, nil
}

// externalPrepare is prepare plus a check, on the replica, that every
// target.reindex entry names an index, so a typo fails before writes are held.
func (e *Engine) externalPrepare(ctx context.Context) (map[string]any, error) {
	ev, err := e.prepare(ctx)
	if err != nil || len(e.plan.Target.Reindex) == 0 {
		return ev, err
	}
	db, err := e.targetDB(ctx)
	if err != nil {
		return ev, err
	}
	for _, name := range e.plan.Target.Reindex {
		ok, err := db.IsIndex(ctx, name)
		if err != nil {
			return ev, fmt.Errorf("target.reindex %s: %w", name, err)
		}
		if !ok {
			return ev, fmt.Errorf("target.reindex %s is not an index on the target", name)
		}
	}
	ev["reindexChecked"] = len(e.plan.Target.Reindex)
	return ev, nil
}

// externalFence: hold writes at the gateway, make the source refuse writes,
// record its flush LSN and copy the last files.
func (e *Engine) externalFence(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	if err := e.gw.HoldWrites(ctx); err != nil {
		return ev, fmt.Errorf("hold writes: %w", err)
	}
	now := time.Now().UTC()
	e.setFlags(func(s *State) { s.WritesHeld, s.WritesHeldAt = true, &now })
	ev["writesHeldAt"] = now
	e.log(StepFence, "writes held at gateway", nil)

	// The fence ends every client session but its own, the engine's too: its
	// pool is closed here and reopened afterwards (read-only).
	if e.srcDB != nil {
		e.srcDB.Close()
		e.srcDB = nil
	}
	// The flag goes first: a fence whose answer was lost may still have been
	// applied, and the rollback undoes it either way.
	e.setFlags(func(s *State) { s.SourceFenced = true })
	f, err := e.ext.Fence(ctx)
	ev["terminatedSessions"] = f.Terminated
	if err != nil {
		return ev, fmt.Errorf("fence source: %w", err)
	}
	e.setFlags(func(s *State) { s.FenceLSN, s.FenceXid = f.LSN, f.NextXid })
	ev["fenceLsn"], ev["fenceXid"] = f.LSN, f.NextXid
	e.log(StepFence, "source read-only", map[string]any{"fenceLsn": f.LSN, "fenceXid": f.NextXid, "terminatedSessions": f.Terminated})

	if e.rclone != nil {
		r, err := e.rclone.Sync(ctx, e.plan.Files.Source, e.plan.Files.Target)
		ev["filesSync"] = r
		if err != nil {
			return ev, err
		}
	}
	return ev, nil
}

// externalSwitchover: wait for the target to replay the fence LSN, compare
// data while both sides are frozen, then promote the target. CNPG promotes a
// replica that names itself primary without a promotion token; the engine
// sends none (SetPrimary removes the field).
func (e *Engine) externalSwitchover(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{"fenceLsn": e.state.FenceLSN}
	tctx, cancel := context.WithTimeout(ctx, e.plan.Thresholds.SwitchoverTimeout.Duration)
	defer cancel()

	if _, err := e.sourceDB(tctx); err != nil {
		return ev, err
	}
	if _, err := e.targetDB(tctx); err != nil {
		return ev, err
	}
	timing := map[string]int64{}
	ev["timingMillis"] = timing
	mark := time.Now()
	lap := func(name string) {
		now := time.Now()
		timing[name] = now.Sub(mark).Milliseconds()
		mark = now
	}
	replayed, err := e.waitReplayed(tctx, e.state.FenceLSN)
	ev["targetReplayLsn"] = replayed
	lap("replayFence")
	if err != nil {
		return ev, fmt.Errorf("target did not reach fence LSN %s: %w", e.state.FenceLSN, err)
	}

	if *e.plan.Verify.FenceGate {
		start := time.Now()
		results, ok, err := e.verifier.FenceGate(tctx, e.srcDB, e.dstDB)
		if err != nil {
			return ev, fmt.Errorf("fence gate: %w", err)
		}
		ev["fenceGate"] = map[string]any{"tables": results, "ok": ok, "durationMillis": time.Since(start).Milliseconds()}
		if !ok {
			return ev, fmt.Errorf("fence gate: data mismatch in %s", mismatched(results))
		}
		e.log(StepSwitchover, "fence gate passed", map[string]any{"tables": len(results)})
	}
	lap("fenceGate")

	dstS, err := e.dst.Get(tctx)
	if err != nil {
		return ev, err
	}
	// The fence stops writes by default only: a session can still ask for a
	// read-write transaction. A write since the fence would not be on the
	// target, so check right before the point of no return.
	src, err := e.ext.Status(tctx)
	ev["sourceBeforePromotion"] = src
	if err != nil {
		return ev, fmt.Errorf("re-check the fenced source: %w", err)
	}
	switch {
	case !src.ReadOnly:
		return ev, errors.New("external source takes writes again (default_transaction_read_only is off)")
	case e.state.FenceXid == "" || src.NextXid != e.state.FenceXid:
		return ev, fmt.Errorf("external source assigned transaction ids after the fence (next xid %s, at the fence %s): something wrote", src.NextXid, e.state.FenceXid)
	case src.WriteSessions > 0:
		return ev, fmt.Errorf("external source has %d client sessions with an open write transaction", src.WriteSessions)
	}
	lap("sourceRecheck")
	// From here on there is no automatic rollback.
	e.setFlags(func(s *State) { s.PromoteRequested = true })
	if err := e.dst.SetPrimary(tctx, dstS.Self, ""); err != nil {
		return ev, fmt.Errorf("promote target: %w", err)
	}
	e.log(StepSwitchover, "target promotion requested", map[string]any{"oldPrimary": e.plan.Source.Name})
	if err := e.waitWritable(tctx, e.plan.Target.DSN, &e.dstDB); err != nil {
		lap("promoteToWritable")
		return ev, fmt.Errorf("target did not become writable: %w", err)
	}
	lap("promoteToWritable")
	e.log(StepSwitchover, "target writable", map[string]any{"afterPromotionMillis": timing["promoteToWritable"]})
	if pos, err := e.dstDB.WALPosition(tctx); err == nil {
		ev["targetLsnAfterPromotion"] = pos
	}
	// CNPG restarts the new primary once for archive_mode (ADR 0009).
	settle, err := e.waitSettled(tctx, e.dst, e.plan.Target.DSN, &e.dstDB)
	lap("settle")
	ev["settle"] = settle
	ev["settleMillis"] = timing["settle"]
	if err != nil {
		return ev, fmt.Errorf("target did not settle after promotion: %w", err)
	}
	e.log(StepSwitchover, "target settled", map[string]any{"settleMillis": timing["settle"], "polls": settle.Polls,
		"unobserved": settle.Unobserved, "restarts": settle.Restarts, "notHealthy": settle.NotHealthy,
		"inRecovery": settle.InRecovery, "pendingRestart": settle.PendingRestart, "lastBlockedMillis": settle.LastBlockedMillis})
	return ev, nil
}

// externalStartTarget rebuilds target.reindex on the settled primary (a text
// index copied from a source with another C library can be corrupt), one at
// a time, then starts the cold app. It runs again whole on a resume; REINDEX
// is safe to repeat.
func (e *Engine) externalStartTarget(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	if names := e.plan.Target.Reindex; len(names) > 0 {
		start := time.Now()
		db, err := e.targetDB(ctx)
		if err != nil {
			return ev, err
		}
		done := make([]map[string]any, 0, len(names))
		for _, name := range names {
			t := time.Now()
			if err := db.ReindexIndex(ctx, name); err != nil {
				ev["reindex"] = done
				return ev, fmt.Errorf("reindex %s: %w", name, err)
			}
			done = append(done, map[string]any{"index": name, "millis": time.Since(t).Milliseconds()})
		}
		ev["reindex"] = done
		ev["reindexMillis"] = time.Since(start).Milliseconds()
		e.log(StepStartTarget, "indexes rebuilt", map[string]any{"indexes": len(names), "millis": ev["reindexMillis"]})
	}
	app, err := e.startTarget(ctx)
	for k, v := range app {
		ev[k] = v
	}
	return ev, err
}

// externalVerify: the source stays frozen at the fence and no longer follows
// the target, so the fence gate was the database comparison. This checks the
// k6 writes on the target and that every source object is on the target.
func (e *Engine) externalVerify(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	wctx, cancel := context.WithTimeout(ctx, e.plan.Thresholds.CatchupTimeout.Duration)
	err := e.waitWritable(wctx, e.plan.Target.DSN, &e.dstDB)
	cancel()
	if err != nil {
		return ev, fmt.Errorf("target not reachable as primary: %w", err)
	}
	rep, err := e.verifier.Report(ctx, e.srcDB, e.dstDB)
	if err != nil {
		return ev, err
	}
	ev["report"] = rep
	if !rep.OK {
		return ev, fmt.Errorf("verification failed: %s", strings.Join(rep.Problems, "; "))
	}
	return ev, nil
}

// externalFinalize records the end state: the target is the primary, the
// source is left read-only for the platform to retire.
func (e *Engine) externalFinalize(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{"downtimeMillis": e.state.DowntimeMillis}
	if s, err := e.ext.Status(ctx); err == nil {
		ev["source"] = s
	} else {
		ev["sourceError"] = err.Error()
	}
	d, err := e.dst.Get(ctx)
	if err != nil {
		return ev, err
	}
	ev["target"] = d
	if !d.IsPrimary() {
		return ev, fmt.Errorf("target is not the topology primary (replica.primary=%q)", d.ReplicaPrimary)
	}
	return ev, nil
}

// onExternalFailure applies the rollback boundary of an external source.
func (e *Engine) onExternalFailure(step string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), e.plan.Thresholds.SwitchoverTimeout.Duration+rollbackSlack)
	defer cancel()
	switch {
	case e.state.PromoteRequested:
		e.mu.Lock()
		e.state.Outcome = OutcomeHalted
		e.mu.Unlock()
		e.save()
		e.emit(step, EventHalt, "target promotion was requested; the external source stays read-only and writes stay held. Operator action required (resume continues on the target).",
			map[string]any{"writesHeld": e.state.WritesHeld, "trafficOnTarget": e.state.TrafficOnTarget})
		return fmt.Errorf("halted at %s: %w", step, cause)

	case e.state.WritesHeld || e.state.SourceFenced:
		e.emit(step, EventRollback, "rolling back to the external source", nil)
		rbErr := e.rollbackExternal(ctx)
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
		e.emit(step, EventRollback, "rolled back; source is writable and serving traffic", nil)
		return fmt.Errorf("rolled back after %s: %w", step, cause)

	default:
		e.mu.Lock()
		e.state.Outcome = OutcomeFailed
		e.mu.Unlock()
		e.save()
		return fmt.Errorf("%s: %w", step, cause)
	}
}

// rollbackExternal makes the source writable again and releases the held
// writes to it. Only valid while the target has not been asked to promote.
func (e *Engine) rollbackExternal(ctx context.Context) error {
	if e.state.SourceFenced {
		if err := e.ext.Unfence(ctx); err != nil {
			return fmt.Errorf("unfence source: %w", err)
		}
		// The reload reaches new sessions a moment later.
		for {
			s, err := e.ext.Status(ctx)
			if err == nil && !s.ReadOnly && !s.InRecovery {
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("source does not take writes again after unfence (last error: %v)", err)
			case <-time.After(e.poll / 4):
			}
		}
		e.setFlags(func(s *State) { s.SourceFenced = false })
		e.log(StepFence, "source writable again", nil)
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

// ErrResumeRolledBack is returned by ResumeHalted when it finished a halted
// external-source run by rolling it back (ADR 0019): the run is over.
var ErrResumeRolledBack = errors.New("halted run rolled back to the external source")

// resumeHaltedExternal is ResumeHalted for an external source.
//
// Before the promotion request (an interrupted process, or a failed
// rollback) it finishes the rollback: unfence the source, route and release
// the held writes to it. An interrupted fence may have held writes or fenced
// the source without saving it, so both are undone; both are safe to repeat.
//
// After it, the target must be the topology primary, writable and settled,
// and the source still read-only, so nothing else takes writes. An
// interrupted start-target runs again (REINDEX and scaling repeat safely);
// an interrupted cutover still needs an operator.
func (e *Engine) resumeHaltedExternal(ctx context.Context, st State) error {
	if !st.PromoteRequested {
		cctx, cancel := context.WithTimeout(ctx, e.plan.Thresholds.SwitchoverTimeout.Duration+rollbackSlack)
		defer cancel()
		if st.Steps[StepFence].Status == StatusRunning {
			e.setFlags(func(s *State) { s.WritesHeld, s.SourceFenced = true, true })
		}
		if !e.state.WritesHeld && !e.state.SourceFenced {
			return fmt.Errorf("run %s halted before the fence (%s: %s); nothing to resume or roll back", st.RunID, st.FailureStep, st.FailureError)
		}
		e.emit(st.FailureStep, EventRollback, "resume of a run halted before promotion: rolling back to the external source", nil)
		if err := e.rollbackExternal(cctx); err != nil {
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
		e.emit(st.FailureStep, EventRollback, "rolled back; source is writable and serving traffic", nil)
		return fmt.Errorf("%w (halted at %s: %s)", ErrResumeRolledBack, st.FailureStep, st.FailureError)
	}
	if st.Steps[StepCutover].Status == StatusRunning {
		return fmt.Errorf("interrupted %s requires operator reconciliation; refusing automatic resume", StepCutover)
	}
	if !st.SourceFenced {
		return fmt.Errorf("run %s halted after promotion without a fenced source; this needs an operator", st.RunID)
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
	s, err := e.ext.Status(cctx)
	if err != nil {
		return fmt.Errorf("cannot confirm the external source is still read-only: %w", err)
	}
	if !s.ReadOnly {
		return errors.New("external source takes writes again (default_transaction_read_only is off); not resuming")
	}
	ev["source"] = s
	if err := e.waitWritable(cctx, e.plan.Target.DSN, &e.dstDB); err != nil {
		return fmt.Errorf("target is not writable: %w", err)
	}
	rep, err := e.waitSettled(cctx, e.dst, e.plan.Target.DSN, &e.dstDB)
	ev["settle"] = rep
	if err != nil {
		return fmt.Errorf("target did not settle: %w", err)
	}
	if st.Steps[StepStartTarget].Status == StatusRunning {
		// Run would halt again on the interrupted step; it is repeated instead.
		e.mu.Lock()
		rec := e.state.Steps[StepStartTarget]
		rec.Status, rec.Error = StatusFailed, "interrupted; repeated on resume"
		e.mu.Unlock()
	}
	e.markResumed(st, ev, "resumed after halt: target is primary and settled, external source is read-only")
	return nil
}
