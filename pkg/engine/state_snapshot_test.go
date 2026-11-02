package engine

import (
	"context"
	"sync"
	"testing"
)

// State() must return a snapshot: later steps must not change it, and callers
// must not be able to change the engine through it. The platform reads State()
// from another goroutine while steps run.
func TestStateIsASnapshot(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")

	before := e.State()
	if err := e.RunStep(context.Background(), StepPrecheck); err != nil {
		t.Fatal(err)
	}
	if got := before.Steps[StepPrecheck].Status; got != StatusPending {
		t.Fatalf("snapshot taken before precheck changed to %q", got)
	}

	after := e.State()
	rec := after.Steps[StepPrecheck]
	if rec.Status != StatusSucceeded || rec.Evidence == nil {
		t.Fatalf("precheck record = %+v, want succeeded with evidence", rec)
	}
	rec.Status = StatusFailed
	rec.Evidence["injected"] = true
	after.Steps[StepPrepare] = &StepRecord{Status: StatusSucceeded}

	again := e.State()
	if again.Steps[StepPrecheck].Status != StatusSucceeded {
		t.Fatal("changing a snapshot step record changed the engine state")
	}
	if _, ok := again.Steps[StepPrecheck].Evidence["injected"]; ok {
		t.Fatal("changing snapshot evidence changed the engine state")
	}
	if again.Steps[StepPrepare].Status != StatusPending {
		t.Fatal("changing the snapshot step map changed the engine state")
	}
}

// Reading State() while the run is in progress. With -race (needs cgo) this
// catches the shared *StepRecord regression; without it, it still checks that
// concurrent reads do not panic or block the run.
func TestStateReadableDuringRun(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			s := e.State()
			for _, rec := range s.Steps {
				_ = rec.Status
				_ = rec.EndedAt
				for range rec.Evidence {
				}
			}
		}
	}()
	err := e.Run(context.Background())
	close(done)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := e.State().Outcome; got != OutcomeSucceeded {
		t.Fatalf("outcome %q, want succeeded", got)
	}
}
