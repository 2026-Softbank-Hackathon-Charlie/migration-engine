//go:build cnpgfence

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// Discard all trace payloads, including auth data. Only the protocol event is
// observed; no trace content is retained or printed.
type fixtureAdmissionTrace struct {
	ready chan struct{}
	once  sync.Once
}

func (v *fixtureAdmissionTrace) Write(p []byte) (int, error) {
	// SCRAM continuation is flushed after HBA selection. AuthenticationOk is
	// buffered until ReadyForQuery, so observing it cannot attest this window.
	if bytes.Contains(p, []byte("AuthenticationSASLContinue")) {
		v.once.Do(func() { close(v.ready) })
	}
	return len(p), nil
}

// This fixture exercises real CNPG controllers in two disposable kind clusters.
// The HTTP server is only a control-plane contract fixture, not a traffic proxy;
// actual gateway retry/drain behavior is tested in the platform companion PR.
func TestSourceWriteFenceCNPGShutdownCheckpoint(t *testing.T) {
	if os.Getenv("HOPCHARLIE_CNPG_FENCE_FIXTURE") != "disposable-kind" {
		t.Skip("disposable CNPG fixture not enabled")
	}
	work := os.Getenv("HOPCHARLIE_CNPG_FENCE_WORK")
	if work == "" {
		t.Fatal("fixture work directory missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	password := os.Getenv("PGPW")
	if password == "" {
		t.Fatal("disposable writer password missing")
	}
	writerDSN := fmt.Sprintf("postgres://app:%s@127.0.0.1:55562/app?sslmode=require", password)
	observerDSN := "postgres://hop_observer:fixture-observer@127.0.0.1:55562/app?sslmode=require"
	targetDSN := "postgres://hop_observer:fixture-observer@127.0.0.1:55563/app?sslmode=require"
	writer, err := pgx.Connect(ctx, writerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close(context.Background())
	tx, err := writer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO orders(total,note) VALUES (1,'fence-open-transaction')`); err != nil {
		t.Fatal(err)
	}
	lateResult := make(chan error, 1)
	authenticated := make(chan struct{})
	var mu sync.Mutex
	held, excluded := false, ""
	backend := "src"
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/healthz":
		case "/v1/state":
			_ = json.NewEncoder(w).Encode(map[string]any{"sourceReadExclusionSupported": true, "writeBackend": backend, "writesHeld": held, "readWeights": map[string]int{"src": 0, "dst": 100}, "inFlight": map[string]int{"reads": 0, "writes": 0}, "readExclusions": map[string]string{"src": excluded}, "readInFlight": map[string]int{"src": 0, "dst": 0}, "totals": map[string]any{"src": map[string]int{"requests": 0, "errors": 0}, "dst": map[string]int{"requests": 0, "errors": 0}}})
		case "/v1/writes/hold":
			held = true
			once.Do(func() {
				go func() {
					// This client's post_auth_delay=10 makes an authenticating backend
					// invisible to the earlier pg_stat_activity session snapshot.
					config, e := pgx.ParseConfig(writerDSN)
					if e != nil {
						lateResult <- e
						return
					}
					config.RuntimeParams["options"] = "-c post_auth_delay=10"
					trace := &fixtureAdmissionTrace{ready: authenticated}
					config.BuildFrontend = func(r io.Reader, w io.Writer) *pgproto3.Frontend {
						frontend := pgproto3.NewFrontend(r, w)
						frontend.Trace(trace, pgproto3.TracerOptions{SuppressTimestamps: true})
						return frontend
					}
					c, e := pgx.ConnectConfig(ctx, config)
					if e == nil {
						_, e = c.Exec(ctx, `INSERT INTO orders(total,note) VALUES (2,'fence-late-authentication')`)
						_ = c.Close(context.Background())
					}
					lateResult <- e
				}()
				select {
				case <-authenticated:
				case <-time.After(2 * time.Second):
					http.Error(w, "late client never passed original HBA selection before fencing", 503)
					return
				}
				observer, e := pgx.Connect(ctx, observerDSN)
				if e != nil {
					http.Error(w, "cannot observe the fixture startup race", 503)
					return
				}
				var visible int
				e = observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename='app' AND backend_type='client backend'`).Scan(&visible)
				_ = observer.Close(ctx)
				if e != nil || visible != 1 {
					http.Error(w, "startup client was not hidden from the source-drain snapshot", 503)
					return
				}
				t.Log("late SQL client passed original HBA selection but is absent from the source writer snapshot")
			})
		case "/v1/reads/exclude", "/v1/reads/include":
			var body struct{ Backend, Lease string }
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Backend != "src" {
				http.Error(w, "bad fixture request", 400)
				return
			}
			if r.URL.Path == "/v1/reads/exclude" {
				if !held || excluded != "" && excluded != body.Lease {
					http.Error(w, "conflict", 409)
					return
				}
				excluded = body.Lease
			} else {
				if excluded != "" && excluded != body.Lease {
					http.Error(w, "conflict", 409)
					return
				}
				excluded = ""
			}
		case "/v1/route":
			var body struct{ Backend string }
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				http.Error(w, "bad fixture route", 400)
				return
			}
			backend = body.Backend
		case "/v1/writes/release":
			held = false
		default:
			http.Error(w, "unexpected fixture path", 404)
		}
	}))
	defer server.Close()
	p := &plan.Plan{ID: "source-write-fence-cnpg", Source: plan.Endpoint{Env: "src", Kubeconfig: filepath.Join(work, "cloud.kubeconfig"), Context: "kind-hopcloud", Namespace: "hop", Cluster: "pg-src", DSN: observerDSN}, Target: plan.Endpoint{Env: "dst", Kubeconfig: filepath.Join(work, "onprem.kubeconfig"), Context: "kind-hoponprem", Namespace: "hop", Cluster: "pg-dst", DSN: targetDSN}, Gateway: plan.GatewaySpec{Mode: "http", URL: server.URL, SourceBackend: "src", TargetBackend: "dst"}, SourceWriteFence: &plan.SourceWriteFenceSpec{Mode: "cnpg_hba", WriterRoles: []string{"app"}, Timeout: plan.Duration{Duration: 30 * time.Second}}}
	// Parse also exercises the public direct-plan serialization/default boundary.
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p, err = plan.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(p, Options{StateDir: filepath.Join(work, "fence-runs"), PollInterval: 200 * time.Millisecond, Emit: func(v Event) {
		if v.Kind != EventMetric {
			t.Logf("%s %s %s", v.Step, v.Kind, v.Message)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, step := range Steps {
		if err = e.RunStep(ctx, step); err != nil {
			t.Fatalf("real CNPG step %s: %v; state=%+v", step, err, e.State())
		}
		if step == StepFence {
			select {
			case lateErr := <-lateResult:
				t.Fatalf("startup client finished before the provisional marker; race was not reproduced: %v", lateErr)
			default:
			}
			if err = tx.Commit(ctx); err == nil {
				t.Fatal("direct SQL transaction committed through source admission/drain")
			}
			// A fresh target connection must survive source-only HBA changes.
			reader, e := pgx.Connect(ctx, targetDSN)
			if e != nil {
				t.Fatalf("new target reader blocked by source fence: %v", e)
			}
			var n, uncommitted int
			if e = reader.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE note='fence-open-transaction') FROM orders`).Scan(&n, &uncommitted); e != nil || n < 20000 || n > 20001 || uncommitted != 0 {
				t.Fatalf("target read n=%d uncommitted=%d err=%v", n, uncommitted, e)
			}
			_ = reader.Close(ctx)
		}
	}
	proof := e.State().SourceWriteFence
	if proof == nil || proof.FinalFenceLSN == "" || !proof.Released {
		t.Fatalf("final checkpoint/read admission proof missing: %+v", proof)
	}
	select {
	case lateErr := <-lateResult:
		var pgError *pgconn.PgError
		if errors.As(lateErr, &pgError) && pgError.Code != "57P01" && pgError.Code != "57P02" && pgError.Code != "57P03" && pgError.Code != "25006" {
			t.Fatalf("late startup fixture failed for an unrelated SQL error: %v", lateErr)
		}
		// Either the shutdown terminated this startup backend, or its commit
		// preceded shutdown and MUST be included in the final target contents.
		target, e := pgx.Connect(ctx, targetDSN)
		if e != nil {
			t.Fatal(e)
		}
		defer target.Close(context.Background())
		var n int
		if e = target.QueryRow(ctx, `SELECT count(*) FROM orders WHERE note='fence-late-authentication'`).Scan(&n); e != nil {
			t.Fatal(e)
		}
		if lateErr == nil && n != 1 {
			t.Fatal("successful late-authentication write missing beyond shutdown-checkpoint fence")
		}
		t.Logf("real CNPG final checkpoint=%s late-client-committed=%v target-late-rows=%d", proof.FinalFenceLSN, lateErr == nil, n)
	case <-ctx.Done():
		t.Fatal("late authentication fixture did not finish")
	}
}
