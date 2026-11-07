// Package plan defines the migration Plan that the engine executes.
//
// A Plan is produced by the AI planner (area D) or written by hand, validated
// here, approved by a human, and then executed deterministically by the engine.
package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
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
	// AllowSourceRewire opts an approved run into selecting the target as the
	// current primary's future upstream, in prepare before writes are held.
	// It never reparents the target or changes either cluster's primary role.
	// Omitted/false preserves the strict prewired two-cluster topology.
	AllowSourceRewire bool `json:"allowSourceRewire,omitempty"`
	// Decommission configures the clean-up of the source after a successful
	// migration (hopmigrate decommission). Optional.
	Decommission *DecommissionSpec `json:"decommission,omitempty"`
}

// Endpoint is one environment (gke / eks / onprem) holding a CNPG Cluster,
// or (source only) a PostgreSQL primary outside CNPG (ADR 0019), or (both
// ends together) no database at all (ADR 0020).
type Endpoint struct {
	// Kind: "cnpg" (default), "external" (source only) or "none" (both
	// source and target: the app has no database, ADR 0020).
	Kind string `json:"kind,omitempty"`
	// Name (external source only) is the source's name in the target
	// Cluster's externalClusters, which the target's replica.primary names.
	Name string `json:"name,omitempty"`
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
	// App (target only) says when the target app starts (ADR 0012). Omitted means warm.
	App *AppSpec `json:"app,omitempty"`
	// Reindex (target only, external source) lists "schema.index" names that
	// are rebuilt on the promoted target before its app starts (ADR 0019).
	Reindex []string `json:"reindex,omitempty"`
}

// Source kinds (ADR 0019).
const (
	SourceCNPG     = "cnpg"
	SourceExternal = "external"
)

// KindNone on both endpoints means the app has no database (ADR 0020).
const KindNone = "none"

// External reports whether the endpoint is a PostgreSQL primary outside CNPG.
func (e Endpoint) External() bool { return e.Kind == SourceExternal }

// Stateless reports whether the plan moves an app without a database: only
// the gateway route and, optionally, files (ADR 0020).
func (p *Plan) Stateless() bool { return p.Source.Kind == KindNone && p.Target.Kind == KindNone }

// App start modes (ADR 0012).
const (
	// AppWarm: the target app already runs on the replica before the switch
	// (the default; canary reads are possible).
	AppWarm = "warm"
	// AppCold: the target app is kept at zero replicas and started only after
	// the target database is promoted, for apps that change the schema on
	// start and cannot run on a read-only replica.
	AppCold = "cold"
)

// AppSpec is the target app workload for the cold mode.
type AppSpec struct {
	Mode string `json:"mode,omitempty"` // "warm" (default) or "cold"
	// Kind is Deployment or StatefulSet in target.namespace.
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
	// Replicas the app is started with after promotion.
	Replicas int64 `json:"replicas,omitempty"`
	// StartTimeout bounds scaling up and waiting for ready pods. Default 3m.
	StartTimeout Duration `json:"startTimeout,omitempty"`
	// SmokeURL reaches the target app directly, not through the gateway
	// (the gateway still points at the source while the app starts).
	SmokeURL string `json:"smokeUrl,omitempty"`
	// Smoke requests must all succeed before traffic moves. Read-only
	// (GET or HEAD): users' writes must not reach the target before cutover.
	Smoke []SmokeRequest `json:"smoke,omitempty"`
}

// Cold reports whether the target app starts after promotion.
func (a *AppSpec) Cold() bool { return a != nil && a.Mode == AppCold }

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
	Schemas []string `json:"schemas,omitempty"` // empty: every user schema (verify.ListTables)
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
	// MaxFenceGate bounds the fence gate, which runs while writes are held and
	// scans every compared table. delta times a rehearsal of it before the
	// hold and fails (writes never held) when it takes longer. Default 20s,
	// well inside the gateway's 55s per-write hold limit (ADR 0013).
	MaxFenceGate Duration `json:"maxFenceGate,omitempty"`
}

// DecommissionSpec says what belongs to the app in the source environment and
// how to prove the new environment no longer depends on it.
type DecommissionSpec struct {
	// Selector is a Kubernetes label selector for the app's resources in
	// source.namespace, e.g. "app.kubernetes.io/part-of=shop". Required.
	Selector string `json:"selector"`
	// Observe is how long source connections are counted after the old app is
	// stopped. Default 2m.
	Observe Duration `json:"observe,omitempty"`
	// SmokeURL is where user traffic enters (the gateway's public address).
	SmokeURL string `json:"smokeUrl"`
	// Smoke requests must all succeed while the source is blocked.
	Smoke []SmokeRequest `json:"smoke"`
}

// SmokeRequest is one request sent to SmokeURL.
type SmokeRequest struct {
	Method string `json:"method"`           // GET, HEAD or POST
	Path   string `json:"path"`             // starts with "/"
	Expect int    `json:"expect,omitempty"` // status code; 0 means any 2xx
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
	if p.Stateless() {
		// No database to compare: the verify options stay as written (Validate
		// rejects them) and the fence gate is off.
		if p.Verify.FenceGate == nil {
			f := false
			p.Verify.FenceGate = &f
		}
	} else if p.Verify.FenceGate == nil {
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
	if t.MaxFenceGate.Duration == 0 {
		t.MaxFenceGate.Duration = 20 * time.Second
	}
	if d := p.Decommission; d != nil && d.Observe.Duration == 0 {
		d.Observe.Duration = 2 * time.Minute
	}
	if a := p.Target.App; a != nil {
		if a.Mode == "" {
			a.Mode = AppWarm
		}
		if a.Mode == AppCold && a.StartTimeout.Duration == 0 {
			a.StartTimeout.Duration = 3 * time.Minute
		}
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
		if e.External() {
			if e.DSN == "" {
				errs = append(errs, fmt.Errorf("%s.dsn is required", role))
			}
			continue
		}
		if e.Kind == KindNone {
			continue // validateStateless
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
	if p.Thresholds.MaxFenceGate.Duration < 0 {
		errs = append(errs, errors.New("thresholds.maxFenceGate must be >= 0"))
	}
	if d := p.Decommission; d != nil {
		errs = append(errs, d.validate()...)
	}
	if p.Source.App != nil {
		errs = append(errs, errors.New("source.app is not supported (target only)"))
	}
	if a := p.Target.App; a != nil {
		errs = append(errs, a.validate(p)...)
	}
	errs = append(errs, p.validateKinds()...)
	return errors.Join(errs...)
}

// validateKinds checks source.kind and what an external source needs (ADR 0019).
func (p *Plan) validateKinds() []error {
	if p.Source.Kind == KindNone || p.Target.Kind == KindNone {
		return p.validateStateless()
	}
	var errs []error
	s := p.Source
	switch s.Kind {
	case "", SourceCNPG:
		if s.Name != "" {
			errs = append(errs, errors.New("source.name is only for source.kind external"))
		}
		if len(p.Target.Reindex) > 0 {
			errs = append(errs, errors.New("target.reindex is only for source.kind external"))
		}
	case SourceExternal:
		if strings.TrimSpace(s.Name) == "" {
			errs = append(errs, errors.New("source.name is required for source.kind external (the target's externalClusters name of the source)"))
		}
		if s.Kubeconfig != "" || s.Context != "" || s.Namespace != "" || s.Cluster != "" {
			errs = append(errs, errors.New("source.kubeconfig, context, namespace and cluster are not used by source.kind external"))
		}
		// Not even an app's start-up schema change can run on the target before
		// it is promoted, and the frozen source cannot take one either.
		if !p.Target.App.Cold() {
			errs = append(errs, errors.New("source.kind external requires target.app.mode cold"))
		}
		if p.Decommission != nil {
			errs = append(errs, errors.New("decommission is not supported for source.kind external"))
		}
		if p.AllowSourceRewire {
			errs = append(errs, errors.New("allowSourceRewire is not supported for source.kind external"))
		}
		// The source no longer follows the target, so the fence gate is the
		// only database comparison (verify skips it).
		if fg := p.Verify.FenceGate; fg != nil && !*fg {
			errs = append(errs, errors.New("source.kind external requires verify.fenceGate"))
		}
		for i, ix := range p.Target.Reindex {
			parts := strings.Split(ix, ".")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.IndexFunc(ix, unicode.IsSpace) >= 0 {
				errs = append(errs, fmt.Errorf("target.reindex[%d] must be \"schema.index\", got %q", i, ix))
			}
		}
	default:
		errs = append(errs, fmt.Errorf("source.kind must be %q, %q or %q, got %q", SourceCNPG, SourceExternal, KindNone, s.Kind))
	}
	if p.Target.Kind != "" && p.Target.Kind != SourceCNPG {
		errs = append(errs, fmt.Errorf("target.kind must be %q or %q, got %q", SourceCNPG, KindNone, p.Target.Kind))
	}
	if p.Target.Name != "" {
		errs = append(errs, errors.New("target.name is not supported (external source only)"))
	}
	if len(p.Source.Reindex) > 0 {
		errs = append(errs, errors.New("source.reindex is not supported (target only)"))
	}
	return errs
}

// validateStateless checks a plan for an app without a database (ADR 0020):
// both kinds are none, no endpoint names a database, and nothing asks for a
// database comparison. Only a cold target app may say where it runs.
func (p *Plan) validateStateless() []error {
	var errs []error
	if !p.Stateless() {
		return []error{fmt.Errorf("source.kind and target.kind must both be %q for an app without a database (got %s and %s)", KindNone, kindName(p.Source.Kind), kindName(p.Target.Kind))}
	}
	cold := p.Target.App.Cold()
	for role, e := range map[string]Endpoint{"source": p.Source, "target": p.Target} {
		var set []string
		for _, f := range []struct{ name, val string }{
			{"name", e.Name}, {"cluster", e.Cluster}, {"dsn", e.DSN}, {"manifest", e.Manifest},
		} {
			if f.val != "" {
				set = append(set, f.name)
			}
		}
		if len(e.Reindex) > 0 {
			set = append(set, "reindex")
		}
		// A cold target app is a workload in target.namespace (ADR 0012).
		if role == "source" || !cold {
			for _, f := range []struct{ name, val string }{
				{"namespace", e.Namespace}, {"kubeconfig", e.Kubeconfig}, {"context", e.Context},
			} {
				if f.val != "" {
					set = append(set, f.name)
				}
			}
		}
		if len(set) > 0 {
			errs = append(errs, fmt.Errorf("%s.%s not used by kind %q (only env; target.namespace, kubeconfig and context only with target.app.mode cold)", role, strings.Join(set, ", "), KindNone))
		}
	}
	if cold && p.Target.Namespace == "" {
		errs = append(errs, errors.New("target.namespace is required with target.app.mode cold (where the target app runs)"))
	}
	if a := p.Target.App; a != nil && !cold {
		if a.Kind != "" || a.Name != "" || a.Replicas != 0 {
			errs = append(errs, errors.New("target.app.kind, name and replicas are only for target.app.mode cold (a warm app is not scaled)"))
		}
		if a.SmokeURL != "" || len(a.Smoke) > 0 {
			errs = append(errs, a.validateSmoke()...)
		}
	}
	if p.Gateway.SourceBackend == p.Gateway.TargetBackend {
		errs = append(errs, fmt.Errorf("gateway.sourceBackend and targetBackend are both %q: the route would not move", p.Gateway.SourceBackend))
	}
	if p.Gateway.Mode != "http" {
		// Moving the gateway route is the whole migration.
		errs = append(errs, fmt.Errorf("kind %q requires gateway.mode http", KindNone))
	}
	v := p.Verify
	if len(v.Schemas) > 0 || len(v.Tables) > 0 || v.WrittenRecords != "" {
		errs = append(errs, fmt.Errorf("verify.schemas, tables and writtenRecords are not used by kind %q (no database)", KindNone))
	}
	if v.FenceGate != nil && *v.FenceGate {
		errs = append(errs, fmt.Errorf("verify.fenceGate must be false or omitted for kind %q (no database to compare)", KindNone))
	}
	if p.AllowSourceRewire {
		errs = append(errs, fmt.Errorf("allowSourceRewire is not supported for kind %q (no database topology)", KindNone))
	}
	if p.Decommission != nil {
		errs = append(errs, fmt.Errorf("decommission is not supported for kind %q yet (it retires a source database)", KindNone))
	}
	return errs
}

// kindName shows an endpoint kind, the empty default as cnpg.
func kindName(k string) string {
	if k == "" {
		return `"" (cnpg)`
	}
	return fmt.Sprintf("%q", k)
}

func (a *AppSpec) validate(p *Plan) []error {
	var errs []error
	switch a.Mode {
	case AppWarm:
		return nil
	case AppCold:
	default:
		return []error{fmt.Errorf("target.app.mode must be %q or %q, got %q", AppWarm, AppCold, a.Mode)}
	}
	if p.Gateway.Mode != "http" {
		// Rolling back a promoted target is safe only while the gateway holds
		// every user write; without it nothing proves the target took none.
		errs = append(errs, errors.New("target.app.mode cold requires gateway.mode http"))
	}
	if a.Kind != "Deployment" && a.Kind != "StatefulSet" {
		errs = append(errs, fmt.Errorf("target.app.kind must be Deployment or StatefulSet, got %q", a.Kind))
	}
	if strings.TrimSpace(a.Name) == "" {
		errs = append(errs, errors.New("target.app.name is required"))
	}
	if a.Replicas < 1 {
		errs = append(errs, errors.New("target.app.replicas must be at least 1"))
	}
	if a.StartTimeout.Duration < 0 || a.StartTimeout.Duration > 30*time.Minute {
		errs = append(errs, errors.New("target.app.startTimeout must be between 0 and 30m"))
	}
	return append(errs, a.validateSmoke()...)
}

// validateSmoke checks target.app.smokeUrl and target.app.smoke.
func (a *AppSpec) validateSmoke() []error {
	var errs []error
	if u, err := url.Parse(a.SmokeURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, errors.New("target.app.smokeUrl must be an http(s) URL of the target app"))
	}
	if len(a.Smoke) == 0 {
		errs = append(errs, errors.New("target.app.smoke needs at least one request: traffic moves only after the started app answers"))
	}
	for i, r := range a.Smoke {
		if r.Method != "GET" && r.Method != "HEAD" {
			errs = append(errs, fmt.Errorf("target.app.smoke[%d].method must be GET or HEAD (no writes before cutover)", i))
		}
		if !strings.HasPrefix(r.Path, "/") {
			errs = append(errs, fmt.Errorf("target.app.smoke[%d].path must start with /", i))
		}
		if r.Expect != 0 && (r.Expect < 100 || r.Expect > 599) {
			errs = append(errs, fmt.Errorf("target.app.smoke[%d].expect is not an HTTP status", i))
		}
	}
	return errs
}

func (d *DecommissionSpec) validate() []error {
	var errs []error
	if strings.TrimSpace(d.Selector) == "" {
		errs = append(errs, errors.New("decommission.selector is required (a label selector for the app's resources)"))
	}
	if d.Observe.Duration < 0 || d.Observe.Duration > time.Hour {
		errs = append(errs, errors.New("decommission.observe must be between 0 and 1h"))
	}
	if u, err := url.Parse(d.SmokeURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, errors.New("decommission.smokeUrl must be an http(s) URL"))
	}
	if len(d.Smoke) == 0 {
		errs = append(errs, errors.New("decommission.smoke needs at least one request: the source is blocked only after the new environment is shown to work without it"))
	}
	for i, r := range d.Smoke {
		switch r.Method {
		case "GET", "HEAD", "POST":
		default:
			errs = append(errs, fmt.Errorf("decommission.smoke[%d].method must be GET, HEAD or POST", i))
		}
		if !strings.HasPrefix(r.Path, "/") {
			errs = append(errs, fmt.Errorf("decommission.smoke[%d].path must start with /", i))
		}
		if r.Expect != 0 && (r.Expect < 100 || r.Expect > 599) {
			errs = append(errs, fmt.Errorf("decommission.smoke[%d].expect is not an HTTP status", i))
		}
	}
	return errs
}
