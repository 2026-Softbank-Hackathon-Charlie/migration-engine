package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSourceFenceReadDrainIsOwnedAndSourceSpecific(t *testing.T) {
	for _, tc := range []struct {
		weights     map[string]int
		inflight    any
		lease       string
		ready, fail bool
	}{
		{map[string]int{"dst": 100, "src": 0}, 0, "owner", true, false},
		{map[string]int{"dst": 100}, 1, "owner", false, false},
		{map[string]int{"dst": 50, "src": 50}, 0, "owner", false, false},
		{map[string]int{"dst": 100}, nil, "owner", false, true},
		{map[string]int{"dst": 100}, 0, "other-owner", false, true},
	} {
		state := map[string]any{"writeBackend": "src", "writesHeld": true, "readWeights": tc.weights, "inFlight": map[string]any{"reads": 7}, "readExclusions": map[string]string{"src": tc.lease}, "readInFlight": map[string]any{"src": tc.inflight, "dst": 7}}
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(state) }))
		ready, err := NewHTTP(s.URL, "").SourceFenceReady(context.Background(), "src", "dst", "owner")
		s.Close()
		if ready != tc.ready || (err != nil) != tc.fail {
			t.Fatalf("state=%v ready=%v err=%v", state, ready, err)
		}
	}
}

func TestSourceFenceRejectsLegacyGatewayEvenAt100PercentTarget(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"writeBackend":"src","writesHeld":true,"readWeights":{"dst":100},"inFlight":{"reads":0}}`))
	}))
	defer s.Close()
	g := NewHTTP(s.URL, "")
	if err := g.SourceFenceSupported(context.Background()); err == nil {
		t.Fatal("old gateway treated as read exclusion capable")
	}
	if ready, err := g.SourceFenceReady(context.Background(), "src", "dst", "owner"); err == nil || ready {
		t.Fatal("weights-only drain accepted")
	}
}
