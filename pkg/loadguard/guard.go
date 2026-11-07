// Package loadguard admits migration work against operator-owned, source-scoped
// Prometheus budgets. It never pauses WAL replication or changes PostgreSQL GUCs.
package loadguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Env = "HOPMIGRATE_LOAD_POLICY"

type Rule struct {
	ID    string `json:"id"`
	Query string `json:"query"`
	// TimestampQuery must return the oldest actual scrape timestamp used by
	// Query, not Prometheus's evaluation timestamp or the time() function.
	TimestampQuery string   `json:"timestampQuery"`
	Min            *float64 `json:"min,omitempty"`
	Max            *float64 `json:"max,omitempty"`
	Critical       bool     `json:"critical,omitempty"`
}

type Policy struct {
	PrometheusURL         string `json:"prometheusURL"`
	TokenEnv              string `json:"tokenEnv,omitempty"`
	PollSeconds           int    `json:"pollSeconds"`
	MaxWaitSeconds        int    `json:"maxWaitSeconds"`
	MaxAgeSeconds         int    `json:"maxAgeSeconds"`
	RequestTimeoutSeconds int    `json:"requestTimeoutSeconds"`
	BatchSize             int    `json:"batchSize"`
	BatchPauseMillis      int    `json:"batchPauseMillis"`
	QueryTimeoutMillis    int    `json:"queryTimeoutMillis"`
	// Both endpoints must be registered as env/namespace/cluster. No query is
	// built from client input; labels and threshold units are operator policy.
	Sources map[string][]Rule `json:"sources"`
}

func FromEnv() (*Policy, error) {
	path := os.Getenv(Env)
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read DB load policy")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("DB load policy exceeds 1 MiB or cannot be read")
	}
	var p Policy
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid DB load policy JSON")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

var ruleID = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,47}$`)

func (p *Policy) Validate() error {
	u, err := url.Parse(p.PrometheusURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid DB load Prometheus URL")
	}
	if p.PollSeconds < 1 || p.PollSeconds > 60 || p.MaxWaitSeconds < 1 || p.MaxWaitSeconds > 600 || p.MaxAgeSeconds < 1 || p.MaxAgeSeconds > 300 || p.RequestTimeoutSeconds < 1 || p.RequestTimeoutSeconds > 15 {
		return errors.New("DB load policy time bounds invalid")
	}
	if p.BatchSize < 1 || p.BatchSize > 1000 || p.BatchPauseMillis < 1 || p.BatchPauseMillis > 1000 || p.QueryTimeoutMillis < 10 || p.QueryTimeoutMillis > 30000 {
		return errors.New("DB verification budget invalid")
	}
	if len(p.Sources) == 0 {
		return errors.New("DB load sources missing")
	}
	for scope, rules := range p.Sources {
		if len(strings.Split(scope, "/")) != 3 || len(rules) < 4 || len(rules) > 12 {
			return errors.New("DB load source scope or rule count invalid")
		}
		seen := map[string]bool{}
		for _, r := range rules {
			if !ruleID.MatchString(r.ID) || seen[r.ID] || r.Query == "" || len(r.Query) > 16384 || r.TimestampQuery == "" || len(r.TimestampQuery) > 16384 || (r.Min == nil && r.Max == nil) {
				return errors.New("DB load rule invalid")
			}
			for _, bound := range []*float64{r.Min, r.Max} {
				if bound != nil && (math.IsNaN(*bound) || math.IsInf(*bound, 0)) {
					return errors.New("DB load bound invalid")
				}
			}
			if r.Min != nil && r.Max != nil && *r.Min > *r.Max {
				return errors.New("DB load min exceeds max")
			}
			if (r.ID == "cpu" && (r.Max == nil || *r.Max <= 0 || *r.Max > 1)) || (r.ID == "freeDiskBytes" && (r.Min == nil || *r.Min <= 0)) || ((r.ID == "ioLatency" || r.ID == "walBytes") && (r.Max == nil || *r.Max <= 0)) {
				return errors.New("DB load metric bound direction or unit invalid")
			}
			if (r.ID == "freeDiskBytes" || r.ID == "walBytes") && !r.Critical {
				return errors.New("disk and WAL rules must be critical")
			}
			seen[r.ID] = true
		}
		for _, id := range []string{"cpu", "ioLatency", "freeDiskBytes", "walBytes"} {
			if !seen[id] {
				return fmt.Errorf("DB load rule %s missing", id)
			}
		}
	}
	return nil
}

type Sample struct {
	Value      float64   `json:"value"`
	Min        *float64  `json:"min,omitempty"`
	Max        *float64  `json:"max,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}
type Snapshot struct {
	Scope   string            `json:"scope"`
	At      time.Time         `json:"at"`
	Status  string            `json:"status"`
	Reason  string            `json:"reason,omitempty"`
	Metrics map[string]Sample `json:"metrics"`
}
type Controller struct {
	Policy Policy
	Scope  string
	Client *http.Client
	Now    func() time.Time
	Emit   func(Snapshot)
	token  string
	mu     sync.Mutex
	last   Snapshot
}

func New(p *Policy, scope string, emit func(Snapshot)) (*Controller, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if len(p.Sources[scope]) == 0 {
		return nil, errors.New("DB load policy does not register this endpoint")
	}
	token := ""
	if p.TokenEnv != "" {
		token = os.Getenv(p.TokenEnv)
		if token == "" {
			return nil, errors.New("DB load Prometheus credential unavailable")
		}
	}
	return &Controller{Policy: *p, Scope: scope, Now: time.Now, Emit: emit, token: token, Client: &http.Client{Timeout: time.Duration(p.RequestTimeoutSeconds) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Controller) Last() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.last
	s.Metrics = cloneMetrics(s.Metrics)
	return s
}
func cloneMetrics(m map[string]Sample) map[string]Sample {
	out := make(map[string]Sample, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
func (c *Controller) record(s Snapshot) {
	c.mu.Lock()
	c.last = s
	c.mu.Unlock()
	if c.Emit != nil {
		c.Emit(s)
	}
}

func (c *Controller) scalar(ctx context.Context, query string) (float64, error) {
	u, err := url.Parse(strings.TrimRight(c.Policy.PrometheusURL, "/") + "/api/v1/query")
	if err != nil {
		return 0, errors.New("metrics URL invalid")
	}
	q := u.Query()
	q.Set("query", query)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, errors.New("metrics request invalid")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return 0, errors.New("metrics unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, errors.New("metrics status not OK")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return 0, errors.New("metrics response exceeds limit")
	}
	var answer struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &answer) != nil || answer.Status != "success" || answer.Data.ResultType != "vector" || len(answer.Data.Result) != 1 || len(answer.Data.Result[0].Value) != 2 {
		return 0, errors.New("metrics missing or ambiguous")
	}
	var value string
	if json.Unmarshal(answer.Data.Result[0].Value[1], &value) != nil {
		return 0, errors.New("metric value invalid")
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, errors.New("metric non-finite")
	}
	return n, nil
}

func (c *Controller) probe(ctx context.Context) (Snapshot, bool, error) {
	ctx, cancelProbe := context.WithTimeout(ctx, time.Duration(c.Policy.RequestTimeoutSeconds)*time.Second)
	defer cancelProbe()
	s := Snapshot{Scope: c.Scope, At: c.Now().UTC(), Status: "allowed", Metrics: map[string]Sample{}}
	critical := false
	for _, rule := range c.Policy.Sources[c.Scope] {
		one, cancel := context.WithTimeout(ctx, time.Duration(c.Policy.RequestTimeoutSeconds)*time.Second)
		value, err := c.scalar(one, rule.Query)
		var stamp float64
		if err == nil {
			stamp, err = c.scalar(one, rule.TimestampQuery)
		}
		cancel()
		if err != nil {
			s.Status, s.Reason = "unknown", rule.ID+": measurement unavailable"
			return s, true, errors.New(s.Reason)
		}
		now := c.Now()
		if value < 0 || stamp < 0 || stamp > float64(now.Add(5*time.Second).Unix()) {
			s.Status, s.Reason = "unknown", rule.ID+": invalid metric or sample timestamp"
			return s, true, errors.New(s.Reason)
		}
		at := time.Unix(int64(stamp), int64((stamp-math.Floor(stamp))*1e9)).UTC()
		if now.Sub(at) > time.Duration(c.Policy.MaxAgeSeconds)*time.Second {
			s.Status, s.Reason = "unknown", rule.ID+": sample stale"
			return s, true, errors.New(s.Reason)
		}
		s.Metrics[rule.ID] = Sample{Value: value, Min: rule.Min, Max: rule.Max, ObservedAt: at}
		if (rule.Min != nil && value < *rule.Min) || (rule.Max != nil && value > *rule.Max) {
			s.Status = "waiting"
			if s.Reason == "" || rule.Critical {
				s.Reason = rule.ID + ": budget exceeded"
			}
			critical = critical || rule.Critical
		}
	}
	return s, critical, nil
}

// Wait blocks only new work. Missing, stale, disk/WAL critical signals fail
// closed immediately. CPU/IO can recover within the bounded admission window.
func (c *Controller) Wait(ctx context.Context) error {
	last := c.Last()
	now := c.Now()
	fresh := len(last.Metrics) == len(c.Policy.Sources[c.Scope])
	for _, sample := range last.Metrics {
		if now.Sub(sample.ObservedAt) > time.Duration(c.Policy.MaxAgeSeconds)*time.Second || sample.ObservedAt.After(now.Add(5*time.Second)) {
			fresh = false
		}
	}
	if fresh && last.Status == "allowed" && !last.At.IsZero() && now.Sub(last.At) >= 0 && now.Sub(last.At) < time.Duration(c.Policy.PollSeconds)*time.Second {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(c.Policy.MaxWaitSeconds)*time.Second)
	defer cancel()
	for {
		s, critical, err := c.probe(waitCtx)
		c.record(s)
		if err != nil {
			return err
		}
		if s.Status == "allowed" {
			return nil
		}
		if critical {
			return errors.New(s.Reason)
		}
		timer := time.NewTimer(time.Duration(c.Policy.PollSeconds) * time.Second)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return errors.New("DB load admission timed out or cancelled")
		case <-timer.C:
		}
	}
}

// CheckNow is a fresh, non-waiting recheck immediately before a critical action.
func (c *Controller) CheckNow(ctx context.Context) error {
	s, _, err := c.probe(ctx)
	c.record(s)
	if err != nil {
		return err
	}
	if s.Status != "allowed" {
		return errors.New(s.Reason)
	}
	return nil
}

// Do monitors a cancellable operation. Cancelling a CNPG wait does NOT stop
// the operator's ongoing base backup or streaming receiver.
func (c *Controller) Do(ctx context.Context, action func(context.Context) error) error {
	if err := c.Wait(ctx); err != nil {
		return err
	}
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	monitor := make(chan error, 1)
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Duration(c.Policy.PollSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-ticker.C:
				s, _, err := c.probe(work)
				if work.Err() != nil {
					return
				}
				c.record(s)
				if err != nil || s.Status != "allowed" {
					monitor <- errors.New("DB load budget exceeded or unavailable during work")
					cancel()
					return
				}
			}
		}
	}()
	err := action(work)
	cancel()
	<-done
	select {
	case guardErr := <-monitor:
		return guardErr
	default:
		return err
	}
}
