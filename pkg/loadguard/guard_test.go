package loadguard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func bound(n float64) *float64 { return &n }
func testPolicy(u string) *Policy {
	return &Policy{PrometheusURL: u, PollSeconds: 1, MaxWaitSeconds: 2, MaxAgeSeconds: 30, RequestTimeoutSeconds: 1, BatchSize: 32, BatchPauseMillis: 10, QueryTimeoutMillis: 100, Sources: map[string][]Rule{"a/app/db": {
		{ID: "cpu", Query: "cpu", TimestampQuery: "time", Max: bound(.7)},
		{ID: "ioLatency", Query: "io", TimestampQuery: "time", Max: bound(.01)},
		{ID: "freeDiskBytes", Query: "disk", TimestampQuery: "time", Min: bound(100), Critical: true},
		{ID: "walBytes", Query: "wal", TimestampQuery: "time", Max: bound(1000), Critical: true},
	}}}
}

type metrics struct {
	mu     sync.Mutex
	values map[string]float64
	mode   string
}

func (m *metrics) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	query := r.URL.Query().Get("query")
	if m.mode == "http" {
		http.Error(w, "SECRET credentials in upstream error", 500)
		return
	}
	result := []any{}
	value := fmt.Sprint(m.values[query])
	if query == "time" {
		value = fmt.Sprint(time.Now().Unix())
		if m.mode == "stale" {
			value = fmt.Sprint(time.Now().Add(-time.Hour).Unix())
		}
	}
	if m.mode == "nan" && query == "cpu" {
		value = "NaN"
	}
	if m.mode != "missing" {
		result = append(result, map[string]any{"metric": map[string]string{}, "value": []any{time.Now().Unix(), value}})
	}
	if m.mode == "multiple" {
		result = append(result, result[0])
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
}
func fixture(t *testing.T) (*Controller, *metrics) {
	t.Helper()
	m := &metrics{values: map[string]float64{"cpu": .2, "io": .001, "disk": 1000, "wal": 10}}
	server := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(server.Close)
	c, err := New(testPolicy(server.URL), "a/app/db", nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, m
}

func TestUnknownMetricsNeverAdmit(t *testing.T) {
	for _, mode := range []string{"missing", "multiple", "stale", "nan", "http"} {
		t.Run(mode, func(t *testing.T) {
			c, m := fixture(t)
			m.mode = mode
			if err := c.Wait(t.Context()); err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe result: %v", err)
			}
			if c.Last().Status != "unknown" {
				t.Fatal("unknown signal became allowed")
			}
		})
	}
}
func TestCriticalWALAndDiskFailWithoutWaiting(t *testing.T) {
	for _, key := range []string{"wal", "disk"} {
		t.Run(key, func(t *testing.T) {
			c, m := fixture(t)
			if key == "wal" {
				m.values[key] = 2000
			} else {
				m.values[key] = 1
			}
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			err := c.Wait(ctx)
			if err == nil || ctx.Err() != nil {
				t.Fatalf("critical condition did not fail immediately: %v", err)
			}
		})
	}
}
func TestSoftBudgetRecoversBeforeWork(t *testing.T) {
	c, m := fixture(t)
	m.values["cpu"] = .9
	go func() { time.Sleep(100 * time.Millisecond); m.mu.Lock(); m.values["cpu"] = .2; m.mu.Unlock() }()
	if err := c.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c.Last().Status != "allowed" {
		t.Fatal("recovery not admitted")
	}
}
func TestMonitorCancelsOnlyMigrationWork(t *testing.T) {
	c, m := fixture(t)
	err := c.Do(t.Context(), func(ctx context.Context) error {
		m.mu.Lock()
		m.values["wal"] = 2000
		m.mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("critical load did not cancel work")
	}
}
func TestNormalCompletionDoesNotBecomeCancellationFailure(t *testing.T) {
	c, _ := fixture(t)
	for i := 0; i < 10; i++ {
		if err := c.Do(t.Context(), func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
}
func TestAdmissionCancellationAndPolicyValidation(t *testing.T) {
	c, m := fixture(t)
	m.values["cpu"] = .9
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Wait(ctx); err == nil {
		t.Fatal("cancelled wait admitted")
	}
	p := testPolicy("http://localhost:9090")
	p.Sources["a/app/db"][0].Max = bound(70)
	if p.Validate() == nil {
		t.Fatal("percentage was accepted as ratio")
	}
	p = testPolicy("http://localhost:9090")
	p.Sources["a/app/db"][3].Critical = false
	if p.Validate() == nil {
		t.Fatal("soft WAL rule accepted")
	}
}
