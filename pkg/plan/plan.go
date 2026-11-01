// Package plan defines the migration Plan that the engine executes.
//
// A Plan is produced by the AI planner (area D) or written by hand, validated
// here, approved by a human, and then executed deterministically by the engine.
package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Plan describes one migration of a CNPG-backed service from Source to Target.
type Plan struct {
	ID         string      `json:"id"`
	Source     Endpoint    `json:"source"`
	Target     Endpoint    `json:"target"`
	Files      *FilesSpec  `json:"files,omitempty"`
	Gateway    GatewaySpec `json:"gateway"`
	Verify     VerifySpec  `json:"verify"`
	Thresholds Thresholds  `json:"thresholds"`
}

// Endpoint is one environment (gke / eks / onprem) holding a CNPG Cluster.
type Endpoint struct {
	// Env is the environment label, e.g. "gke", "eks", "onprem".
	Env string `json:"env"`
	// Kubeconfig path; empty uses the default loading rules ($KUBECONFIG, ~/.kube/config).
	Kubeconfig string `json:"kubeconfig,omitempty"`
	// Context in the kubeconfig; empty uses the current context.
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace"`
	// Cluster is the CNPG Cluster resource name.
	Cluster string `json:"cluster"`
	// DSN reaches the cluster's -rw service (over Tailscale). ${ENV} references are expanded.
	DSN string `json:"dsn"`
	// Manifest (target only): CNPG Cluster YAML applied when the target Cluster does not exist yet.
	Manifest string `json:"manifest,omitempty"`
}

// FilesSpec configures rclone-based object storage migration.
// Remotes are configured through rclone env vars (RCLONE_CONFIG_<NAME>_*).
type FilesSpec struct {
	Source string   `json:"source"` // e.g. "src:shop-uploads"
	Target string   `json:"target"` // e.g. "dst:shop-uploads"
	Flags  []string `json:"flags,omitempty"`
	Rclone string   `json:"rclone,omitempty"` // binary path, default "rclone"
}

// GatewaySpec configures the traffic gateway (area C).
type GatewaySpec struct {
	// Mode: "http" talks to the gateway admin API, "none" skips it (rehearsal / manual).
	Mode          string `json:"mode"`
	URL           string `json:"url,omitempty"`
	Token         string `json:"token,omitempty"`
	SourceBackend string `json:"sourceBackend,omitempty"` // default: source.env
	TargetBackend string `json:"targetBackend,omitempty"` // default: target.env
}

// VerifySpec configures data verification.
type VerifySpec struct {
	Schemas []string `json:"schemas,omitempty"` // default ["public"]
	Tables  []string `json:"tables,omitempty"`  // optional filter: "schema.table" or "table"
	// FenceGate compares per-table count+hash while writes are held, before the
	// target is promoted. A mismatch triggers an automatic rollback.
	FenceGate *bool `json:"fenceGate,omitempty"` // default true
	// WrittenRecords is a JSONL file of {"table":..,"pk":..} lines that k6 recorded
	// as successful writes; every one must exist on the target.
	WrittenRecords string `json:"writtenRecords,omitempty"`
}

// Thresholds bound each phase.
type Thresholds struct {
	MaxLagBytes       int64    `json:"maxLagBytes,omitempty"`       // default 16 MiB
	CatchupTimeout    Duration `json:"catchupTimeout,omitempty"`    // default 10m
	SwitchoverTimeout Duration `json:"switchoverTimeout,omitempty"` // default 2m
	MaxDowntime       Duration `json:"maxDowntime,omitempty"`       // default 3m
}

// Duration is a time.Duration that (un)marshals as a Go duration string ("90s").
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"90s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Load reads, expands, defaults and validates a plan file.
func Load(path string) (*Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes, expands, defaults and validates plan JSON.
func Parse(b []byte) (*Plan, error) {
	var p Plan
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	p.expand()
	p.applyDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Plan) expand() {
	p.Source.DSN = os.ExpandEnv(p.Source.DSN)
	p.Target.DSN = os.ExpandEnv(p.Target.DSN)
	p.Gateway.URL = os.ExpandEnv(p.Gateway.URL)
	p.Gateway.Token = os.ExpandEnv(p.Gateway.Token)
}

func (p *Plan) applyDefaults() {
	if p.Gateway.Mode == "" {
		p.Gateway.Mode = "none"
	}
	if p.Gateway.SourceBackend == "" {
		p.Gateway.SourceBackend = p.Source.Env
	}
	if p.Gateway.TargetBackend == "" {
		p.Gateway.TargetBackend = p.Target.Env
	}
	if len(p.Verify.Schemas) == 0 {
		p.Verify.Schemas = []string{"public"}
	}
	if p.Verify.FenceGate == nil {
		t := true
		p.Verify.FenceGate = &t
	}
	if p.Files != nil && p.Files.Rclone == "" {
		p.Files.Rclone = "rclone"
	}
	t := &p.Thresholds
	if t.MaxLagBytes == 0 {
		t.MaxLagBytes = 16 << 20
	}
	if t.CatchupTimeout.Duration == 0 {
		t.CatchupTimeout.Duration = 10 * time.Minute
	}
	if t.SwitchoverTimeout.Duration == 0 {
		t.SwitchoverTimeout.Duration = 2 * time.Minute
	}
	if t.MaxDowntime.Duration == 0 {
		t.MaxDowntime.Duration = 3 * time.Minute
	}
}

// Validate checks that the plan is executable.
func (p *Plan) Validate() error {
	var errs []error
	if p.ID == "" {
		errs = append(errs, errors.New("id is required"))
	}
	for role, e := range map[string]Endpoint{"source": p.Source, "target": p.Target} {
		if e.Env == "" {
			errs = append(errs, fmt.Errorf("%s.env is required", role))
		}
		if e.Namespace == "" {
			errs = append(errs, fmt.Errorf("%s.namespace is required", role))
		}
		if e.Cluster == "" {
			errs = append(errs, fmt.Errorf("%s.cluster is required", role))
		}
		if e.DSN == "" {
			errs = append(errs, fmt.Errorf("%s.dsn is required", role))
		}
	}
	if p.Source.Env != "" && p.Source.Env == p.Target.Env {
		errs = append(errs, fmt.Errorf("source and target env are both %q", p.Source.Env))
	}
	if p.Source.Manifest != "" {
		errs = append(errs, errors.New("source.manifest is not supported (target only)"))
	}
	switch p.Gateway.Mode {
	case "none":
	case "http":
		if p.Gateway.URL == "" {
			errs = append(errs, errors.New("gateway.url is required when gateway.mode is http"))
		}
	default:
		errs = append(errs, fmt.Errorf("gateway.mode must be \"http\" or \"none\", got %q", p.Gateway.Mode))
	}
	if p.Files != nil && (p.Files.Source == "" || p.Files.Target == "") {
		errs = append(errs, errors.New("files.source and files.target are required when files is set"))
	}
	if p.Thresholds.MaxLagBytes < 0 {
		errs = append(errs, errors.New("thresholds.maxLagBytes must be >= 0"))
	}
	return errors.Join(errs...)
}
