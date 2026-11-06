package engine

import (
	"context"
	"testing"
)

func TestInterruptedSafetyStepHaltsWithoutExternalCalls(t *testing.T) {
	for _, step := range []string{StepFence, StepSwitchover, StepStartTarget, StepCutover} {
		t.Run(step, func(t *testing.T) {
			h := newHarness(t)
			e := h.engine(t, "")
			e.state.Steps[step].Status = StatusRunning
			e.save()
			resumed := h.engine(t, e.RunID())
			before := len(h.w.Calls())
			if err := resumed.Run(context.Background()); err == nil {
				t.Fatal("interrupted run resumed")
			}
			if resumed.State().Outcome != OutcomeHalted {
				t.Fatal("not halted")
			}
			if len(h.w.Calls()) != before {
				t.Fatal("external side effects during interrupted resume")
			}
			if resumed.State().FailureStep != step {
				t.Fatal("lost interrupted step")
			}
		})
	}
}
