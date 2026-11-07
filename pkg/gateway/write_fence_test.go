package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSourceFenceReadDrainIsMeasuredAndTargetOnly(t *testing.T) {
	for _, tc := range []struct {
		body        string
		ready, fail bool
	}{
		{`{"writeBackend":"src","writesHeld":true,"readWeights":{"dst":100,"src":0},"inFlight":{"reads":0}}`, true, false},
		{`{"writeBackend":"src","writesHeld":true,"readWeights":{"dst":100},"inFlight":{"reads":1}}`, false, false},
		{`{"writeBackend":"src","writesHeld":true,"readWeights":{"dst":50,"src":50},"inFlight":{"reads":0}}`, false, false},
		{`{"writeBackend":"src","writesHeld":true,"readWeights":{"dst":100}}`, false, true},
		{`{"writeBackend":"src","writesHeld":false,"readWeights":{"dst":100},"inFlight":{"reads":0}}`, false, true},
		{`{"writeBackend":"dst","writesHeld":true,"readWeights":{"dst":100},"inFlight":{"reads":0}}`, false, true},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
		ready, err := NewHTTP(s.URL, "").SourceFenceReady(context.Background(), "src", "dst")
		s.Close()
		if ready != tc.ready || (err != nil) != tc.fail {
			t.Fatalf("body=%s ready=%v err=%v", tc.body, ready, err)
		}
	}
}
