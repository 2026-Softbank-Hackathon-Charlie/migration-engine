package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// Resource is one namespaced Kubernetes object.
type Resource struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

func (r Resource) String() string { return r.Kind + "/" + r.Name }

// Kinds an app usually owns, in listing order.
var Kinds = []struct {
	Kind string
	GVR  schema.GroupVersionResource
}{
	{"Deployment", schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}},
	{"StatefulSet", schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}},
	{"CronJob", schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}},
	{"Service", schema.GroupVersionResource{Version: "v1", Resource: "services"}},
	{"Ingress", schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}},
	{"ConfigMap", schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}},
	{"Secret", schema.GroupVersionResource{Version: "v1", Resource: "secrets"}},
	{"PersistentVolumeClaim", schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}},
	{"Cluster", cnpg.ClusterGVR},
}

var (
	networkPolicyGVR = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}
	podGVR           = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
)

func gvrOf(kind string) (schema.GroupVersionResource, error) {
	for _, k := range Kinds {
		if k.Kind == kind {
			return k.GVR, nil
		}
	}
	return schema.GroupVersionResource{}, fmt.Errorf("unsupported kind %q", kind)
}

// Annotations that remember what Stop changed, so Restore can undo it.
const (
	AnnReplicas  = "hopcharlie.io/replicas-before-decommission"
	AnnSuspended = "hopcharlie.io/suspend-before-decommission"
	LabelBlock   = "hopcharlie.io/decommission-block"
)

// Inventory reads and changes an app's resources in one namespace.
type Inventory struct {
	Dyn       dynamic.Interface
	Namespace string
	// OperatorNamespace may still reach blocked database pods (the CNPG operator).
	OperatorNamespace string
}

// List returns the resources of every kind in Kinds that match selector.
// A kind whose API is not installed (e.g. no CNPG) is skipped.
func (i *Inventory) List(ctx context.Context, selector string) ([]Resource, error) {
	var out []Resource
	for _, k := range Kinds {
		l, err := i.Dyn.Resource(k.GVR).Namespace(i.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", k.Kind, err)
		}
		for _, it := range l.Items {
			out = append(out, Resource{Kind: k.Kind, Name: it.GetName()})
		}
	}
	return out, nil
}

func (i *Inventory) get(ctx context.Context, r Resource) (*unstructured.Unstructured, error) {
	gvr, err := gvrOf(r.Kind)
	if err != nil {
		return nil, err
	}
	return i.Dyn.Resource(gvr).Namespace(i.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
}

// Exists reports whether the resource is still there.
func (i *Inventory) Exists(ctx context.Context, r Resource) (bool, error) {
	_, err := i.get(ctx, r)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (i *Inventory) patch(ctx context.Context, r Resource, p map[string]any) error {
	gvr, err := gvrOf(r.Kind)
	if err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = i.Dyn.Resource(gvr).Namespace(i.Namespace).Patch(ctx, r.Name, types.MergePatchType, b, metav1.PatchOptions{})
	return err
}

// Stop scales a Deployment or StatefulSet to zero, or suspends a CronJob.
// The previous value is kept in an annotation; stopping twice keeps the first.
func (i *Inventory) Stop(ctx context.Context, r Resource) error {
	obj, err := i.get(ctx, r)
	if err != nil {
		return err
	}
	ann := obj.GetAnnotations()
	switch r.Kind {
	case "Deployment", "StatefulSet":
		prev, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
		if !found {
			prev = 1 // the API default
		}
		if v, ok := ann[AnnReplicas]; ok {
			prev, _ = strconv.ParseInt(v, 10, 64)
		}
		return i.patch(ctx, r, map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{AnnReplicas: strconv.FormatInt(prev, 10)}},
			"spec":     map[string]any{"replicas": 0},
		})
	case "CronJob":
		prev, _, _ := unstructured.NestedBool(obj.Object, "spec", "suspend")
		if v, ok := ann[AnnSuspended]; ok {
			prev = v == "true"
		}
		return i.patch(ctx, r, map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{AnnSuspended: strconv.FormatBool(prev)}},
			"spec":     map[string]any{"suspend": true},
		})
	}
	return fmt.Errorf("cannot stop %s", r)
}

// Restore undoes Stop from the annotation. A resource without it is left alone.
func (i *Inventory) Restore(ctx context.Context, r Resource) error {
	obj, err := i.get(ctx, r)
	if err != nil {
		return err
	}
	ann := obj.GetAnnotations()
	switch r.Kind {
	case "Deployment", "StatefulSet":
		v, ok := ann[AnnReplicas]
		if !ok {
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("%s: bad %s annotation %q", r, AnnReplicas, v)
		}
		return i.patch(ctx, r, map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{AnnReplicas: nil}},
			"spec":     map[string]any{"replicas": n},
		})
	case "CronJob":
		v, ok := ann[AnnSuspended]
		if !ok {
			return nil
		}
		return i.patch(ctx, r, map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{AnnSuspended: nil}},
			"spec":     map[string]any{"suspend": v == "true"},
		})
	}
	return fmt.Errorf("cannot restore %s", r)
}

// Running is the number of pods of a Deployment or StatefulSet that still
// exist, terminating ones included, or the active jobs of a CronJob. A pod
// that got its stop signal keeps running (and holding connections) until its
// graceful shutdown ends, while status.replicas already leaves it out.
func (i *Inventory) Running(ctx context.Context, r Resource) (int64, error) {
	obj, err := i.get(ctx, r)
	if err != nil {
		return 0, err
	}
	switch r.Kind {
	case "Deployment", "StatefulSet":
		raw, found, err := unstructured.NestedMap(obj.Object, "spec", "selector")
		if err != nil || !found {
			return 0, fmt.Errorf("%s has no spec.selector", r)
		}
		var ls metav1.LabelSelector
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &ls); err != nil {
			return 0, fmt.Errorf("%s selector: %w", r, err)
		}
		sel, err := metav1.LabelSelectorAsSelector(&ls)
		if err != nil {
			return 0, fmt.Errorf("%s selector: %w", r, err)
		}
		pods, err := i.Dyn.Resource(podGVR).Namespace(i.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
		if err != nil {
			return 0, fmt.Errorf("list pods of %s: %w", r, err)
		}
		var n int64
		for _, p := range pods.Items {
			switch phase, _, _ := unstructured.NestedString(p.Object, "status", "phase"); phase {
			case "Succeeded", "Failed":
			default:
				n++
			}
		}
		return n, nil
	case "CronJob":
		active, _, _ := unstructured.NestedSlice(obj.Object, "status", "active")
		return int64(len(active)), nil
	}
	return 0, fmt.Errorf("%s has no running count", r)
}

// Block creates (or replaces) a NetworkPolicy that drops all ingress to pods
// matching podSelector, except from the operator namespace.
func (i *Inventory) Block(ctx context.Context, name, podSelector string) error {
	sel, err := metav1.ParseToLabelSelector(podSelector)
	if err != nil {
		return fmt.Errorf("pod selector %q: %w", podSelector, err)
	}
	spec := map[string]any{"podSelector": sel, "policyTypes": []string{"Ingress"}, "ingress": []any{}}
	if i.OperatorNamespace != "" {
		spec["ingress"] = []any{map[string]any{"from": []any{map[string]any{"namespaceSelector": map[string]any{
			"matchLabels": map[string]any{"kubernetes.io/metadata.name": i.OperatorNamespace},
		}}}}}
	}
	np := map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata":   map[string]any{"name": name, "namespace": i.Namespace, "labels": map[string]any{LabelBlock: "true"}},
		"spec":       spec,
	}
	// Round-trip through JSON so typed values (the LabelSelector) become plain maps.
	raw, err := json.Marshal(np)
	if err != nil {
		return err
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(raw); err != nil {
		return err
	}
	res := i.Dyn.Resource(networkPolicyGVR).Namespace(i.Namespace)
	if _, err = res.Create(ctx, obj, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		cur, gerr := res.Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		obj.SetResourceVersion(cur.GetResourceVersion())
		_, err = res.Update(ctx, obj, metav1.UpdateOptions{})
	}
	return err
}

// Unblock deletes a policy made by Block. A missing policy is not an error.
func (i *Inventory) Unblock(ctx context.Context, name string) error {
	err := i.Dyn.Resource(networkPolicyGVR).Namespace(i.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
