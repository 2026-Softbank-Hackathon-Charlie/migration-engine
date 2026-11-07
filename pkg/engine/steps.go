package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/verify"
)

// precheck: both sides reachable, source is a healthy writable primary, the
// distributed topology is wired both ways, and the tools are available.
func (e *Engine) precheck(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	if e.plan.SourceWriteFence != nil {
		if _, ok := e.gw.(sourceFenceGateway); !ok {
			return ev, errors.New("gateway cannot report source read drain")
		}
		if err := e.prepareSourceWriteFence(ctx); err != nil {
			return ev, fmt.Errorf("source SQL write fence precheck: %w", err)
		}
		ev["sourceWriteFenceConfigured"] = true
	}
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
	if s.ReplicaSource == "" && !e.plan.AllowSourceRewire {
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
		if err := e.checkPreparedTopology(s, d); err != nil {
			return ev, err
		}
		if e.plan.AllowSourceRewire {
			if err := e.checkSourceRewireCapability(s, d); err != nil {
				return ev, err
			}
			target, err := e.targetDB(ctx)
			if err != nil {
				return ev, fmt.Errorf("target db: %w", err)
			}
			if rec, err := target.InRecovery(ctx); err != nil {
				return ev, err
			} else if !rec {
				return ev, errors.New("target database is writable; source rewire requires a replica")
			}
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
	if a := e.plan.Target.App; a.Cold() {
		// Reading the workload proves it exists and the engine may see it.
		n, err := e.app.Running(ctx)
		if err != nil {
			return ev, fmt.Errorf("target app %s/%s: %w", a.Kind, a.Name, err)
		}
		ev["targetApp"] = map[string]any{"mode": a.Mode, "kind": a.Kind, "name": a.Name, "podsNow": n}
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
	var s *cnpg.Status
	if e.plan.Source.External() {
		if err := checkExternalTopology(e.plan.Source.Name, d); err != nil {
			return ev, err
		}
	} else {
		if s, err = e.src.Get(ctx); err != nil {
			return ev, err
		}
		if err := e.checkPreparedTopology(s, d); err != nil {
			return ev, err
		}
	}

	// Right after CNPG reports the new cluster healthy its service may still
	// refuse a connection or two (seen in the platform E2E: "tls error: EOF").
	// Retry until catchupTimeout instead of failing the run on one attempt.
	rec, err := e.readRecovery(wctx, e.plan.Target.DSN, &e.dstDB)
	if err != nil {
		return ev, fmt.Errorf("target db: %w", err)
	}
	if !rec {
		return ev, errors.New("target database is writable; it must be a replica before migration")
	}
	if e.plan.AllowSourceRewire {
		if err := e.prepareSourceRewire(wctx, s, d, ev); err != nil {
			return ev, err
		}
	}
	// Replication to the old source after cutover is asynchronous, so the new
	// primary is the only copy of the latest writes until it streams them. One
	// instance means one disk and one node (ADR 0013). Not a failure: the demo
	// clusters run one instance on purpose.
	if d.Instances < 2 {
		w := fmt.Sprintf("target cluster runs %d instance(s): until the old source has streamed them, writes after cutover live on one node only; use instances >= 2 (with synchronous replication) where losing that node is not acceptable", d.Instances)
		ev["warnings"] = []string{w}
		e.log(StepPrepare, "WARNING: "+w, nil)
	}
	if a := e.plan.Target.App; a.Cold() {
		// A cold app must not run on the replica (it would fail or crash-loop
		// on its start-up schema change); it starts after promotion.
		if err := e.app.Stop(ctx); err != nil {
			return ev, fmt.Errorf("stop target app: %w", err)
		}
		if err := e.waitAppStopped(ctx, a.StartTimeout.Duration); err != nil {
			return ev, err
		}
		ev["targetApp"] = "stopped until the target is promoted"
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
	if err != nil {
		return ev, err
	}
	// The fence gate scans every compared table while writes are held, so its
	// time grows with the data, not with the traffic. Time a rehearsal now,
	// while writes still flow (its mismatches mean nothing yet), and refuse to
	// hold writes for longer than maxFenceGate (ADR 0013).
	if *e.plan.Verify.FenceGate {
		start := time.Now()
		if _, _, err := e.verifier.FenceGate(ctx, e.srcDB, e.dstDB); err != nil {
			return ev, fmt.Errorf("fence gate rehearsal: %w", err)
		}
		took, limit := time.Since(start), e.plan.Thresholds.MaxFenceGate.Duration
		ev["fenceGateRehearsalMillis"] = took.Milliseconds()
		if took > limit {
			return ev, fmt.Errorf("the fence gate took %s before the hold, over thresholds.maxFenceGate %s: writes would be held at least that long; narrow verify.tables, or raise maxFenceGate knowing the gateway answers 503 to a write held 55s", took.Round(time.Millisecond), limit)
		}
	}
	return ev, nil
}

// fence: hold writes at the gateway, record the source's final LSN, and copy
// the last files. Downtime for writes starts here.
func (e *Engine) fence(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{}
	if e.plan.SourceWriteFence != nil && e.writeFencer == nil {
		if err := e.prepareSourceWriteFence(ctx); err != nil {
			return ev, err
		}
	}
	if err := e.gw.HoldWrites(ctx); err != nil {
		return ev, fmt.Errorf("hold writes: %w", err)
	}
	now := time.Now().UTC()
	e.setFlags(func(s *State) { s.WritesHeld, s.WritesHeldAt = true, &now })
	ev["writesHeldAt"] = now
	e.log(StepFence, "writes held at gateway", nil)
	if e.plan.SourceWriteFence != nil {
		proof, err := e.acquireSourceWriteFence(ctx)
		ev["sourceWriteFence"] = proof
		if err != nil {
			return ev, fmt.Errorf("source SQL write fence: %w", err)
		}
	}

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
	// timing splits the write hold into its parts (ADR 0013).
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
	lap("fenceGate")

	// 3. Demote source; CNPG publishes a demotion token.
	if err := e.assertSourceWriteFence(tctx); err != nil {
		return ev, err
	}
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
	lap("demote")
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
		lap("promoteToWritable")
		return ev, fmt.Errorf("target did not become writable: %w", err)
	}
	lap("promoteToWritable")
	e.log(StepSwitchover, "target writable", map[string]any{"afterPromotionMillis": timing["promoteToWritable"]})
	if pos, err := e.dstDB.WALPosition(tctx); err == nil {
		ev["targetLsnAfterPromotion"] = pos
	}
	// 5. Writes stay held until CNPG has finished with the new primary.
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

// startTarget (cold plans): start the target app on the promoted primary,
// wait for its pods to be ready and smoke-test it directly. The gateway still
// holds writes and routes to the source, so users reach nothing here. The
// write hold must stay within maxDowntime; past it the step fails and the
// promotion is undone (ADR 0012).
func (e *Engine) startTarget(ctx context.Context) (map[string]any, error) {
	a := e.plan.Target.App
	ev := map[string]any{"kind": a.Kind, "name": a.Name, "replicas": a.Replicas}
	timeout := a.StartTimeout.Duration
	var held time.Duration
	// An external source's run cannot be undone after promotion (ADR 0019):
	// failing on the budget would only halt with writes held. Start the app.
	if e.state.WritesHeldAt != nil && !e.plan.Source.External() {
		held = time.Since(*e.state.WritesHeldAt)
		if left := e.plan.Thresholds.MaxDowntime.Duration - held; left < timeout {
			timeout = left
		}
	}
	if timeout <= 0 {
		return ev, fmt.Errorf("writes already held %s, over maxDowntime %s, before the app started", held.Round(time.Millisecond), e.plan.Thresholds.MaxDowntime.Duration)
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()

	if err := e.app.Scale(tctx, a.Replicas); err != nil {
		return ev, fmt.Errorf("scale target app: %w", err)
	}
	e.log(StepStartTarget, "target app scaled up", map[string]any{"replicas": a.Replicas})
	var ready int64
	for {
		n, err := e.app.Ready(tctx)
		if err == nil {
			ready = n
			if n >= a.Replicas {
				break
			}
		}
		select {
		case <-tctx.Done():
			ev["ready"] = ready
			return ev, fmt.Errorf("target app has %d of %d ready pods after %s (last error: %v)", ready, a.Replicas, timeout.Round(time.Millisecond), err)
		case <-time.After(e.poll / 2):
		}
	}
	ev["ready"] = ready
	ev["readyMillis"] = time.Since(start).Milliseconds()

	// A pod can be ready a moment before it answers through its Service:
	// repeat the smoke set until every request passes or the time is up.
	for {
		results, failed := e.smoke(tctx, a)
		ev["smoke"] = results
		if failed == 0 {
			break
		}
		select {
		case <-tctx.Done():
			return ev, fmt.Errorf("%d of %d smoke requests to the target app failed", failed, len(results))
		case <-time.After(e.poll):
		}
	}
	ev["startMillis"] = time.Since(start).Milliseconds()
	e.log(StepStartTarget, "target app answers", map[string]any{"startMillis": ev["startMillis"]})
	return ev, nil
}

// SmokeResult is one smoke request's outcome.
type SmokeResult struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status,omitempty"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Millis int64  `json:"millis"`
}

func (e *Engine) smoke(ctx context.Context, a *plan.AppSpec) ([]SmokeResult, int) {
	base := strings.TrimRight(a.SmokeURL, "/")
	results := make([]SmokeResult, 0, len(a.Smoke))
	failed := 0
	for _, q := range a.Smoke {
		start := time.Now()
		code, err := e.prober.Probe(ctx, q.Method, base+q.Path)
		r := SmokeResult{Method: q.Method, Path: q.Path, Status: code, Millis: time.Since(start).Milliseconds()}
		switch {
		case err != nil:
			r.Error = err.Error()
		case q.Expect != 0:
			r.OK = code == q.Expect
		default:
			r.OK = code/100 == 2
		}
		if !r.OK {
			failed++
		}
		results = append(results, r)
	}
	return results, failed
}

// waitAppStopped waits until no target app pod exists, terminating ones
// included (ADR 0011: a stopping pod still holds its connections).
func (e *Engine) waitAppStopped(ctx context.Context, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last int64 = -1
	for {
		n, err := e.app.Running(wctx)
		if err == nil {
			last = n
			if n == 0 {
				return nil
			}
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("target app still has %d pods after %s (last error: %v)", last, timeout, err)
		case <-time.After(e.poll / 2):
		}
	}
}

// cutover: route all traffic to the target and release held writes there.
func (e *Engine) cutover(ctx context.Context) (map[string]any, error) {
	ev := map[string]any{"backend": e.plan.Gateway.TargetBackend}
	if e.plan.SourceWriteFence != nil {
		if err := e.waitRecovery(ctx, e.plan.Source.DSN, &e.srcDB, true); err != nil {
			return ev, fmt.Errorf("source SQL role before write fence cleanup: %w", err)
		}
		if err := e.restoreSourceWriteFence(ctx, false); err != nil {
			return ev, fmt.Errorf("remove source SQL fence only after confirmed demotion: %w", err)
		}
		ev["sourceWriteFenceReleasedOnReplica"] = true
	}
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

// SettleReport is what waitSettled saw (step evidence).
type SettleReport struct {
	Polls int `json:"polls"`
	// Unobserved polls could not read the cluster or the database (API or
	// connection error). They neither confirm nor break stability.
	Unobserved int `json:"unobserved"`
	// Restarts counts server restarts seen through pg_postmaster_start_time.
	Restarts int `json:"restarts"`
	// What held the settle back, in polls: the CNPG Cluster not healthy, the
	// database still in recovery, a setting pending a restart.
	NotHealthy     int `json:"notHealthy"`
	InRecovery     int `json:"inRecovery"`
	PendingRestart int `json:"pendingRestart"`
	// LastBlockedMillis is when, after the wait began, a poll last saw one of
	// the three; the rest of the wait is the settleMin floor and the polls.
	LastBlockedMillis int64 `json:"lastBlockedMillis"`
	// Observation times accumulate all successful and failed samples without
	// rounding each poll. They exclude the poll sleep and are final evidence,
	// not a live timer or the duration of CNPG's restart itself.
	ObservationMillis  int64 `json:"observationMillis"`
	ClusterReadMillis  int64 `json:"clusterReadMillis"`
	ConnectMillis      int64 `json:"connectMillis"`
	DatabaseReadMillis int64 `json:"databaseReadMillis"`
	CloseMillis        int64 `json:"closeMillis"`
}

type settleDurations struct {
	clusterRead, connect, databaseRead, close time.Duration
}

func (d *settleDurations) add(other settleDurations) {
	d.clusterRead += other.clusterRead
	d.connect += other.connect
	d.databaseRead += other.databaseRead
	d.close += other.close
}

func (d settleDurations) report(rep *SettleReport) {
	rep.ClusterReadMillis = d.clusterRead.Milliseconds()
	rep.ConnectMillis = d.connect.Milliseconds()
	rep.DatabaseReadMillis = d.databaseRead.Milliseconds()
	rep.CloseMillis = d.close.Milliseconds()
	rep.ObservationMillis = (d.clusterRead + d.connect + d.databaseRead + d.close).Milliseconds()
}

// settleSample is one read of a promoted cluster.
type settleSample struct {
	healthy, inRecovery, pending bool
	server                       time.Time // pg_postmaster_start_time; zero when not read
	durations                    settleDurations
}

func (s settleSample) settled() bool { return s.healthy && !s.inRecovery && !s.pending }

// waitSettled waits until a promoted cluster is healthy, writable and has no
// setting pending a restart, for at least settleMin and two settled reads of
// the same server process. CNPG restarts a new primary in place when promotion
// changed a setting such as archive_mode; that restart is a smart shutdown,
// which waits for every client session, so the engine drops its own pool and
// checks with short connections.
//
// A failed read (a flaky port-forward or API blip) is not evidence of a
// restart: the postmaster start time of the next successful read tells whether
// the server restarted in between. Before ADR 0013 every failed read reset the
// count, and a link that dropped every few seconds kept a settled primary
// "unsettled" for the whole switchoverTimeout (production run 2026-11-05).
func (e *Engine) waitSettled(ctx context.Context, c Cluster, dsn string, slot *Database) (SettleReport, error) {
	if *slot != nil {
		(*slot).Close()
		*slot = nil
	}
	var rep SettleReport
	var durations settleDurations
	var server time.Time // postmaster start of the last read that had one
	start, stable := time.Now(), 0
	for {
		rep.Polls++
		s, err := e.observeSettled(ctx, c, dsn)
		durations.add(s.durations)
		durations.report(&rep)
		if err == nil && !s.server.IsZero() {
			if !server.IsZero() && !s.server.Equal(server) {
				rep.Restarts++
				stable = 0
			}
			server = s.server
		}
		switch {
		case err != nil:
			rep.Unobserved++
		case !s.settled():
			stable = 0
			if !s.healthy {
				rep.NotHealthy++
			}
			if s.inRecovery {
				rep.InRecovery++
			}
			if s.pending {
				rep.PendingRestart++
			}
			rep.LastBlockedMillis = time.Since(start).Milliseconds()
		default:
			stable++
		}
		if err == nil && s.settled() && stable >= 2 && time.Since(start) >= settleMin {
			return rep, nil
		}
		select {
		case <-ctx.Done():
			return rep, ctx.Err()
		case <-time.After(e.poll / 4):
		}
	}
}

// observeSettled reads one settle sample: the CNPG Cluster's health, then the
// database's recovery, pending-restart and server start time. The database is
// read even when the Cluster is not healthy, so the report says which of them
// held the settle back. err means the Cluster could not be read; a database
// that cannot be reached leaves the sample unsettled with no server time.
func (e *Engine) observeSettled(ctx context.Context, c Cluster, dsn string) (s settleSample, err error) {
	started := time.Now()
	st, err := c.Get(ctx)
	s.durations.clusterRead = time.Since(started)
	if err != nil {
		return s, err
	}
	s.healthy = st.Healthy()
	started = time.Now()
	db, err := e.connect(ctx, dsn)
	s.durations.connect = time.Since(started)
	if err != nil {
		return s, err
	}
	defer func() {
		started := time.Now()
		db.Close()
		s.durations.close = time.Since(started)
	}()
	queryStarted := time.Now()
	defer func() { s.durations.databaseRead = time.Since(queryStarted) }()
	if observer, ok := db.(settleObserver); ok {
		s.inRecovery, s.pending, s.server, err = observer.SettleStatus(ctx)
		return s, err
	}
	if s.inRecovery, err = db.InRecovery(ctx); err != nil {
		return s, err
	}
	if s.pending, err = db.PendingRestart(ctx); err != nil {
		return s, err
	}
	if s.server, err = db.PostmasterStart(ctx); err != nil {
		return s, err
	}
	return s, nil
}

// readRecovery reads pg_is_in_recovery() at dsn, reconnecting after failures
// until ctx ends; the error is the last one seen.
func (e *Engine) readRecovery(ctx context.Context, dsn string, slot *Database) (bool, error) {
	var last error
	for {
		if *slot == nil {
			db, err := e.connect(ctx, dsn)
			if err == nil {
				*slot = db
			} else {
				last = err
			}
		}
		if *slot != nil {
			rec, err := (*slot).InRecovery(ctx)
			if err == nil {
				return rec, nil
			}
			last = err
			(*slot).Close()
			*slot = nil
		}
		select {
		case <-ctx.Done():
			if last == nil {
				last = ctx.Err()
			}
			return false, last
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
