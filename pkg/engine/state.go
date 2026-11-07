package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Step status values.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Run outcome values.
const (
	OutcomeRunning    = "running"
	OutcomeSucceeded  = "succeeded"
	OutcomeRolledBack = "rolled_back"
	OutcomeHalted     = "halted" // target took over; operator action required, no auto rollback
	OutcomeFailed     = "failed" // failed before anything irreversible; nothing to roll back
)

// StepRecord is the persisted record of one step.
type StepRecord struct {
	Status    string         `json:"status"`
	StartedAt *time.Time     `json:"startedAt,omitempty"`
	EndedAt   *time.Time     `json:"endedAt,omitempty"`
	Error     string         `json:"error,omitempty"`
	Evidence  map[string]any `json:"evidence,omitempty"`
}

// State is the persisted run state. It is saved after every transition so a
// run can be resumed after the engine process restarts.
type State struct {
	RunID     string                 `json:"runId"`
	PlanID    string                 `json:"planId"`
	CreatedAt time.Time              `json:"createdAt"`
	UpdatedAt time.Time              `json:"updatedAt"`
	Outcome   string                 `json:"outcome"`
	Steps     map[string]*StepRecord `json:"steps"`

	// Safety flags that decide what a failure is allowed to undo.
	WritesHeld       bool       `json:"writesHeld"`
	WritesHeldAt     *time.Time `json:"writesHeldAt,omitempty"`
	FenceLSN         string     `json:"fenceLsn,omitempty"`
	SourceDemoted    bool       `json:"sourceDemoted"`
	PromoteRequested bool       `json:"promoteRequested"` // point of no auto-rollback
	TrafficOnTarget  bool       `json:"trafficOnTarget"`
	DowntimeMillis   int64      `json:"downtimeMillis,omitempty"`
	FailureStep      string     `json:"failureStep,omitempty"`
	FailureError     string     `json:"failureError,omitempty"`
	RollbackError    string     `json:"rollbackError,omitempty"`
	SourceFenced     bool       `json:"sourceFenced,omitempty"` // external source asked to refuse writes (ADR 0019)
	FenceXid         string     `json:"fenceXid,omitempty"`     // external source's next xid at the fence
}

func newState(runID, planID string) *State {
	now := time.Now().UTC()
	s := &State{RunID: runID, PlanID: planID, CreatedAt: now, UpdatedAt: now, Outcome: OutcomeRunning, Steps: map[string]*StepRecord{}}
	for _, st := range Steps {
		s.Steps[st] = &StepRecord{Status: StatusPending}
	}
	return s
}

// ReadState loads the saved state of a run from dir.
func ReadState(dir, runID string) (*State, error) {
	return loadState(filepath.Join(dir, runID+".json"))
}

func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode state %s: %w", path, err)
	}
	for _, st := range Steps {
		if s.Steps[st] == nil {
			s.Steps[st] = &StepRecord{Status: StatusPending}
		}
	}
	return &s, nil
}

func (s *State) save(path string) error {
	s.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
