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

type fixtureStartupWriter struct {
	ctx               context.Context
	writer            io.Writer
	selected, release <-chan struct{}
}

func (v fixtureStartupWriter) Write(p []byte) (int, error) {
	select {
	case <-v.selected:
		select {
		case <-v.release:
		case <-v.ctx.Done():
			return 0, v.ctx.Err()
		}
	default:
	}
	return v.writer.Write(p)
}

type fixtureStartupClient struct {
	selected, release chan struct{}
	result            chan error
}

func startFixtureStartupClient(ctx context.Context, dsn, note string) *fixtureStartupClient {
	c := &fixtureStartupClient{selected: make(chan struct{}), release: make(chan struct{}), result: make(chan error, 1)}
	go func() {
		config, e := pgx.ParseConfig(dsn)
		if e != nil {
			c.result <- e
			return
		}
		trace := &fixtureAdmissionTrace{ready: c.selected}
		config.BuildFrontend = func(r io.Reader, w io.Writer) *pgproto3.Frontend {
			frontend := pgproto3.NewFrontend(r, fixtureStartupWriter{ctx: ctx, writer: w, selected: c.selected, release: c.release})
			frontend.Trace(trace, pgproto3.TracerOptions{SuppressTimestamps: true})
			return frontend
		}
		conn, e := pgx.ConnectConfig(ctx, config)
		if e == nil {
			_, e = conn.Exec(ctx, `INSERT INTO orders(total,note) VALUES (2,$1)`, note)
			_ = conn.Close(context.Background())
		}
		c.result <- e
	}()
	return c
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
	var startupClients []*fixtureStartupClient
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
				startupClients = []*fixtureStartupClient{startFixtureStartupClient(ctx, writerDSN, "fence-late-authentication"), startFixtureStartupClient(ctx, writerDSN, "fence-after-demotion")}
				for _, client := range startupClients {
					select {
					case <-client.selected:
					case <-time.After(2 * time.Second):
						http.Error(w, "client never passed original HBA selection", 503)
						return
					}
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
				t.Log("two SQL clients passed original HBA selection but are absent from the source writer snapshot")
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
	var lateCommitLSN string
	for _, step := range Steps {
		if err = e.RunStep(ctx, step); err != nil {
			t.Fatalf("real CNPG step %s: %v; state=%+v", step, err, e.State())
		}
		if step == StepFence {
			mu.Lock()
			clients := append([]*fixtureStartupClient(nil), startupClients...)
			mu.Unlock()
			for _, client := range clients {
				select {
				case lateErr := <-client.result:
					t.Fatalf("client passed the authentication barrier early: %v", lateErr)
				default:
				}
			}
			if err = tx.Commit(ctx); err == nil {
				t.Fatal("direct SQL transaction committed through source admission/drain")
			}
			// A fresh target connection must survive source-only HBA changes.
			reader, readErr := pgx.Connect(ctx, targetDSN)
			if readErr != nil {
				t.Fatalf("new target reader blocked by source fence: %v", readErr)
			}
			var n, uncommitted int
			if readErr = reader.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE note='fence-open-transaction') FROM orders`).Scan(&n, &uncommitted); readErr != nil || n < 20000 || n > 20001 || uncommitted != 0 {
				t.Fatalf("target read n=%d uncommitted=%d err=%v", n, uncommitted, readErr)
			}
			_ = reader.Close(ctx)
			// Release one real old-HBA connection AFTER the provisional marker.
			// It must actually commit; the final shutdown marker must include it.
			close(clients[0].release)
			select {
			case lateErr := <-clients[0].result:
				if lateErr != nil {
					t.Fatalf("old-HBA race client did not commit: %v", lateErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("old-HBA client did not finish after authentication release")
			}
			lateCommitLSN, err = e.srcDB.WALPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if diff, e := e.srcDB.LSNDiff(ctx, lateCommitLSN, e.State().FenceLSN); e != nil || diff <= 0 {
				t.Fatalf("race commit did not exceed provisional marker: diff=%d err=%v", diff, e)
			}
			t.Log("a real old-HBA client committed beyond the provisional source marker")
		}
		if step == StepSwitchover {
			mu.Lock()
			pending := startupClients[1]
			mu.Unlock()
			close(pending.release)
			select {
			case lateErr := <-pending.result:
				if lateErr == nil {
					t.Fatal("old startup backend survived source demotion and wrote")
				}
				var pgError *pgconn.PgError
				if errors.As(lateErr, &pgError) && pgError.Code != "57P01" && pgError.Code != "57P02" && pgError.Code != "57P03" && pgError.Code != "25006" {
					t.Fatalf("pending startup client failed for unrelated SQL error: %v", lateErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("old startup backend was not conclusively terminated after demotion")
			}
		}
	}
	proof := e.State().SourceWriteFence
	if proof == nil || proof.FinalFenceLSN == "" || !proof.Released {
		t.Fatalf("final checkpoint/read admission proof missing: %+v", proof)
	}
	if diff, err := e.dstDB.LSNDiff(ctx, proof.FinalFenceLSN, lateCommitLSN); err != nil || diff < 0 {
		t.Fatalf("shutdown marker omitted the real late commit: diff=%d err=%v", diff, err)
	}
	target, err := pgx.Connect(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close(context.Background())
	var committed, afterDemotion int
	if err = target.QueryRow(ctx, `SELECT count(*) FILTER (WHERE note='fence-late-authentication'),count(*) FILTER (WHERE note='fence-after-demotion') FROM orders`).Scan(&committed, &afterDemotion); err != nil {
		t.Fatal(err)
	}
	if committed != 1 || afterDemotion != 0 {
		t.Fatalf("startup race accounting failed: committed=%d afterDemotion=%d", committed, afterDemotion)
	}
	t.Logf("real CNPG checkpoint=%s lateCommitLSN=%s targetLateRows=%d oldStartupBackendTerminated=true", proof.FinalFenceLSN, lateCommitLSN, committed)
	t.Logf("real CNPG timingMillis=%v writeHoldMillis=%d", e.State().Steps[StepSwitchover].Evidence["timingMillis"], e.State().DowntimeMillis)
}
