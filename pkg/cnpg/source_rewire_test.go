package cnpg

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

func rewiringCluster() *unstructured.Unstructured {
	obj := cluster("db-gke", map[string]any{
		"self": "db-gke", "primary": "db-gke", "source": "db-eks", "promotionToken": "preserve-token",
	}, healthy())
	obj.SetUID("source-uid")
	obj.SetResourceVersion("10")
	_ = unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{"name": "db-eks", "connectionParameters": map[string]any{"password": "never-in-status"}},
		map[string]any{"name": "db-onprem"},
	}, "spec", "externalClusters")
	return obj
}

func TestSourceRewireChangesOnlySource(t *testing.T) {
	ctx := context.Background()
	before := rewiringCluster()
	dyn := newFake(before.DeepCopy())
	c := New(dyn, "shop", "db-gke")
	observed, err := c.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(observed.ExternalClusters, []string{"db-eks", "db-onprem"}) {
		t.Fatalf("external names = %v", observed.ExternalClusters)
	}
	encoded, _ := json.Marshal(observed)
	if strings.Contains(string(encoded), "never-in-status") || strings.Contains(string(encoded), "source-uid") {
		t.Fatal("evidence exposed connection data or CAS identity")
	}
	if err := c.RewireSource(ctx, observed, "db-onprem"); err != nil {
		t.Fatal(err)
	}
	after, err := dyn.Resource(ClusterGVR).Namespace("shop").Get(ctx, "db-gke", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(before.Object, "db-onprem", "spec", "replica", "source")
	if !reflect.DeepEqual(before.Object, after.Object) {
		t.Fatalf("changed fields beyond replica.source: before=%v after=%v", before.Object, after.Object)
	}
	for _, action := range dyn.Actions() {
		if action.GetVerb() == "patch" && action.(k8stesting.PatchAction).GetPatchType() != types.JSONPatchType {
			t.Fatal("rewire must use conditional JSON Patch")
		}
	}
}

func TestSourceRewireCASRejectsConcurrentChanges(t *testing.T) {
	for _, field := range []string{"uid", "resourceVersion", "self", "primary"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			dyn := newFake(rewiringCluster())
			c := New(dyn, "shop", "db-gke")
			observed, _ := c.Get(ctx)
			// The fake API applies JSON Patch tests, so a server-side change
			// between observation and patch is refused at the real adapter edge.
			obj, _ := dyn.Resource(ClusterGVR).Namespace("shop").Get(ctx, "db-gke", metav1.GetOptions{})
			switch field {
			case "uid":
				obj.SetUID("replacement-uid")
			case "resourceVersion":
				obj.SetResourceVersion("11")
			case "self", "primary":
				_ = unstructured.SetNestedField(obj.Object, "changed", "spec", "replica", field)
			}
			if _, err := dyn.Resource(ClusterGVR).Namespace("shop").Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := c.RewireSource(ctx, observed, "db-onprem"); err == nil {
				t.Fatal("accepted stale cluster observation")
			}
			after, _ := c.Get(ctx)
			if after.ReplicaSource != "db-eks" {
				t.Fatal("failed conditional patch changed source")
			}
		})
	}
}

func TestSourceRewireRejectsInvalidObservationAndCancellation(t *testing.T) {
	for _, fault := range []string{"missing identity", "replica", "unhealthy", "unknown external", "self", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			dyn := newFake(rewiringCluster())
			c := New(dyn, "shop", "db-gke")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed, _ := c.Get(ctx)
			target := "db-onprem"
			switch fault {
			case "missing identity":
				observed.ResourceVersion = ""
			case "replica":
				observed.ReplicaPrimary = "db-eks"
			case "unhealthy":
				observed.ReadyInstances = 0
			case "unknown external":
				target = "unknown"
			case "self":
				target = observed.Self
			case "cancelled":
				cancel()
			}
			if err := c.RewireSource(ctx, observed, target); err == nil {
				t.Fatal("accepted unsafe rewire")
			}
			for _, action := range dyn.Actions() {
				if action.GetVerb() == "patch" {
					t.Fatal("invalid rewire reached API patch")
				}
			}
		})
	}
}

func TestSourceRewireSurfacesAPIFailure(t *testing.T) {
	for _, cause := range []error{errors.New("admission denied"), context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			dyn := newFake(rewiringCluster())
			c := New(dyn, "shop", "db-gke")
			observed, _ := c.Get(context.Background())
			dyn.PrependReactor("patch", "clusters", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, cause
			})
			if err := c.RewireSource(context.Background(), observed, "db-onprem"); !errors.Is(err, cause) {
				t.Fatalf("API failure lost: %v", err)
			}
		})
	}
}
