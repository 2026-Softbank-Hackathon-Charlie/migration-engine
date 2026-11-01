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

func (h *HTTP) do(ctx context.Context, method, path string, body any) error {
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
	return nil
}
