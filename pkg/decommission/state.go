package decommission

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/kube"
)

// State is saved next to the migration run as <runID>.decommission.json.
type State struct {
	RunID     string                        `json:"runId"`
	PlanID    string                        `json:"planId"`
	CreatedAt time.Time                     `json:"createdAt"`
	UpdatedAt time.Time                     `json:"updatedAt"`
	Outcome   string                        `json:"outcome"`
	Steps     map[string]*engine.StepRecord `json:"steps"`

	Inventory []Item          `json:"inventory,omitempty"`
	Stopped   []kube.Resource `json:"stopped,omitempty"` // changed by stop; Undo restores them
	Blocked   []string        `json:"blocked,omitempty"` // NetworkPolicies in place; Undo removes them
	Observe   *ObserveResult  `json:"observe,omitempty"`
	Block     *BlockResult    `json:"block,omitempty"`
	Smoke     []SmokeResult   `json:"smoke,omitempty"`
	Report    *Report         `json:"report,omitempty"`

	FailureStep  string `json:"failureStep,omitempty"`
	FailureError string `json:"failureError,omitempty"`
	UndoError    string `json:"undoError,omitempty"`
}

// Item is an inventoried resource and what decommission does with it.
type Item struct {
	kube.Resource
	Action string `json:"action"` // stop, retain, await_approval
	Reason string `json:"reason"`
}

// ObserveResult is what still reached the source after the old app stopped.
// A nil count means it was not measured, which is not the same as zero.
type ObserveResult struct {
	WindowSeconds         int      `json:"windowSeconds"`
	Samples               int      `json:"samples"`
	MaxSourceSessions     *int     `json:"maxSourceSessions"`
	SourceBackendRequests *int64   `json:"sourceBackendRequests"`
	NotMeasured           []string `json:"notMeasured,omitempty"`
}

// BlockResult says whether the source was really unreachable once blocked.
type BlockResult struct {
	Policies  []string `json:"policies"`
	Verified  bool     `json:"verified"` // the engine's own connection to the old database was refused
	Attempts  int      `json:"attempts"`
	LastError string   `json:"lastError,omitempty"`
}

// SmokeResult is one smoke request made while the source was blocked.
type SmokeResult struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Millis int64  `json:"millis"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

// Report is the completion verdict with its evidence kept apart, so a screen
// can show "0 unprocessed / 2 retained / 0 source sessions in 2m / blocked"
// instead of a bare zero.
type Report struct {
	Passed   bool           `json:"passed"`
	Problems []string       `json:"problems,omitempty"`
	Counts   Counts         `json:"counts"`
	Items    []ItemResult   `json:"items"`
	Observe  *ObserveResult `json:"observe"`
	Block    *BlockResult   `json:"block"`
	Smoke    []SmokeResult  `json:"smoke"`
}

// Counts of resources by final status.
type Counts struct {
	Unprocessed      int `json:"unprocessed"`
	Stopped          int `json:"stopped"`
	Retained         int `json:"retained"`
	AwaitingApproval int `json:"awaitingApproval"`
	Removed          int `json:"removed"`
}

// ItemResult is one resource's final status.
type ItemResult struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

func newState(runID, planID string) *State {
	now := time.Now().UTC()
	s := &State{RunID: runID, PlanID: planID, CreatedAt: now, UpdatedAt: now}
	s.reset()
	return s
}

// reset starts the steps over (after a failure or undo put everything back).
func (s *State) reset() {
	s.Outcome = OutcomeRunning
	s.Steps = map[string]*engine.StepRecord{}
	for _, st := range Steps {
		s.Steps[st] = &engine.StepRecord{Status: engine.StatusPending}
	}
	s.Inventory, s.Observe, s.Block, s.Smoke, s.Report = nil, nil, nil, nil, nil
	s.FailureStep, s.FailureError, s.UndoError = "", "", ""
}

func (s *State) clone() State {
	c := *s
	c.Steps = make(map[string]*engine.StepRecord, len(s.Steps))
	for k, v := range s.Steps {
		r := *v
		r.Evidence = maps.Clone(v.Evidence)
		c.Steps[k] = &r
	}
	c.Inventory = slices.Clone(s.Inventory)
	c.Stopped = slices.Clone(s.Stopped)
	c.Blocked = slices.Clone(s.Blocked)
	c.Smoke = slices.Clone(s.Smoke)
	return c
}

func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	for _, st := range Steps {
		if s.Steps[st] == nil {
			s.Steps[st] = &engine.StepRecord{Status: engine.StatusPending}
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
