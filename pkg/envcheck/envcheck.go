// Package envcheck answers "can this cluster be a migration source or target?"
// before a plan exists: the Kubernetes API, the namespace, a default
// StorageClass, CNPG (CRD and operator version), the permissions the engine
// and the platform use, and optionally the database, Prometheus and MinIO.
// It only reads (and asks SelfSubjectAccessReview questions); it changes
// nothing. Built for bringing up a new environment such as an on-prem k3s
// cluster (ADR 0014).
package envcheck

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
)

// Status of one check.
type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn" // works, but differs from what the other environments run
	Fail Status = "fail" // a migration to or from here will not work until fixed
	Skip Status = "skip" // not asked for (no -dsn, -prometheus, -minio)
)

// Check is one finding.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Report is every check in order. OK means no check failed.
type Report struct {
	Context   string  `json:"context,omitempty"`
	Namespace string  `json:"namespace"`
	OK        bool    `json:"ok"`
	Checks    []Check `json:"checks"`
}

// Options says what to compare against. Empty values skip that comparison.
type Options struct {
	Context   string // for the report only
	Namespace string
	// CNPGVersion is the operator version the other environments run, e.g.
	// "1.30.1". A different minor version is a warning.
	CNPGVersion string
	// PGMajor is the PostgreSQL major version of the other side; with DSN the
	// database must match it (physical replication).
	PGMajor int
	// DSN, when set, is connected to (read only): version and recovery state.
	DSN string
	// PrometheusURL and MinIOURL, when set, must answer their health endpoints.
	PrometheusURL string
	MinIOURL      string
}

// Database is what the postgres check reads.
type Database interface {
	ServerVersionNum(ctx context.Context) (int, error)
	InRecovery(ctx context.Context) (bool, error)
	Close()
}

// Deps are the clients the checks use; tests pass fakes.
type Deps struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	Connect func(ctx context.Context, dsn string) (Database, error)
	HTTP    *http.Client
}

// permission is one SelfSubjectAccessReview the engine or the platform needs.
type permission struct {
	verb, group, resource string
	namespaced            bool
	why                   string
	optional              bool // missing is a warning
}

var permissions = []permission{
	{"get", "postgresql.cnpg.io", "clusters", true, "engine reads both CNPG Clusters", false},
	{"create", "postgresql.cnpg.io", "clusters", true, "engine applies the target manifest in prepare", false},
	{"patch", "postgresql.cnpg.io", "clusters", true, "engine demotes and promotes", false},
	{"get", "", "secrets", true, "replication certificates and the app password are copied between environments", false},
	{"create", "", "secrets", true, "replication certificates and the app password are copied between environments", false},
	{"get", "", "services", true, "the deploy executor reads the app's service address", false},
	{"list", "", "pods", true, "the security view and decommission list pods", false},
	{"create", "authorization.k8s.io", "subjectaccessreviews", false, "the parity check asks what the app's service account may do (platform ADR 0022)", true},
}

// Run executes every check. It never returns an error: a check that cannot
// run is a failed check with the reason.
func Run(ctx context.Context, d Deps, o Options) Report {
	r := Report{Context: o.Context, Namespace: o.Namespace}
	add := func(name string, s Status, format string, a ...any) {
		r.Checks = append(r.Checks, Check{Name: name, Status: s, Detail: fmt.Sprintf(format, a...)})
	}
	addf := func(name string, s Status, format string, a []any) { add(name, s, format, a...) }

	// 1. API server. Nothing else works without it.
	v, err := d.Kube.Discovery().ServerVersion()
	if err != nil {
		add("kube-api", Fail, "cannot reach the Kubernetes API: %v", err)
		return finish(r)
	}
	flavor := ""
	if strings.Contains(v.GitVersion, "k3s") {
		flavor = " (k3s)"
	}
	add("kube-api", Pass, "server %s%s", v.GitVersion, flavor)

	// 2. Namespace.
	if _, err := d.Kube.CoreV1().Namespaces().Get(ctx, o.Namespace, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			add("namespace", Fail, "namespace %q does not exist", o.Namespace)
		} else {
			add("namespace", Fail, "read namespace %q: %v", o.Namespace, err)
		}
	} else {
		add("namespace", Pass, "%q exists", o.Namespace)
	}

	// 3. Default StorageClass: CNPG PVCs without storageClass use it.
	if scs, err := d.Kube.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{}); err != nil {
		add("storage-class", Fail, "list StorageClasses: %v", err)
	} else {
		var names, defaults []string
		for _, sc := range scs.Items {
			names = append(names, sc.Name)
			if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
				defaults = append(defaults, sc.Name+" ("+sc.Provisioner+")")
			}
		}
		switch {
		case len(defaults) > 0:
			add("storage-class", Pass, "default %s", strings.Join(defaults, ", "))
		case len(names) > 0:
			add("storage-class", Warn, "no default StorageClass; CNPG manifests must name one of %s", strings.Join(names, ", "))
		default:
			add("storage-class", Fail, "no StorageClass: CNPG cannot create volumes")
		}
	}

	// 4. CNPG CRD (and that we may list Clusters here).
	if list, err := d.Dynamic.Resource(cnpg.ClusterGVR).Namespace(o.Namespace).List(ctx, metav1.ListOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			add("cnpg-crd", Fail, "the postgresql.cnpg.io Cluster resource is missing: install the CNPG operator")
		} else {
			add("cnpg-crd", Fail, "list CNPG Clusters in %q: %v", o.Namespace, err)
		}
	} else {
		var names []string
		for _, it := range list.Items {
			img, _, _ := unstructuredString(it.Object, "spec", "imageName")
			if img == "" {
				img = "operator default image"
			}
			names = append(names, it.GetName()+" ["+img+"]")
		}
		if len(names) == 0 {
			add("cnpg-crd", Pass, "installed; no Cluster in %q yet", o.Namespace)
		} else {
			add("cnpg-crd", Pass, "installed; Clusters in %q: %s", o.Namespace, strings.Join(names, ", "))
		}
	}

	// 5. CNPG operator version.
	addf(operatorCheck(ctx, d.Kube, o.CNPGVersion))

	// 6. Permissions of the credentials in use.
	for _, p := range permissions {
		attrs := &authv1.ResourceAttributes{Verb: p.verb, Group: p.group, Resource: p.resource}
		if p.namespaced {
			attrs.Namespace = o.Namespace
		}
		ssar, err := d.Kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx,
			&authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: attrs}}, metav1.CreateOptions{})
		name := "permission " + p.verb + " " + p.resource
		switch {
		case err != nil:
			add(name, Fail, "cannot ask: %v", err)
		case ssar.Status.Allowed:
			add(name, Pass, "allowed")
		case p.optional:
			add(name, Warn, "denied: %s", p.why)
		default:
			add(name, Fail, "denied: %s", p.why)
		}
	}

	// 7. Database.
	if o.DSN == "" {
		add("postgres", Skip, "no -dsn")
	} else {
		addf(postgresCheck(ctx, d.Connect, o.DSN, o.PGMajor))
	}

	// 8. Prometheus and MinIO health endpoints.
	addf(httpCheck(ctx, d.HTTP, "prometheus", o.PrometheusURL, "/-/ready"))
	addf(httpCheck(ctx, d.HTTP, "minio", o.MinIOURL, "/minio/health/live"))
	return finish(r)
}

func finish(r Report) Report {
	r.OK = true
	for _, c := range r.Checks {
		if c.Status == Fail {
			r.OK = false
		}
	}
	return r
}

func operatorCheck(ctx context.Context, kube kubernetes.Interface, want string) (string, Status, string, []any) {
	const name = "cnpg-operator"
	deps, err := kube.AppsV1().Deployments("").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=cloudnative-pg"})
	if err != nil {
		return name, Warn, "cannot list the operator Deployment (version not compared): %v", []any{err}
	}
	if len(deps.Items) == 0 {
		return name, Warn, "no Deployment labelled app.kubernetes.io/name=cloudnative-pg (version not compared)", nil
	}
	dep := deps.Items[0]
	image := ""
	for _, c := range dep.Spec.Template.Spec.Containers {
		if strings.Contains(c.Image, "cloudnative-pg") {
			image = c.Image
			break
		}
	}
	got := imageVersion(image)
	where := dep.Namespace + "/" + dep.Name
	switch {
	case dep.Status.AvailableReplicas < 1:
		return name, Fail, "%s (%s) has no available replica", []any{where, got}
	case want == "":
		return name, Pass, "%s runs %s", []any{where, got}
	case minor(got) != minor(want):
		return name, Warn, "%s runs %s, the other environments run %s; keep both on one minor version", []any{where, got, want}
	default:
		return name, Pass, "%s runs %s (same as the other environments)", []any{where, got}
	}
}

// imageVersion is the tag of an image without a digest: 1.30.1.
func imageVersion(image string) string {
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndexByte(image, ':'); i > strings.LastIndexByte(image, '/') {
		return image[i+1:]
	}
	return "unknown"
}

// minor is "1.30" of "1.30.1" (or the input when it has no second dot).
func minor(v string) string {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

func postgresCheck(ctx context.Context, connect func(context.Context, string) (Database, error), dsn string, wantMajor int) (string, Status, string, []any) {
	const name = "postgres"
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	db, err := connect(cctx, dsn)
	if err != nil {
		return name, Fail, "cannot connect (from where this runs): %s", []any{redact(err)}
	}
	defer db.Close()
	n, err := db.ServerVersionNum(cctx)
	if err != nil {
		return name, Fail, "read server_version_num: %s", []any{redact(err)}
	}
	rec, err := db.InRecovery(cctx)
	if err != nil {
		return name, Fail, "read pg_is_in_recovery: %s", []any{redact(err)}
	}
	role := "primary"
	if rec {
		role = "replica"
	}
	major := n / 10000
	if wantMajor > 0 && major != wantMajor {
		return name, Fail, "PostgreSQL %d (%s), the other side runs %d: physical replication needs the same major version", []any{major, role, wantMajor}
	}
	return name, Pass, "PostgreSQL %d.%d, %s", []any{major, n % 10000, role}
}

func httpCheck(ctx context.Context, hc *http.Client, name, base, path string) (string, Status, string, []any) {
	if base == "" {
		return name, Skip, "no -%s", []any{name}
	}
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return name, Fail, "bad URL: %v", []any{err}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return name, Fail, "%s%s unreachable: %v", []any{base, path, err}
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return name, Fail, "%s%s answered %s", []any{base, path, resp.Status}
	}
	return name, Pass, "%s%s answered 200", []any{base, path}
}

// redact keeps a DSN's password out of an error message.
func redact(err error) string {
	var s string
	if err != nil {
		s = err.Error()
	}
	if i := strings.Index(s, "://"); i >= 0 {
		if j := strings.IndexByte(s[i+3:], '@'); j >= 0 {
			s = s[:i+3] + "***" + s[i+3+j:]
		}
	}
	return s
}

func unstructuredString(obj map[string]any, fields ...string) (string, bool, error) {
	cur := any(obj)
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false, errors.New("not an object")
		}
		cur, ok = m[f]
		if !ok {
			return "", false, nil
		}
	}
	s, ok := cur.(string)
	return s, ok, nil
}
