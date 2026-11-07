package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/pg"
)

// SourceWriteFencer is an optional capability; existing plans remain unchanged.
type SourceWriteFencer interface {
	Precheck(context.Context, string) (cnpg.HBASnapshot, error)
	Apply(context.Context, cnpg.HBASnapshot, string) (int, error)
	AssertHeld(context.Context, cnpg.HBASnapshot, string) error
	ObserverAlive(context.Context) error
	ConfirmReadOnlySource(context.Context, Database, cnpg.HBASnapshot, string) (string, error)
	Restore(context.Context, cnpg.HBASnapshot, string, bool) error
}

type sourceFenceCluster interface {
	Get(context.Context) (*cnpg.Status, error)
	InspectWriteFence(context.Context) (cnpg.HBASnapshot, error)
	DryRunWriteFence(context.Context, cnpg.HBASnapshot, string, []string) error
	SetWriteFence(context.Context, cnpg.HBASnapshot, string, []string) error
	CheckWriteFence(context.Context, cnpg.HBASnapshot, string, []string) error
	ClearWriteFence(context.Context, cnpg.HBASnapshot, string, []string, bool) error
}

type sourceHBAFence struct {
	cluster         sourceFenceCluster
	target          Cluster
	targetDB        *pg.DB
	targetPeer      string
	finalCheckpoint string
	pg              *pg.WriterFence
	roles           []string
	interval        time.Duration
}

func (f *sourceHBAFence) Precheck(ctx context.Context, lease string) (cnpg.HBASnapshot, error) {
	s, err := f.cluster.Get(ctx)
	if err != nil {
		return cnpg.HBASnapshot{}, err
	}
	if s.Instances != 1 {
		return cnpg.HBASnapshot{}, errors.New("source write fence currently requires one source instance; multi-instance reload attestation is not implemented")
	}
	snapshot, err := f.cluster.InspectWriteFence(ctx)
	if err != nil {
		return snapshot, err
	}
	snapshot.SQLPeerSHA256, err = f.pg.DB.TLSPeerSHA256(ctx)
	if err != nil {
		return snapshot, err
	}
	f.targetPeer, err = f.targetDB.TLSPeerSHA256(ctx)
	if err != nil {
		return snapshot, err
	}
	if snapshot.SQLPeerSHA256 == f.targetPeer {
		return snapshot, errors.New("source and target SQL endpoints have indistinguishable TLS identities")
	}
	if err = f.pg.DB.PinTLSPeer(ctx, snapshot.SQLPeerSHA256); err != nil {
		return snapshot, err
	}
	if err = f.targetDB.PinTLSPeer(ctx, f.targetPeer); err != nil {
		return snapshot, err
	}
	if err = f.pg.Precheck(ctx); err != nil {
		return snapshot, err
	}
	if err = f.pg.VerifySystemIdentifier(ctx, snapshot.SystemIdentifier); err != nil {
		return snapshot, err
	}
	if _, err = f.pg.DB.CheckpointREDOLSN(ctx); err != nil {
		return snapshot, err
	}
	// Cleanup must be observable using this same credential-free writer probe.
	// Certificate/LDAP/custom admission that cannot be attested is unsupported,
	// rather than discovered only after writes have been held.
	if err = f.pg.Admitted(ctx); err != nil {
		return snapshot, fmt.Errorf("baseline writer admission cannot be attested: %w", err)
	}
	target, err := f.target.Get(ctx)
	if err != nil {
		return snapshot, fmt.Errorf("source write fence requires an existing warm target: %w", err)
	}
	if !target.Healthy() || target.IsPrimary() || target.SystemIdentifier != snapshot.SystemIdentifier || target.UID == "" {
		return snapshot, errors.New("warm target identity or replica health cannot be proven for source write fence")
	}
	if err = (&pg.WriterFence{DB: f.targetDB}).VerifySystemIdentifier(ctx, snapshot.SystemIdentifier); err != nil {
		return snapshot, fmt.Errorf("target SQL identity: %w", err)
	}
	if err = f.cluster.DryRunWriteFence(ctx, snapshot, lease, f.roles); err != nil {
		return snapshot, fmt.Errorf("source write fence admission capability: %w", err)
	}
	return snapshot, nil
}

func (f *sourceHBAFence) Apply(ctx context.Context, snapshot cnpg.HBASnapshot, lease string) (int, error) {
	if err := f.pg.Pinned(ctx); err != nil {
		return 0, err
	}
	if err := f.cluster.SetWriteFence(ctx, snapshot, lease, f.roles); err != nil {
		return 0, err
	}
	for {
		if err := f.cluster.CheckWriteFence(ctx, snapshot, lease, f.roles); err != nil {
			return 0, err
		}
		if err := f.pg.Rejected(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("source HBA did not prove new writer connections rejected: %w", ctx.Err())
		case <-time.After(f.interval):
		}
	}
	return f.pg.Drain(ctx)
}

func (f *sourceHBAFence) AssertHeld(ctx context.Context, snapshot cnpg.HBASnapshot, lease string) error {
	if err := f.cluster.CheckWriteFence(ctx, snapshot, lease, f.roles); err != nil {
		return err
	}
	return f.pg.AssertDrained(ctx)
}

func (f *sourceHBAFence) ObserverAlive(ctx context.Context) error { return f.pg.Pinned(ctx) }

func (f *sourceHBAFence) ConfirmReadOnlySource(ctx context.Context, db Database, snapshot cnpg.HBASnapshot, token string) (string, error) {
	p, ok := db.(*pg.DB)
	if !ok {
		return "", errors.New("cannot identify the demoted source observer")
	}
	s, err := f.cluster.Get(ctx)
	if err != nil {
		return "", err
	}
	if s.UID != snapshot.UID || s.SystemIdentifier != snapshot.SystemIdentifier || s.IsPrimary() || s.Instances != 1 || token == "" || s.DemotionToken != token {
		return "", errors.New("source identity or topology is not the observed demoted cluster")
	}
	f.pg.DB = p
	if err = p.PinTLSPeer(ctx, snapshot.SQLPeerSHA256); err != nil {
		return "", err
	}
	if err = f.pg.VerifySystemIdentifier(ctx, snapshot.SystemIdentifier); err != nil {
		return "", err
	}
	recovery, err := p.InRecovery(ctx)
	if err != nil || !recovery {
		return "", errors.New("source SQL recovery is not confirmed after demotion")
	}
	// The source replay position can be NULL or lag behind its final shutdown
	// checkpoint. It is an observation, never the final commit barrier itself.
	checkpoint, err := cnpg.DemotionCheckpoint(token, snapshot.SystemIdentifier)
	if err != nil {
		return "", err
	}
	redo, err := p.CheckpointREDOLSN(ctx)
	if err != nil {
		return "", err
	}
	diff, err := p.LSNDiff(ctx, redo, checkpoint)
	if err != nil || diff < 0 || f.finalCheckpoint == "" && diff != 0 || f.finalCheckpoint != "" && f.finalCheckpoint != checkpoint {
		return "", errors.New("demoted source control checkpoint does not attest the owned shutdown token")
	}
	f.finalCheckpoint = checkpoint
	return checkpoint, nil
}

func (f *sourceHBAFence) Restore(ctx context.Context, snapshot cnpg.HBASnapshot, lease string, primary bool) error {
	recovery, err := f.pg.DB.InRecovery(ctx)
	if err != nil || recovery == primary {
		return errors.New("source SQL role is unknown or disagrees with write fence cleanup")
	}
	if err = f.cluster.ClearWriteFence(ctx, snapshot, lease, f.roles, primary); err != nil {
		return err
	}
	for {
		if err = f.pg.Admitted(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("writer admission was not restored after source fence cleanup")
		case <-time.After(f.interval):
		}
	}
}

// SourceWriteFenceState is persisted before the first external patch. The HBA
// source text and any authentication options never enter run state or events.
type SourceWriteFenceState struct {
	Lease         string           `json:"lease"`
	Source        cnpg.HBASnapshot `json:"source"`
	WriterRoles   []string         `json:"writerRoles"`
	Intent        bool             `json:"intent"`
	Active        bool             `json:"active"`
	Released      bool             `json:"released"`
	FinalFenceLSN string           `json:"finalFenceLsn,omitempty"`
}

func (e *Engine) prepareSourceWriteFence(ctx context.Context) error {
	if e.plan.SourceWriteFence == nil {
		return nil
	}
	if e.state.SourceWriteFence != nil && e.state.SourceWriteFence.Intent {
		return errors.New("source write fence already has an intent; operator reconciliation required")
	}
	if e.writeFencer == nil {
		cluster, ok := e.src.(sourceFenceCluster)
		if !ok {
			return errors.New("source cluster has no source-local write fence capability")
		}
		db, err := e.sourceDB(ctx)
		if err != nil {
			return err
		}
		p, ok := db.(*pg.DB)
		if !ok {
			return errors.New("source write fence requires a PostgreSQL observer")
		}
		target, err := e.targetDB(ctx)
		if err != nil {
			return err
		}
		tp, ok := target.(*pg.DB)
		if !ok {
			return errors.New("target SQL identity cannot be verified")
		}
		e.writeFencer = &sourceHBAFence{cluster: cluster, target: e.dst, targetDB: tp, pg: &pg.WriterFence{DB: p, DSN: e.plan.Source.DSN, Roles: e.plan.SourceWriteFence.WriterRoles}, roles: e.plan.SourceWriteFence.WriterRoles, interval: 50 * time.Millisecond}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	lease := hex.EncodeToString(nonce[:])
	snapshot, err := e.writeFencer.Precheck(ctx, lease)
	if err != nil {
		return err
	}
	if f, ok := e.writeFencer.(*sourceHBAFence); ok {
		connect := e.connect
		sourceDSN, targetDSN := e.plan.Source.DSN, e.plan.Target.DSN
		e.connect = func(ctx context.Context, dsn string) (Database, error) {
			expected := ""
			if dsn == sourceDSN {
				expected = snapshot.SQLPeerSHA256
			} else if dsn == targetDSN {
				expected = f.targetPeer
			}
			if expected != "" {
				return pg.ConnectPinned(ctx, dsn, expected)
			}
			return connect(ctx, dsn)
		}
	}
	proof := &SourceWriteFenceState{Lease: lease, Source: snapshot, WriterRoles: slices.Clone(e.plan.SourceWriteFence.WriterRoles)}
	return e.persistWriteFence(proof)
}

func (e *Engine) persistWriteFence(proof *SourceWriteFenceState) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state.SourceWriteFence = proof
	if proof.FinalFenceLSN != "" {
		e.state.FenceLSN = proof.FinalFenceLSN
	}
	if err := e.state.save(e.statePath); err != nil {
		return fmt.Errorf("save source write fence intent: %w", err)
	}
	return nil
}

type sourceFenceGateway interface {
	SourceFenceSupported(context.Context) error
	AcquireSourceReadFence(context.Context, string, string) error
	SourceFenceReady(context.Context, string, string, string) (bool, error)
	ReleaseSourceReadFence(context.Context, string, string) error
}

func (e *Engine) acquireSourceWriteFence(ctx context.Context) (map[string]any, error) {
	proof := *e.state.SourceWriteFence
	gw, ok := e.gw.(sourceFenceGateway)
	if !ok {
		return nil, errors.New("gateway cannot prove target-only reads and drained reads before SQL fencing")
	}
	tctx, cancel := context.WithTimeout(ctx, e.plan.SourceWriteFence.Timeout.Duration)
	defer cancel()
	proof.Intent = true
	if err := e.persistWriteFence(&proof); err != nil {
		return nil, err
	}
	if err := gw.AcquireSourceReadFence(tctx, e.plan.Gateway.SourceBackend, proof.Lease); err != nil {
		return nil, err
	}
	for {
		ready, err := gw.SourceFenceReady(tctx, e.plan.Gateway.SourceBackend, e.plan.Gateway.TargetBackend, proof.Lease)
		if err != nil {
			return nil, err
		}
		if ready {
			break
		}
		select {
		case <-tctx.Done():
			return nil, errors.New("source write fence needs all reads on the warm target and no in-flight reads")
		case <-time.After(e.poll):
		}
	}
	n, err := e.writeFencer.Apply(tctx, proof.Source, proof.Lease)
	if err != nil {
		return nil, err
	}
	proof.Active = true
	if err = e.persistWriteFence(&proof); err != nil {
		return nil, err
	}
	return map[string]any{"writerRoles": slices.Clone(proof.WriterRoles), "terminatedClientSessions": n, "newConnectionsRejected": true}, nil
}

func (e *Engine) assertSourceWriteFence(ctx context.Context) error {
	if e.plan.SourceWriteFence == nil {
		return nil
	}
	p := e.state.SourceWriteFence
	if p == nil || !p.Active || p.Released || e.writeFencer == nil {
		return errors.New("source SQL write fence was not proven active")
	}
	return e.writeFencer.AssertHeld(ctx, p.Source, p.Lease)
}

func (e *Engine) restoreSourceWriteFence(ctx context.Context, primary bool) error {
	p := e.state.SourceWriteFence
	if p == nil || !p.Intent || p.Released {
		return nil
	}
	if e.writeFencer == nil {
		return errors.New("source SQL write fence requires operator reconciliation after process restart")
	}
	if err := e.writeFencer.Restore(ctx, p.Source, p.Lease, primary); err != nil {
		return err
	}
	gw, ok := e.gw.(sourceFenceGateway)
	if !ok {
		return errors.New("read exclusion owner cannot be safely released")
	}
	if err := gw.ReleaseSourceReadFence(ctx, e.plan.Gateway.SourceBackend, p.Lease); err != nil {
		return err
	}
	copy := *p
	copy.Active = false
	copy.Released = true
	return e.persistWriteFence(&copy)
}
