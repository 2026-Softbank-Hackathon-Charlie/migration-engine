package kube

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func obj(apiVersion, kind, name string, labels map[string]string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": "shop"}}}
	u.SetLabels(labels)
	if spec != nil {
		u.Object["spec"] = spec
	}
	return u
}

func fakeInventory(objs ...runtime.Object) (*Inventory, *dynamicfake.FakeDynamicClient) {
	lists := map[schema.GroupVersionResource]string{networkPolicyGVR: "NetworkPolicyList", podGVR: "PodList"}
	for _, k := range Kinds {
		lists[k.GVR] = k.Kind + "List"
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, objs...)
	return &Inventory{Dyn: dyn, Namespace: "shop", OperatorNamespace: "cnpg-system"}, dyn
}

var app = map[string]string{"app.kubernetes.io/part-of": "shop"}

func TestListBySelector(t *testing.T) {
	inv, _ := fakeInventory(
		obj("apps/v1", "Deployment", "web", app, map[string]any{"replicas": int64(2)}),
		obj("batch/v1", "CronJob", "report", app, nil),
		obj("v1", "ConfigMap", "web-config", app, nil),
		obj("v1", "ConfigMap", "other", map[string]string{"app": "other"}, nil),
	)
	got, err := inv.List(context.Background(), "app.kubernetes.io/part-of=shop")
	if err != nil {
		t.Fatal(err)
	}
	want := map[Resource]bool{{"Deployment", "web"}: true, {"CronJob", "report"}: true, {"ConfigMap", "web-config"}: true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, r := range got {
		if !want[r] {
			t.Errorf("unexpected %v", r)
		}
	}
}

func TestStopAndRestoreDeployment(t *testing.T) {
	ctx := context.Background()
	inv, dyn := fakeInventory(obj("apps/v1", "Deployment", "web", app, map[string]any{"replicas": int64(3)}))
	r := Resource{"Deployment", "web"}
	if err := inv.Stop(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := inv.Stop(ctx, r); err != nil { // twice must keep the original count
		t.Fatal(err)
	}
	d, _ := dyn.Resource(Kinds[0].GVR).Namespace("shop").Get(ctx, "web", metav1.GetOptions{})
	if n, _, _ := unstructured.NestedInt64(d.Object, "spec", "replicas"); n != 0 {
		t.Fatalf("replicas after stop = %d", n)
	}
	if d.GetAnnotations()[AnnReplicas] != "3" {
		t.Fatalf("annotation = %v", d.GetAnnotations())
	}
	if err := inv.Restore(ctx, r); err != nil {
		t.Fatal(err)
	}
	d, _ = dyn.Resource(Kinds[0].GVR).Namespace("shop").Get(ctx, "web", metav1.GetOptions{})
	if n, _, _ := unstructured.NestedInt64(d.Object, "spec", "replicas"); n != 3 {
		t.Fatalf("replicas after restore = %d", n)
	}
	if _, ok := d.GetAnnotations()[AnnReplicas]; ok {
		t.Error("annotation left after restore")
	}
}

func TestStopAndRestoreCronJob(t *testing.T) {
	ctx := context.Background()
	inv, dyn := fakeInventory(obj("batch/v1", "CronJob", "report", app, map[string]any{"suspend": false}))
	r := Resource{"CronJob", "report"}
	if err := inv.Stop(ctx, r); err != nil {
		t.Fatal(err)
	}
	c, _ := dyn.Resource(Kinds[2].GVR).Namespace("shop").Get(ctx, "report", metav1.GetOptions{})
	if s, _, _ := unstructured.NestedBool(c.Object, "spec", "suspend"); !s {
		t.Fatal("not suspended")
	}
	if err := inv.Restore(ctx, r); err != nil {
		t.Fatal(err)
	}
	c, _ = dyn.Resource(Kinds[2].GVR).Namespace("shop").Get(ctx, "report", metav1.GetOptions{})
	if s, _, _ := unstructured.NestedBool(c.Object, "spec", "suspend"); s {
		t.Fatal("still suspended after restore")
	}
}

func TestBlockAndUnblock(t *testing.T) {
	ctx := context.Background()
	inv, dyn := fakeInventory()
	for i := 0; i < 2; i++ { // a second Block replaces the policy
		if err := inv.Block(ctx, "deny", "cnpg.io/cluster=pg-old"); err != nil {
			t.Fatal(err)
		}
	}
	np, err := dyn.Resource(networkPolicyGVR).Namespace("shop").Get(ctx, "deny", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sel, _, _ := unstructured.NestedStringMap(np.Object, "spec", "podSelector", "matchLabels")
	if sel["cnpg.io/cluster"] != "pg-old" {
		t.Errorf("podSelector = %v", sel)
	}
	ingress, _, _ := unstructured.NestedSlice(np.Object, "spec", "ingress")
	if len(ingress) != 1 {
		t.Fatalf("ingress = %v; want only the operator namespace allowed", ingress)
	}
	if err := inv.Unblock(ctx, "deny"); err != nil {
		t.Fatal(err)
	}
	if err := inv.Unblock(ctx, "deny"); err != nil {
		t.Fatalf("unblocking twice: %v", err)
	}
	if _, err := dyn.Resource(networkPolicyGVR).Namespace("shop").Get(ctx, "deny", metav1.GetOptions{}); err == nil {
		t.Error("policy still there")
	}
}

// A pod that got its stop signal still runs (and holds connections) until
// its graceful shutdown ends; status.replicas already leaves it out.
func TestRunningCountsTerminatingPods(t *testing.T) {
	d := obj("apps/v1", "Deployment", "web", app, map[string]any{
		"replicas": int64(0), "selector": map[string]any{"matchLabels": map[string]any{"app": "web"}},
	})
	d.Object["status"] = map[string]any{"replicas": int64(0)}
	pod := func(name, phase string, terminating bool) *unstructured.Unstructured {
		p := obj("v1", "Pod", name, map[string]string{"app": "web"}, nil)
		p.Object["status"] = map[string]any{"phase": phase}
		if terminating {
			now := metav1.Now()
			p.SetDeletionTimestamp(&now)
		}
		return p
	}
	other := obj("v1", "Pod", "other", map[string]string{"app": "other"}, nil)
	inv, _ := fakeInventory(d, pod("web-1", "Running", true), pod("web-2", "Succeeded", false), other)
	if n, err := inv.Running(context.Background(), Resource{"Deployment", "web"}); err != nil || n != 1 {
		t.Fatalf("running = %d, %v; want 1 (the terminating pod)", n, err)
	}
}
