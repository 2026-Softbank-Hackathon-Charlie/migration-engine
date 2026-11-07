// Package gateway is the engine's view of the traffic gateway owned by area C.
//
// The HTTP contract below is a proposal to agree on with the gateway owner:
//
//	GET  /healthz                     -> 200
//	POST /v1/writes/hold              -> 200 once new writes are held AND in-flight writes have drained
//	POST /v1/writes/release           -> 200 once held writes are flushed to the current backend
//	POST /v1/route {"backend":"eks"}  -> 200 once 100% of traffic targets that backend
//
// All requests carry "Authorization: Bearer <token>" when a token is set.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Gateway is what the engine needs from the traffic gateway.
type Gateway interface {
	Health(ctx context.Context) error
	HoldWrites(ctx context.Context) error
	ReleaseWrites(ctx context.Context) error
	Route(ctx context.Context, backend string) error
}

// None is a no-op gateway for rehearsals or when traffic is switched by hand.
type None struct{}

func (None) Health(context.Context) error        { return nil }
func (None) HoldWrites(context.Context) error    { return nil }
func (None) ReleaseWrites(context.Context) error { return nil }
func (None) Route(context.Context, string) error { return nil }

// HTTP talks to the gateway admin API.
type HTTP struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// NewHTTP returns an HTTP gateway client.
func NewHTTP(baseURL, token string) *HTTP {
	return &HTTP{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		Client:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (h *HTTP) Health(ctx context.Context) error { return h.do(ctx, http.MethodGet, "/healthz", nil) }
func (h *HTTP) HoldWrites(ctx context.Context) error {
	return h.do(ctx, http.MethodPost, "/v1/writes/hold", nil)
}
func (h *HTTP) ReleaseWrites(ctx context.Context) error {
	return h.do(ctx, http.MethodPost, "/v1/writes/release", nil)
}
func (h *HTTP) Route(ctx context.Context, backend string) error {
	return h.do(ctx, http.MethodPost, "/v1/route", map[string]string{"backend": backend})
}

// ErrNotMeasured is returned by gateways that cannot report traffic.
var ErrNotMeasured = errors.New("this gateway does not report traffic")

// Totals is the request count a backend has served since the gateway started.
type Totals struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
}

// Totals returns per-backend request totals (GET /v1/state).
func (h *HTTP) Totals(ctx context.Context) (map[string]Totals, error) {
	var st struct {
		Totals map[string]Totals `json:"totals"`
	}
	if err := h.call(ctx, http.MethodGet, "/v1/state", nil, &st); err != nil {
		return nil, err
	}
	if st.Totals == nil {
		return nil, errors.New("gateway /v1/state has no totals")
	}
	return st.Totals, nil
}

// Totals is not available without a gateway.
func (None) Totals(context.Context) (map[string]Totals, error) { return nil, ErrNotMeasured }

// SourceFenceReady proves old reads have drained before source DB sessions are
// terminated. Missing fields from an older gateway are unknown, never zero.
func (h *HTTP) SourceFenceReady(ctx context.Context, source, target, lease string) (bool, error) {
	var state struct {
		WriteBackend *string        `json:"writeBackend"`
		ReadWeights  map[string]int `json:"readWeights"`
		WritesHeld   *bool          `json:"writesHeld"`
		InFlight     *struct {
			Reads *int `json:"reads"`
		} `json:"inFlight"`
		ReadExclusions map[string]string `json:"readExclusions"`
		ReadInFlight   map[string]*int   `json:"readInFlight"`
	}
	if err := h.call(ctx, http.MethodGet, "/v1/state", nil, &state); err != nil {
		return false, err
	}
	if state.WriteBackend == nil || state.WritesHeld == nil || state.InFlight == nil || state.InFlight.Reads == nil || state.ReadWeights == nil {
		return false, errors.New("gateway does not report the read-drain proof required for SQL fencing")
	}
	if state.ReadExclusions[source] != lease || state.ReadInFlight[source] == nil {
		return false, errors.New("gateway has no owned source-read exclusion and count; weights alone are not a drain proof")
	}
	if !*state.WritesHeld || *state.WriteBackend != source {
		return false, errors.New("gateway source write route or hold changed before SQL fencing")
	}
	if state.ReadWeights[target] != 100 || *state.ReadInFlight[source] != 0 {
		return false, nil
	}
	for backend, weight := range state.ReadWeights {
		if backend != target && weight != 0 {
			return false, nil
		}
	}
	return true, nil
}

func (h *HTTP) SourceFenceSupported(ctx context.Context) error {
	var state struct {
		Supported *bool `json:"sourceReadExclusionSupported"`
	}
	if err := h.call(ctx, http.MethodGet, "/v1/state", nil, &state); err != nil {
		return err
	}
	if state.Supported == nil || !*state.Supported {
		return errors.New("gateway does not support source read exclusion and drain epochs; SQL fencing is unavailable")
	}
	return nil
}

func (h *HTTP) AcquireSourceReadFence(ctx context.Context, source, lease string) error {
	return h.do(ctx, http.MethodPost, "/v1/reads/exclude", map[string]string{"backend": source, "lease": lease})
}

func (h *HTTP) ReleaseSourceReadFence(ctx context.Context, source, lease string) error {
	return h.do(ctx, http.MethodPost, "/v1/reads/include", map[string]string{"backend": source, "lease": lease})
}

func (h *HTTP) do(ctx context.Context, method, path string, body any) error {
	return h.call(ctx, method, path, body, nil)
}

func (h *HTTP) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return fmt.Errorf("gateway %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("gateway %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			return fmt.Errorf("gateway %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}
