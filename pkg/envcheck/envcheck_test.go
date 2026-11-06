package envcheck

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
)

type fakeDB struct {
	num int
	rec bool
}

func (d fakeDB) ServerVersionNum(context.Context) (int, error) { return d.num, nil }
func (d fakeDB) InRecovery(context.Context) (bool, error)      { return d.rec, nil }
func (fakeDB) Close()                                          {}

// onprem is a k3s cluster as it should be: namespace, local-path default
// StorageClass, the CNPG operator 1.30.1, and every permission but the ones
// in denied.
func onprem(t *testing.T, denied ...string) Deps {
	t.Helper()
	kube := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "moimroom"}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "local-path",
			Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}, Provisioner: "rancher.io/local-path"},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cnpg-controller-manager", Namespace: "cnpg-system",
			Labels: map[string]string{"app.kubernetes.io/name": "cloudnative-pg"}},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "manager", Image: "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1@sha256:923c"}}}}},
			Status: appsv1.DeploymentStatus{AvailableReplicas: 1}},
	)
	kube.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.33.5+k3s1"}
	kube.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ssar := a.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
		ra := ssar.Spec.ResourceAttributes
		ssar.Status.Allowed = true
		for _, d := range denied {
			if d == ra.Verb+" "+ra.Resource {
				ssar.Status.Allowed = false
			}
		}
		return true, ssar, nil
	})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cnpg.ClusterGVR: "ClusterList"})
	return Deps{Kube: kube, Dynamic: dyn,
		Connect: func(context.Context, string) (Database, error) { return fakeDB{num: 180006}, nil }}
}

func status(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", name, r.Checks)
	return Check{}
}

func TestReadyOnPremCluster(t *testing.T) {
	r := Run(context.Background(), onprem(t), Options{Namespace: "moimroom", CNPGVersion: "1.30.1", PGMajor: 18, DSN: "postgres://x"})
	if !r.OK {
		t.Fatalf("not OK: %+v", r.Checks)
	}
	for name, want := range map[string]string{
		"kube-api":      "v1.33.5+k3s1 (k3s)",
		"storage-class": "local-path (rancher.io/local-path)",
		"cnpg-operator": "runs 1.30.1 (same",
		"postgres":      "PostgreSQL 18.6, primary",
	} {
		if c := status(t, r, name); c.Status != Pass || !strings.Contains(c.Detail, want) {
			t.Errorf("%s: %+v, want pass with %q", name, c, want)
		}
	}
	if c := status(t, r, "minio"); c.Status != Skip {
		t.Errorf("minio: %+v", c)
	}
}

func TestProblemsAreFailuresOrWarnings(t *testing.T) {
	d := onprem(t, "patch clusters", "create secrets", "create subjectaccessreviews")
	d.Dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "clusters", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "clusters"}, "")
	})
	d.Connect = func(context.Context, string) (Database, error) {
		return nil, errors.New(`failed to connect to "postgres://app:TOPSECRET@db.onprem.ts.net/app": timeout`)
	}
	r := Run(context.Background(), d, Options{Namespace: "moimroom", CNPGVersion: "1.29.2", DSN: "postgres://x"})
	if r.OK {
		t.Fatal("a cluster without CNPG and permissions passed")
	}
	for name, want := range map[string]Status{
		"cnpg-crd":                               Fail,
		"cnpg-operator":                          Warn, // 1.30 vs 1.29
		"permission patch clusters":              Fail,
		"permission create secrets":              Fail,
		"permission get clusters":                Pass,
		"permission create subjectaccessreviews": Warn, // only the parity check needs it
		"postgres":                               Fail,
	} {
		if c := status(t, r, name); c.Status != want {
			t.Errorf("%s: %+v, want %s", name, c, want)
		}
	}
	if c := status(t, r, "postgres"); strings.Contains(c.Detail, "TOPSECRET") {
		t.Errorf("password in the report: %s", c.Detail)
	}
}

func TestNoStorageClassAndNoAPI(t *testing.T) {
	d := onprem(t)
	_ = d.Kube.StorageV1().StorageClasses().Delete(context.Background(), "local-path", metav1.DeleteOptions{})
	r := Run(context.Background(), d, Options{Namespace: "moimroom"})
	if c := status(t, r, "storage-class"); c.Status != Fail {
		t.Errorf("storage-class: %+v", c)
	}

	d = onprem(t)
	d.Kube.Discovery().(*fakediscovery.FakeDiscovery).PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp 100.64.0.9:6443: i/o timeout")
	})
	r = Run(context.Background(), d, Options{Namespace: "moimroom"})
	if r.OK || len(r.Checks) != 1 || r.Checks[0].Name != "kube-api" {
		t.Fatalf("an unreachable API must stop the checks: %+v", r)
	}
}

func TestMinorAndImageVersion(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1@sha256:ab": "1.30.1",
		"registry.local:5000/cnpg:1.29.0":                        "1.29.0",
		"cnpg":                                                   "unknown",
	} {
		if got := imageVersion(in); got != want {
			t.Errorf("imageVersion(%q) = %q, want %q", in, got, want)
		}
	}
	if minor("1.30.1") != minor("v1.30.4") || minor("1.30.1") == minor("1.29.9") {
		t.Error("minor")
	}
}
