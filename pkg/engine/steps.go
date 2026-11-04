package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/verify"
)

// precheck: both sides reachable, source is a healthy writable primary, the
// distributed topology is wired both ways, and the tools are available.
func (e *Engine) precheck(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	s, err := e.src.Get(ctx)
	if err != nil {
		return ev, err
	}
	ev["source"] = s
	if !s.Healthy() {
		return ev, fmt.Errorf("source cluster not healthy: phase=%q ready=%d/%d", s.Phase, s.ReadyInstances, s.Instances)
	}
	if !s.IsPrimary() {
		return ev, fmt.Errorf("source cluster is not the topology primary (replica.primary=%q, self=%q)", s.ReplicaPrimary, s.Self)
	}
	if s.ReplicaSource == "" {
		return ev, errors.New("source spec.replica.source is empty: it must name the target's externalCluster so the source can follow the target after demotion")
	}

	db, err := e.sourceDB(ctx)
	if err != nil {
		return ev, fmt.Errorf("source db: %w", err)
	}
	if rec, err := db.InRecovery(ctx); err != nil {
		return ev, err
	} else if rec {
		return ev, errors.New("source database is in recovery (not writable)")
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
		if err := checkTopology(s, d); err != nil {
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
	return ev, nil
}

func checkTopology(src, dst *cnpg.Status) error {
	if dst.Self == src.Self {
		return fmt.Errorf("source and target share replica.self %q", src.Self)
	}
	if dst.ReplicaPrimary != src.Self {
		return fmt.Errorf("target replica.primary=%q, want source self %q", dst.ReplicaPrimary, src.Self)
	}
	if dst.ReplicaSource != src.Self {
		return fmt.Errorf("target replica.source=%q, want source externalCluster %q", dst.ReplicaSource, src.Self)
	}
	if src.ReplicaSource != dst.Self {
		return fmt.Errorf("source replica.source=%q, want target externalCluster %q", src.ReplicaSource, dst.Self)
	}
	return nil
}

// prepare: create the target replica cluster if needed and wait until it streams.
func (e *Engine) prepare(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	_, err := e.dst.Get(ctx)
	if errors.Is(err, cnpg.ErrNotFound) {
		e.log(StepPrepare, "applying target manifest", map[string]any{"manifest": e.plan.Target.Manifest})
		if err := e.dst.ApplyManifest(ctx, e.plan.Target.Manifest); err != nil {
			return ev, err
		}
		ev["applied"] = e.plan.Target.Manifest
	} else if err != nil {
		return ev, err
	}

	wctx, cancel := context.WithTimeout(ctx, e.plan.Thresholds.CatchupTimeout.Duration)
	defer cancel()
	d, err := e.dst.WaitFor(wctx, e.poll, (*cnpg.Status).Healthy)
	if err != nil {
		return ev, fmt.Errorf("target cluster not healthy: %w", err)
	}
	ev["target"] = d
	s, err := e.src.Get(ctx)
	if err != nil {
		return ev, err
	}
	if err := checkTopology(s, d); err != nil {
		return ev, err
	}

	db, err := e.targetDB(ctx)
	if err != nil {
		return ev, fmt.Errorf("target db: %w", err)
	}
	rec, err := db.InRecovery(ctx)
	if err != nil {
		return ev, err
	}
	if !rec {
		return ev, errors.New("target database is writable; it must be a replica before migration")
	}
	return ev, nil
}

// presync: wait for the initial copy to catch up and do the first file copy.
func (e *Engine) presync(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	lag, err := e.waitCaughtUp(ctx, StepPresync, e.plan.Thresholds.CatchupTimeout.Duration)
	ev["lagBytes"] = lag
	if err != nil {
		return ev, err
	}
	if e.rclone != nil {
		r, err := e.rclone.Sync(ctx, e.plan.Files.Source, e.plan.Files.Target)
		ev["filesSync"] = r
		if err != nil {
			return ev, err
		}
	}
	return ev, nil
}

// delta: sync files changed since presync and confirm lag is small, so the
// write fence that follows is short.
func (e *Engine) delta(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	if e.rclone != nil {
		r, err := e.rclone.Sync(ctx, e.plan.Files.Source, e.plan.Files.Target)
		ev["filesSync"] = r
		if err != nil {
			return ev, err
		}
	}
	lag, err := e.waitCaughtUp(ctx, StepDelta, e.plan.Thresholds.CatchupTimeout.Duration)
	ev["lagBytes"] = lag
	return ev, err
}

// fence: hold writes at the gateway, record the source's final LSN, and copy
// the last files. Downtime for writes starts here.
func (e *Engine) fence(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	if err := e.gw.HoldWrites(ctx); err != nil {
		return ev, fmt.Errorf("hold writes: %w", err)
	}
	now := time.Now().UTC()
	e.setFlags(func(s *State) { s.WritesHeld, s.WritesHeldAt = true, &now })
	ev["writesHeldAt"] = now
	e.log(StepFence, "writes held at gateway", nil)

	src, err := e.sourceDB(ctx)
	if err != nil {
		return ev, err
	}
	lsn, err := src.WALPosition(ctx)
	if err != nil {
		return ev, fmt.Errorf("read fence LSN: %w", err)
	}
	e.setFlags(func(s *State) { s.FenceLSN = lsn })
	ev["fenceLsn"] = lsn

	if e.rclone != nil {
		r, err := e.rclone.Sync(ctx, e.plan.Files.Source, e.plan.Files.Target)
		ev["filesSync"] = r
		if err != nil {
			return ev, err
		}
	}
	return ev, nil
}

// switchover: wait for the target to replay the fence LSN, compare data while
// both sides are frozen, then demote the source and promote the target with
// the demotion token (CNPG promotes only after replaying up to it).
func (e *Engine) switchover(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{"fenceLsn": e.state.FenceLSN}
	tctx, cancel := context.WithTimeout(ctx, e.plan.Thresholds.SwitchoverTimeout.Duration)
	defer cancel()

	// 1. Target catches up to the fence point.
	if _, err := e.sourceDB(tctx); err != nil {
		return ev, err
	}
	if _, err := e.targetDB(tctx); err != nil {
		return ev, err
	}
	replayed, err := e.waitReplayed(tctx, e.state.FenceLSN)
	ev["targetReplayLsn"] = replayed
	if err != nil {
		return ev, fmt.Errorf("target did not reach fence LSN %s: %w", e.state.FenceLSN, err)
	}

	// 2. Gate: both sides frozen at the same point must be identical.
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

	// 3. Demote source; CNPG publishes a demotion token.
	srcS, err := e.src.Get(tctx)
	if err != nil {
		return ev, err
	}
	dstS, err := e.dst.Get(tctx)
	if err != nil {
		return ev, err
	}
	e.tokenBeforeDemote = srcS.DemotionToken
	e.setFlags(func(s *State) { s.SourceDemoted = true })
	if err := e.src.SetPrimary(tctx, dstS.Self, ""); err != nil {
		// The flag is set before the request because a request that times out
		// may still have been applied. When the source still names itself as
		// primary the request was not applied and CNPG never started demoting:
		// the rollback can release writes on it without waiting for a token.
		if s, gerr := e.src.Get(ctx); gerr == nil && s.IsPrimary() && s.DemotionToken == e.tokenBeforeDemote {
			e.setFlags(func(s *State) { s.SourceDemoted = false })
			ev["demotionApplied"] = false
		}
		return ev, fmt.Errorf("demote source: %w", err)
	}
	e.log(StepSwitchover, "source demotion requested", map[string]any{"newPrimary": dstS.Self})
	demoted, err := e.src.WaitFor(tctx, e.poll/2, e.demotionFinished)
	if err != nil {
		return ev, fmt.Errorf("waiting for demotion token: %w", err)
	}
	ev["demotionToken"] = true

	// 4. Promote target. From here on there is no automatic rollback.
	e.setFlags(func(s *State) { s.PromoteRequested = true })
	if err := e.dst.SetPrimary(tctx, dstS.Self, demoted.DemotionToken); err != nil {
		return ev, fmt.Errorf("promote target: %w", err)
	}
	e.log(StepSwitchover, "target promotion requested", map[string]any{"oldPrimary": srcS.Self})
	if err := e.waitWritable(tctx, e.plan.Target.DSN, &e.dstDB); err != nil {
		return ev, fmt.Errorf("target did not become writable: %w", err)
	}
	if pos, err := e.dstDB.WALPosition(tctx); err == nil {
		ev["targetLsnAfterPromotion"] = pos
	}
	// 5. Writes stay held until CNPG has finished with the new primary.
	start := time.Now()
	if err := e.waitSettled(tctx, e.dst, e.plan.Target.DSN, &e.dstDB); err != nil {
		return ev, fmt.Errorf("target did not settle after promotion: %w", err)
	}
	ev["settleMillis"] = time.Since(start).Milliseconds()
	return ev, nil
}

// cutover: route all traffic to the target and release held writes there.
func (e *Engine) cutover(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{"backend": e.plan.Gateway.TargetBackend}
	if err := e.gw.Route(ctx, e.plan.Gateway.TargetBackend); err != nil {
		return ev, fmt.Errorf("route to target: %w", err)
	}
	e.setFlags(func(s *State) { s.TrafficOnTarget = true })
	if err := e.gw.ReleaseWrites(ctx); err != nil {
		return ev, fmt.Errorf("release writes: %w", err)
	}
	var downtime time.Duration
	if e.state.WritesHeldAt != nil {
		downtime = time.Since(*e.state.WritesHeldAt)
	}
	e.setFlags(func(s *State) { s.WritesHeld, s.DowntimeMillis = false, downtime.Milliseconds() })
	budget := e.plan.Thresholds.MaxDowntime.Duration
	ev["downtimeMillis"] = downtime.Milliseconds()
	ev["maxDowntimeMillis"] = budget.Milliseconds()
	ev["withinBudget"] = downtime <= budget
	e.emit(StepCutover, EventMetric, "write downtime", map[string]any{"downtimeMillis": downtime.Milliseconds()})
	if downtime > budget {
		// Not a failure: the switch already happened; the evidence records the miss.
		e.log(StepCutover, fmt.Sprintf("WARNING: downtime %s exceeded budget %s", downtime.Round(time.Millisecond), budget), nil)
	}
	return ev, nil
}

// verify: nothing from the source is missing on the target, every write k6
// saw succeed exists, and every source object exists on the target.
func (e *Engine) verify(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	// The old source restarts as a replica after demotion; a connection error
	// while it does is not a verification result. Wait until both sides are up
	// in their new roles.
	wctx, cancel := context.WithTimeout(ctx, e.plan.Thresholds.CatchupTimeout.Duration)
	err := e.waitRecovery(wctx, e.plan.Source.DSN, &e.srcDB, true)
	if err == nil {
		err = e.waitWritable(wctx, e.plan.Target.DSN, &e.dstDB)
	}
	cancel()
	if err != nil {
		return ev, fmt.Errorf("databases not reachable in their new roles: %w", err)
	}
	src, err := e.sourceDB(ctx)
	if err != nil {
		return ev, err
	}
	dst, err := e.targetDB(ctx)
	if err != nil {
		return ev, err
	}
	rep, err := e.verifier.Report(ctx, src, dst)
	if err != nil {
		return ev, err
	}
	ev["report"] = rep
	if !rep.OK {
		return ev, fmt.Errorf("verification failed: %s", strings.Join(rep.Problems, "; "))
	}
	return ev, nil
}

// finalize: record the end state of both clusters. The old source stays as a
// read-only replica following the new primary.
func (e *Engine) finalize(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{"downtimeMillis": e.state.DowntimeMillis}
	if s, err := e.src.Get(ctx); err == nil {
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

// --- helpers ---

func (e *Engine) sourceDB(ctx context.Context) (Database, error) {
	if e.srcDB == nil {
		db, err := e.connect(ctx, e.plan.Source.DSN)
		if err != nil {
			return nil, err
		}
		e.srcDB = db
	}
	return e.srcDB, nil
}

func (e *Engine) targetDB(ctx context.Context) (Database, error) {
	if e.dstDB == nil {
		db, err := e.connect(ctx, e.plan.Target.DSN)
		if err != nil {
			return nil, err
		}
		e.dstDB = db
	}
	return e.dstDB, nil
}

func (e *Engine) selfName(ctx context.Context, c Cluster) (string, error) {
	s, err := c.Get(ctx)
	if err != nil {
		return "", err
	}
	return s.Self, nil
}

// waitCaughtUp polls replication lag until it is within MaxLagBytes.
func (e *Engine) waitCaughtUp(ctx context.Context, step string, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	src, err := e.sourceDB(ctx)
	if err != nil {
		return -1, err
	}
	dst, err := e.targetDB(ctx)
	if err != nil {
		return -1, err
	}
	var last int64 = -1
	var lastErr error
	for {
		lag, sl, dl, err := measureLag(ctx, src, dst)
		if err == nil {
			last = lag
			e.emit(step, EventMetric, "replication lag", map[string]any{"lagBytes": lag, "sourceLsn": sl, "targetLsn": dl})
			if lag <= e.plan.Thresholds.MaxLagBytes {
				return lag, nil
			}
		} else {
			lastErr = err
			e.log(step, "lag check failed: "+err.Error(), nil)
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("lag %d > %d bytes after %s (last error: %v)", last, e.plan.Thresholds.MaxLagBytes, timeout, lastErr)
		case <-time.After(e.poll):
		}
	}
}

// waitReplayed polls until the target's replay LSN is at or past lsn.
func (e *Engine) waitReplayed(ctx context.Context, lsn string) (string, error) {
	if lsn == "" {
		return "", errors.New("no fence LSN recorded")
	}
	var pos string
	for {
		p, err := e.dstDB.WALPosition(ctx)
		if err == nil {
			pos = p
			if d, err := e.dstDB.LSNDiff(ctx, p, lsn); err == nil && d >= 0 {
				return pos, nil
			}
		}
		select {
		case <-ctx.Done():
			return pos, ctx.Err()
		case <-time.After(e.poll / 8):
		}
	}
}

// demotionFinished reports whether the source published a demotion token from
// this run's demotion, not one left over from an earlier demotion.
func (e *Engine) demotionFinished(s *cnpg.Status) bool {
	return s.DemotionToken != "" && s.DemotionToken != e.tokenBeforeDemote
}

// settleMin is how long a promoted target must look settled before writes are
// released. CNPG decides on a post-promotion restart a few hundred milliseconds
// after the promotion. A variable so tests can shorten it.
var settleMin = 3 * time.Second

// waitSettled waits until a promoted cluster is healthy, writable and has no
// setting pending a restart, for at least settleMin and two polls in a row.
// CNPG restarts a new primary in place when promotion changed a setting such as
// archive_mode; that restart is a smart shutdown, which waits for every client
// session, so the engine drops its own pool and checks with short connections.
func (e *Engine) waitSettled(ctx context.Context, c Cluster, dsn string, slot *Database) error {
	if *slot != nil {
		(*slot).Close()
		*slot = nil
	}
	start, stable := time.Now(), 0
	for {
		ok := false
		if s, err := c.Get(ctx); err == nil && s.Healthy() {
			if db, err := e.connect(ctx, dsn); err == nil {
				rec, err1 := db.InRecovery(ctx)
				pending, err2 := db.PendingRestart(ctx)
				db.Close()
				ok = err1 == nil && err2 == nil && !rec && !pending
			}
		}
		if ok {
			stable++
		} else {
			stable = 0
		}
		if stable >= 2 && time.Since(start) >= settleMin {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.poll / 4):
		}
	}
}

// waitWritable polls until the database at dsn is out of recovery,
// reconnecting as needed (pods may restart during promotion).
func (e *Engine) waitWritable(ctx context.Context, dsn string, slot *Database) error {
	return e.waitRecovery(ctx, dsn, slot, false)
}

// waitRecovery polls until the database at dsn reports pg_is_in_recovery() == want.
func (e *Engine) waitRecovery(ctx context.Context, dsn string, slot *Database, want bool) error {
	for {
		if *slot == nil {
			if db, err := e.connect(ctx, dsn); err == nil {
				*slot = db
			}
		}
		if *slot != nil {
			rec, err := (*slot).InRecovery(ctx)
			if err == nil && rec == want {
				return nil
			}
			if err != nil {
				(*slot).Close()
				*slot = nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.poll / 4):
		}
	}
}

func mismatched(rs []verify.TableResult) string {
	var names []string
	for _, r := range rs {
		if !r.Match {
			names = append(names, r.Table)
		}
	}
	return strings.Join(names, ", ")
}
