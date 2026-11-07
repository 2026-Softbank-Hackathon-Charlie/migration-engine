package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
)

// checkPreparedTopology permits only the source's future upstream to differ.
// The target must already follow the selected source directly, never a third
// cascading peer. The original strict check remains the final invariant.
func (e *Engine) checkPreparedTopology(src, dst *cnpg.Status) error {
	if !e.plan.AllowSourceRewire {
		return checkTopology(src, dst)
	}
	if !src.Healthy() || src.Self == "" || src.ReplicaPrimary != src.Self {
		return errors.New("source rewire requires a healthy explicitly designated topology primary")
	}
	if src.UID == "" || src.ResourceVersion == "" || dst.UID == "" {
		return errors.New("source rewire requires observed cluster identities and source resourceVersion")
	}
	if !src.HasExternalCluster(dst.Self) {
		return fmt.Errorf("source externalClusters does not name target %q", dst.Self)
	}
	if !dst.HasExternalCluster(src.Self) {
		return fmt.Errorf("target externalClusters does not name source %q", src.Self)
	}
	// Check all the strict invariants without relaxing the target topology.
	selected := *src
	selected.ReplicaSource = dst.Self
	return checkTopology(&selected, dst)
}

func (e *Engine) checkSourceRewireCapability(src, dst *cnpg.Status) error {
	if src.ReplicaSource != dst.Self {
		if _, ok := e.src.(SourceRewirer); !ok {
			return errors.New("source cluster adapter does not support guarded source rewiring")
		}
	}
	return nil
}

// prepareSourceRewire runs only inside an executed plan's prepare step, after
// checking the target is a standby and before holding any writes. Selecting a
// future upstream leaves the source primary and does not reparent the target.
func (e *Engine) prepareSourceRewire(ctx context.Context, src, dst *cnpg.Status, ev map[string]any) error {
	if err := e.checkSourceRewireCapability(src, dst); err != nil {
		return err
	}
	db, err := e.sourceDB(ctx)
	if err != nil {
		return fmt.Errorf("source db before source rewire: %w", err)
	}
	if rec, err := db.InRecovery(ctx); err != nil {
		return fmt.Errorf("source recovery before source rewire: %w", err)
	} else if rec {
		return errors.New("source database is in recovery; refusing source rewire")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if src.ReplicaSource != dst.Self {
		// No unconditional retry: a conflict or unknown request outcome must
		// stop before fence. A later execution re-observes the current topology.
		if err := e.src.(SourceRewirer).RewireSource(ctx, src, dst.Self); err != nil {
			return fmt.Errorf("prepare source rewire: %w", err)
		}
		ev["sourceRewire"] = map[string]any{"from": src.ReplicaSource, "to": dst.Self, "metadataOnly": true}
	}
	// Refresh both clusters and real DB roles after the selection, including
	// the no-op/retry path. A replaced or mutated target is never accepted.
	s, err := e.src.Get(ctx)
	if err != nil {
		return err
	}
	d, err := e.dst.Get(ctx)
	if err != nil {
		return err
	}
	if s.UID != src.UID || d.UID != dst.UID || s.Self != src.Self || d.Self != dst.Self {
		return errors.New("cluster identity changed during source rewire; re-observe topology before retrying")
	}
	if err := e.checkPreparedTopology(s, d); err != nil {
		return err
	}
	if err := checkTopology(s, d); err != nil {
		return fmt.Errorf("topology after source rewire: %w", err)
	}
	if !d.Healthy() {
		return errors.New("target cluster became unhealthy during source rewire")
	}
	if rec, err := db.InRecovery(ctx); err != nil {
		return err
	} else if rec {
		return errors.New("source database entered recovery during source rewire")
	}
	target, err := e.targetDB(ctx)
	if err != nil {
		return err
	}
	if rec, err := target.InRecovery(ctx); err != nil {
		return err
	} else if !rec {
		return errors.New("target database became writable during source rewire")
	}
	ev["source"], ev["target"] = s, d
	return ctx.Err()
}
