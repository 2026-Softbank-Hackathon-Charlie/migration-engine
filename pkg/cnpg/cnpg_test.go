package cnpg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func cluster(name string, replica map[string]any, status map[string]any) *unstructured.Unstructured {
	spec := map[string]any{"instances": int64(2)}
	if replica != nil {
		spec["replica"] = replica
	}
	obj := map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": name, "namespace": "shop"},
		"spec":       spec,
	}
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

func newFake(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ClusterGVR: "ClusterList"}, objs...)
}

func healthy() map[string]any {
	return map[string]any{"phase": HealthyPhase, "readyInstances": int64(2), "currentPrimary": "db-gke-1"}
}

func TestGetParsesStatus(t *testing.T) {
	dyn := newFake(cluster("db-gke", map[string]any{"self": "db-gke", "primary": "db-gke", "source": "db-eks"}, healthy()))
	s, err := New(dyn, "shop", "db-gke").Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Healthy() || !s.IsPrimary() || s.CurrentPrimary != "db-gke-1" || s.ReplicaSource != "db-eks" {
		t.Fatalf("status = %+v", s)
	}
}

func TestStatusDefaultsAndHealth(t *testing.T) {
	// No replica section: self defaults to the resource name and it is primary.
	dyn := newFake(cluster("plain", nil, map[string]any{"phase": "Setting up primary", "readyInstances": int64(1)}))
	s, err := New(dyn, "shop", "plain").Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Self != "plain" || !s.IsPrimary() {
		t.Fatalf("self=%q primary=%v", s.Self, s.IsPrimary())
	}
	if s.Healthy() {
		t.Fatal("cluster still setting up must not be healthy")
	}
	partial := &Status{Phase: HealthyPhase, Instances: 3, ReadyInstances: 2}
	if partial.Healthy() {
		t.Fatal("2/3 ready must not be healthy")
	}
	replica := &Status{Self: "db-eks", ReplicaPrimary: "db-gke"}
	if replica.IsPrimary() {
		t.Fatal("replica reported as primary")
	}
}

func TestGetNotFound(t *testing.T) {
	_, err := New(newFake(), "shop", "missing").Get(context.Background())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDemotionToken(t *testing.T) {
	st := healthy()
	st["demotionToken"] = "abc"
	dyn := newFake(cluster("db-gke", map[string]any{"primary": "db-eks"}, st))
	s, err := New(dyn, "shop", "db-gke").Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.DemotionToken != "abc" || !s.HasDemotion {
		t.Fatalf("token = %q", s.DemotionToken)
	}
}

func TestSetPrimaryPatchesAndClearsToken(t *testing.T) {
	ctx := context.Background()
	dyn := newFake(cluster("db-eks", map[string]any{"self": "db-eks", "primary": "db-gke", "source": "db-gke"}, healthy()))
	c := New(dyn, "shop", "db-eks")

	if err := c.SetPrimary(ctx, "db-eks", "tok-1"); err != nil {
		t.Fatal(err)
	}
	obj, _ := dyn.Resource(ClusterGVR).Namespace("shop").Get(ctx, "db-eks", metav1.GetOptions{})
	rep, _, _ := unstructured.NestedMap(obj.Object, "spec", "replica")
	if rep["primary"] != "db-eks" || rep["promotionToken"] != "tok-1" {
		t.Fatalf("replica after promote = %v", rep)
	}
	if rep["self"] != "db-eks" || rep["source"] != "db-gke" {
		t.Fatalf("merge patch clobbered other fields: %v", rep)
	}

	if err := c.SetPrimary(ctx, "db-gke", ""); err != nil {
		t.Fatal(err)
	}
	obj, _ = dyn.Resource(ClusterGVR).Namespace("shop").Get(ctx, "db-eks", metav1.GetOptions{})
	rep, _, _ = unstructured.NestedMap(obj.Object, "spec", "replica")
	if _, has := rep["promotionToken"]; has {
		t.Fatalf("empty token must remove promotionToken, got %v", rep)
	}
	if rep["primary"] != "db-gke" {
		t.Fatalf("primary = %v", rep["primary"])
	}
}

func TestSetPrimaryMissingCluster(t *testing.T) {
	err := New(newFake(), "shop", "missing").SetPrimary(context.Background(), "x", "")
	if err == nil || !strings.Contains(err.Error(), "replica.primary=x") {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitFor(t *testing.T) {
	dyn := newFake(cluster("db-gke", map[string]any{"self": "db-gke", "primary": "db-gke"}, healthy()))
	c := New(dyn, "shop", "db-gke")

	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = c.SetPrimary(context.Background(), "db-eks", "")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := c.WaitFor(ctx, 5*time.Millisecond, func(s *Status) bool { return s.ReplicaPrimary == "db-eks" })
	if err != nil || s.ReplicaPrimary != "db-eks" {
		t.Fatalf("s=%+v err=%v", s, err)
	}

	short, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	if _, err := c.WaitFor(short, 5*time.Millisecond, func(*Status) bool { return false }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}

	missing := New(dyn, "shop", "missing")
	short3, cancel3 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel3()
	_, err = missing.WaitFor(short3, 5*time.Millisecond, func(*Status) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "last error") {
		t.Fatalf("missing cluster err = %v", err)
	}
}

func TestApplyManifestValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c := New(newFake(), "shop", "db-eks")
	ctx := context.Background()
	cases := map[string]struct{ path, want string }{
		"missing file": {filepath.Join(dir, "nope.yaml"), "nope.yaml"},
		"wrong kind":   {write("dep.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: db-eks\n"), "want postgresql.cnpg.io Cluster"},
		"wrong name":   {write("other.yaml", "apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata:\n  name: other\n"), `plan expects "db-eks"`},
		"bad yaml":     {write("bad.yaml", "kind: [unclosed\n"), "parse manifest"},
	}
	for name, tc := range cases {
		err := c.ApplyManifest(ctx, tc.path)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestApplyManifestTargetsPlanNamespace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "target.yaml")
	manifest := `apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: db-eks
  namespace: somewhere-else
spec:
  instances: 2
`
	os.WriteFile(p, []byte(manifest), 0o644)

	// The fake client does not implement server-side apply, so capture the request it would send.
	dyn := newFake()
	var got k8stesting.PatchAction
	dyn.PrependReactor("patch", "clusters", func(a k8stesting.Action) (bool, runtime.Object, error) {
		got = a.(k8stesting.PatchAction)
		return true, &unstructured.Unstructured{}, nil
	})
	if err := New(dyn, "shop", "db-eks").ApplyManifest(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("no apply request sent")
	}
	if got.GetPatchType() != types.ApplyPatchType || got.GetNamespace() != "shop" || got.GetName() != "db-eks" {
		t.Fatalf("request = %s %s/%s", got.GetPatchType(), got.GetNamespace(), got.GetName())
	}
	body := string(got.GetPatch())
	if !strings.Contains(body, `"namespace":"shop"`) || strings.Contains(body, "somewhere-else") {
		t.Fatalf("manifest namespace not overridden: %s", body)
	}
}
