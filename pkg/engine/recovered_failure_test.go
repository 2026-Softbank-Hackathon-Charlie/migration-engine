package engine

import (
	"context"
	"errors"
	"testing"
)

func TestSuccessfulRetryClearsCurrentFailureAndKeepsFailureEvent(t *testing.T) {
	for _, manual := range []bool{false, true} {
		name := "run"
		if manual {
			name = "manual-steps"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			e := h.engine(t, "")
			ctx := context.Background()
			runThrough(t, e, StepDelta)
			h.w.files.syncErr = errors.New("temporary storage outage")
			if err := e.RunStep(ctx, StepDelta); err == nil {
				t.Fatal("expected a real delta failure before retry")
			}
			if st := e.State(); st.Outcome != OutcomeFailed || st.FailureStep != StepDelta || st.FailureError == "" {
				t.Fatalf("the failed run must retain its current failure: %+v", st)
			}
			h.w.files.syncErr = nil
			if manual {
				for _, step := range Steps {
					if e.State().Steps[step].Status != StatusSucceeded {
						if err := e.RunStep(ctx, step); err != nil {
							t.Fatalf("retry %s: %v", step, err)
						}
					}
				}
			} else if err := e.Run(ctx); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if st := e.State(); st.Outcome != OutcomeSucceeded || st.FailureStep != "" || st.FailureError != "" {
				t.Fatalf("a successful retry must have no current failure: %+v", st)
			}
			if len(h.kinds(EventStepFailed)) != 1 {
				t.Fatal("the original failed-attempt event must remain in the audit history")
			}
		})
	}
}
