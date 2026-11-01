package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type hit struct {
	method, path, auth, ctype, body string
}

func server(t *testing.T, status int, reply string) (*httptest.Server, func() []hit) {
	t.Helper()
	var mu sync.Mutex
	var hits []hit
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits = append(hits, hit{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b)})
		mu.Unlock()
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(s.Close)
	return s, func() []hit { mu.Lock(); defer mu.Unlock(); return append([]hit(nil), hits...) }
}

func TestHTTPContract(t *testing.T) {
	s, hits := server(t, http.StatusOK, "")
	g := NewHTTP(s.URL+"/", "s3cret") // trailing slash must not double up
	ctx := context.Background()
	for _, f := range []func(context.Context) error{g.Health, g.HoldWrites, g.ReleaseWrites} {
		if err := f(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.Route(ctx, "eks"); err != nil {
		t.Fatal(err)
	}
	got := hits()
	want := []struct{ method, path string }{
		{"GET", "/healthz"}, {"POST", "/v1/writes/hold"}, {"POST", "/v1/writes/release"}, {"POST", "/v1/route"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d requests: %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].method != w.method || got[i].path != w.path {
			t.Errorf("request %d = %s %s, want %s %s", i, got[i].method, got[i].path, w.method, w.path)
		}
		if got[i].auth != "Bearer s3cret" {
			t.Errorf("request %d auth = %q", i, got[i].auth)
		}
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(got[3].body), &body); err != nil || body["backend"] != "eks" {
		t.Errorf("route body = %q", got[3].body)
	}
	if got[3].ctype != "application/json" {
		t.Errorf("route content-type = %q", got[3].ctype)
	}
	if got[1].body != "" || got[1].ctype != "" {
		t.Errorf("hold should have no body, got %q (%q)", got[1].body, got[1].ctype)
	}
}

func TestHTTPNoTokenNoHeader(t *testing.T) {
	s, hits := server(t, http.StatusNoContent, "")
	if err := NewHTTP(s.URL, "").HoldWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := hits()[0].auth; a != "" {
		t.Fatalf("auth header = %q, want none", a)
	}
}

func TestHTTPErrorStatus(t *testing.T) {
	s, _ := server(t, http.StatusConflict, "  already holding  \n")
	err := NewHTTP(s.URL, "").HoldWrites(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	for _, part := range []string{"/v1/writes/hold", "409", "already holding"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error %q missing %q", err, part)
		}
	}
}

func TestHTTPUnreachableAndCancelled(t *testing.T) {
	s, _ := server(t, http.StatusOK, "")
	url := s.URL
	s.Close()
	if err := NewHTTP(url, "").Health(context.Background()); err == nil {
		t.Fatal("expected connection error")
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := NewHTTP(slow.URL, "").HoldWrites(ctx); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("request ignored context cancellation")
	}
}

func TestNoneIsNoop(t *testing.T) {
	var g Gateway = None{}
	ctx := context.Background()
	if g.Health(ctx) != nil || g.HoldWrites(ctx) != nil || g.ReleaseWrites(ctx) != nil || g.Route(ctx, "x") != nil {
		t.Fatal("None must never fail")
	}
}
