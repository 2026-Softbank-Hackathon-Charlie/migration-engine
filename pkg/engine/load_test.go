package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/loadguard"
)

func overloaded(t *testing.T) *loadguard.Controller {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := float64(.1)
		switch r.URL.Query().Get("query") {
		case "time":
			value = float64(time.Now().Unix())
		case "disk":
			value = 0
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{map[string]any{"value": []any{time.Now().Unix(), fmtFloat(value)}}}}})
	}))
	t.Cleanup(server.Close)
	max, min := 1.0, 100.0
	rules := []loadguard.Rule{{ID: "cpu", Query: "cpu", TimestampQuery: "time", Max: &max}, {ID: "ioLatency", Query: "io", TimestampQuery: "time", Max: &max}, {ID: "freeDiskBytes", Query: "disk", TimestampQuery: "time", Min: &min, Critical: true}, {ID: "walBytes", Query: "wal", TimestampQuery: "time", Max: &max, Critical: true}}
	p := &loadguard.Policy{PrometheusURL: server.URL, PollSeconds: 1, MaxWaitSeconds: 1, MaxAgeSeconds: 30, RequestTimeoutSeconds: 1, BatchSize: 32, BatchPauseMillis: 10, QueryTimeoutMillis: 100, Sources: map[string][]loadguard.Rule{"test/app/db": rules}}
	c, err := loadguard.New(p, "test/app/db", nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func fmtFloat(f float64) string { raw, _ := json.Marshal(f); return string(raw) }

func TestLoadBudgetBlocksBeforeWriteHold(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")
	e.sourceLoad = overloaded(t)
	if err := e.Run(t.Context()); err == nil {
		t.Fatal("overloaded DB admitted")
	}
	if contains(h.w.Calls(), "gw.hold") || e.State().PromoteRequested {
		t.Fatalf("overload reached cutover: %v", h.w.Calls())
	}
	if e.State().Outcome != OutcomeFailed {
		t.Fatalf("outcome=%s", e.State().Outcome)
	}
}
func TestPostCutoverBudgetFailureHaltsRatherThanRollback(t *testing.T) {
	h := newHarness(t)
	e := h.engine(t, "")
	for _, step := range Steps {
		if step == StepVerify {
			break
		}
		if err := e.RunStep(t.Context(), step); err != nil {
			t.Fatal(err)
		}
	}
	e.targetLoad = overloaded(t)
	if err := e.RunStep(t.Context(), StepVerify); err == nil {
		t.Fatal("overloaded target verified")
	}
	if e.State().Outcome != OutcomeHalted || !e.State().PromoteRequested {
		t.Fatalf("unsafe outcome=%s", e.State().Outcome)
	}
}
