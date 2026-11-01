// Package cnpg drives CloudNativePG Cluster resources using the distributed
// topology (spec.replica.primary / self / source + promotionToken), which gives
// a controlled switchover without data loss across Kubernetes clusters.
package cnpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

// ClusterGVR is the CNPG Cluster resource.
var ClusterGVR = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}

// HealthyPhase is the status.phase CNPG reports for a healthy cluster.
const HealthyPhase = "Cluster in healthy state"

const fieldManager = "hopcharlie-migration-engine"

// Client operates on one CNPG Cluster.
type Client struct {
	dyn       dynamic.Interface
	Namespace string
	Name      string
}

// New returns a Client for namespace/name.
func New(dyn dynamic.Interface, namespace, name string) *Client {
	return &Client{dyn: dyn, Namespace: namespace, Name: name}
}

// Status is the subset of the Cluster resource the engine cares about.
type Status struct {
	Phase          string `json:"phase"`
	CurrentPrimary string `json:"currentPrimary"`
	Instances      int64  `json:"instances"`
	ReadyInstances int64  `json:"readyInstances"`
	DemotionToken  string `json:"-"`
	HasDemotion    bool   `json:"hasDemotionToken"`
	// Distributed topology fields from spec.replica.
	Self           string `json:"self"`
	ReplicaPrimary string `json:"replicaPrimary"`
	ReplicaSource  string `json:"replicaSource"`
}

// Healthy reports whether CNPG considers the cluster healthy.
func (s *Status) Healthy() bool {
	return s.Phase == HealthyPhase && s.ReadyInstances > 0 && s.ReadyInstances == s.Instances
}

// IsPrimary reports whether this cluster is the designated primary of the topology.
func (s *Status) IsPrimary() bool { return s.ReplicaPrimary == "" || s.ReplicaPrimary == s.Self }

func (c *Client) res() dynamic.ResourceInterface {
	return c.dyn.Resource(ClusterGVR).Namespace(c.Namespace)
}

// ErrNotFound is returned when the Cluster resource does not exist.
var ErrNotFound = errors.New("cnpg cluster not found")

// Get fetches the cluster status.
func (c *Client) Get(ctx context.Context) (*Status, error) {
	obj, err := c.res().Get(ctx, c.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%s/%s: %w", c.Namespace, c.Name, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get cluster %s/%s: %w", c.Namespace, c.Name, err)
	}
	return statusFrom(obj), nil
}

func statusFrom(obj *unstructured.Unstructured) *Status {
	s := &Status{}
	s.Phase, _, _ = unstructured.NestedString(obj.Object, "status", "phase")
	s.CurrentPrimary, _, _ = unstructured.NestedString(obj.Object, "status", "currentPrimary")
	s.Instances, _, _ = unstructured.NestedInt64(obj.Object, "spec", "instances")
	s.ReadyInstances, _, _ = unstructured.NestedInt64(obj.Object, "status", "readyInstances")
	s.DemotionToken, _, _ = unstructured.NestedString(obj.Object, "status", "demotionToken")
	s.HasDemotion = s.DemotionToken != ""
	s.Self, _, _ = unstructured.NestedString(obj.Object, "spec", "replica", "self")
	if s.Self == "" {
		s.Self = obj.GetName()
	}
	s.ReplicaPrimary, _, _ = unstructured.NestedString(obj.Object, "spec", "replica", "primary")
	s.ReplicaSource, _, _ = unstructured.NestedString(obj.Object, "spec", "replica", "source")
	return s
}

// ApplyManifest server-side-applies a Cluster manifest (YAML) into this client's namespace.
func (c *Client) ApplyManifest(ctx context.Context, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	js, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return fmt.Errorf("parse manifest %s: %w", path, err)
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(js); err != nil {
		return fmt.Errorf("decode manifest %s: %w", path, err)
	}
	if obj.GetKind() != "Cluster" || obj.GroupVersionKind().Group != ClusterGVR.Group {
		return fmt.Errorf("manifest %s is %s, want postgresql.cnpg.io Cluster", path, obj.GroupVersionKind())
	}
	if obj.GetName() != c.Name {
		return fmt.Errorf("manifest %s names cluster %q, plan expects %q", path, obj.GetName(), c.Name)
	}
	obj.SetNamespace(c.Namespace)
	_, err = c.res().Apply(ctx, c.Name, obj, metav1.ApplyOptions{FieldManager: fieldManager, Force: true})
	if err != nil {
		return fmt.Errorf("apply cluster %s/%s: %w", c.Namespace, c.Name, err)
	}
	return nil
}

// SetPrimary sets spec.replica.primary. A non-empty promotionToken is set
// alongside; an empty one removes any existing token.
//
// On the current primary, pointing primary elsewhere demotes it (CNPG then
// publishes status.demotionToken). On a replica, pointing primary at itself
// with the token promotes it once it has replayed up to the token's LSN.
func (c *Client) SetPrimary(ctx context.Context, primary, promotionToken string) error {
	var token any // nil => JSON null => field removed by merge patch
	if promotionToken != "" {
		token = promotionToken
	}
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"replica": map[string]any{"primary": primary, "promotionToken": token},
		},
	})
	if err != nil {
		return err
	}
	_, err = c.res().Patch(ctx, c.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: fieldManager})
	if err != nil {
		return fmt.Errorf("patch %s/%s replica.primary=%s: %w", c.Namespace, c.Name, primary, err)
	}
	return nil
}

// WaitFor polls until cond is true or the context ends.
func (c *Client) WaitFor(ctx context.Context, interval time.Duration, cond func(*Status) bool) (*Status, error) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s, err := c.Get(ctx)
		if err == nil && cond(s) {
			return s, nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return nil, fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
			}
			return s, ctx.Err()
		case <-t.C:
		}
	}
}
