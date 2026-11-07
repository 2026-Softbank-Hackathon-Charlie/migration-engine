package cnpg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const WriteFenceAnnotation = "hopcharlie.dev/write-fence-lease"

// HBASnapshot persists only identity and a hash. HBA options may contain secrets.
type HBASnapshot struct {
	UID    string `json:"uid"`
	Hash   string `json:"originalHbaHash"`
	Absent bool   `json:"originalHbaAbsent"`
}

var writerRole = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func WriterRules(roles []string) ([]string, error) {
	if len(roles) == 0 || len(roles) > 16 {
		return nil, errors.New("write fence requires 1 to 16 application login roles")
	}
	seen := map[string]bool{}
	rules := make([]string, 0, len(roles)*2)
	for _, role := range roles {
		if !writerRole.MatchString(role) || strings.HasPrefix(role, "pg_") || role == "postgres" || role == "streaming_replica" || role == "cnpg_pooler_pgbouncer" || seen[role] {
			return nil, errors.New("write fence roles must be unique ordinary lowercase application roles")
		}
		seen[role] = true
		rules = append(rules, "host all "+role+" 0.0.0.0/0 reject", "host all "+role+" ::/0 reject")
	}
	return rules, nil
}

func hbaHash(rules []string) string {
	if rules == nil {
		rules = []string{}
	}
	b, _ := json.Marshal(rules)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func readHBA(obj *unstructured.Unstructured) ([]string, bool, error) {
	rules, found, err := unstructured.NestedStringSlice(obj.Object, "spec", "postgresql", "pg_hba")
	return rules, !found, err
}

func (c *Client) InspectWriteFence(ctx context.Context) (HBASnapshot, error) {
	obj, err := c.res().Get(ctx, c.Name, metav1.GetOptions{})
	if err != nil {
		return HBASnapshot{}, err
	}
	if !statusFrom(obj).Healthy() || !statusFrom(obj).IsPrimary() || obj.GetUID() == "" {
		return HBASnapshot{}, errors.New("write fence requires an identified healthy source primary")
	}
	if _, exists := obj.GetAnnotations()[WriteFenceAnnotation]; exists {
		return HBASnapshot{}, errors.New("source already has a write fence owner")
	}
	rules, absent, err := readHBA(obj)
	return HBASnapshot{UID: string(obj.GetUID()), Hash: hbaHash(rules), Absent: absent}, err
}

// SetWriteFence changes only the user HBA prefix and its ownership annotation.
// UID/resourceVersion tests make a replacement or concurrent update fail closed.
func (c *Client) SetWriteFence(ctx context.Context, snapshot HBASnapshot, lease string, roles []string) error {
	return c.setWriteFence(ctx, snapshot, lease, roles, false)
}

// DryRunWriteFence exercises API authorization/admission before writes are held.
func (c *Client) DryRunWriteFence(ctx context.Context, snapshot HBASnapshot, lease string, roles []string) error {
	return c.setWriteFence(ctx, snapshot, lease, roles, true)
}

func (c *Client) setWriteFence(ctx context.Context, snapshot HBASnapshot, lease string, roles []string, dryRun bool) error {
	prefix, err := WriterRules(roles)
	if err != nil {
		return err
	}
	if b, e := hex.DecodeString(lease); e != nil || len(b) != 16 {
		return errors.New("invalid write fence lease")
	}
	obj, err := c.res().Get(ctx, c.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	original, absent, err := readHBA(obj)
	if err != nil {
		return err
	}
	if string(obj.GetUID()) != snapshot.UID || obj.GetResourceVersion() == "" || hbaHash(original) != snapshot.Hash || absent != snapshot.Absent || !statusFrom(obj).IsPrimary() {
		return errors.New("source identity, role, or HBA changed since write fence precheck")
	}
	if _, exists := obj.GetAnnotations()[WriteFenceAnnotation]; exists {
		return errors.New("source write fence is already owned")
	}
	patch := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": snapshot.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": obj.GetResourceVersion()},
	}
	if obj.GetAnnotations() == nil {
		patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations", "value": map[string]string{WriteFenceAnnotation: lease}})
	} else {
		patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations/hopcharlie.dev~1write-fence-lease", "value": lease})
	}
	rules := append(append([]string{}, prefix...), original...)
	_, hasPG, err := unstructured.NestedMap(obj.Object, "spec", "postgresql")
	if err != nil {
		return err
	}
	if !hasPG {
		patch = append(patch, map[string]any{"op": "add", "path": "/spec/postgresql", "value": map[string]any{"pg_hba": rules}})
	} else {
		patch = append(patch, map[string]any{"op": "add", "path": "/spec/postgresql/pg_hba", "value": rules})
	}
	return c.patchWriteFence(ctx, patch, dryRun)
}

func (c *Client) CheckWriteFence(ctx context.Context, snapshot HBASnapshot, lease string, roles []string) error {
	obj, err := c.res().Get(ctx, c.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	return matchingFence(obj, snapshot, lease, roles)
}

func matchingFence(obj *unstructured.Unstructured, snapshot HBASnapshot, lease string, roles []string) error {
	prefix, err := WriterRules(roles)
	if err != nil {
		return err
	}
	rules, _, err := readHBA(obj)
	if err != nil {
		return err
	}
	if string(obj.GetUID()) != snapshot.UID || obj.GetAnnotations()[WriteFenceAnnotation] != lease || len(rules) < len(prefix) || !reflect.DeepEqual(rules[:len(prefix)], prefix) || hbaHash(rules[len(prefix):]) != snapshot.Hash {
		return errors.New("write fence identity, owner, prefix or original HBA hash changed; operator reconciliation required")
	}
	return nil
}

// ClearWriteFence removes only this run's exact prefix. Any third-party edit is
// preserved by refusing cleanup. Source role is checked in the same CAS patch.
func (c *Client) ClearWriteFence(ctx context.Context, snapshot HBASnapshot, lease string, roles []string, sourcePrimary bool) error {
	obj, err := c.res().Get(ctx, c.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if _, owned := obj.GetAnnotations()[WriteFenceAnnotation]; !owned {
		original, absent, e := readHBA(obj)
		if e == nil && string(obj.GetUID()) == snapshot.UID && hbaHash(original) == snapshot.Hash && absent == snapshot.Absent && statusFrom(obj).IsPrimary() == sourcePrimary {
			return nil
		}
	}
	if err = matchingFence(obj, snapshot, lease, roles); err != nil {
		return err
	}
	if statusFrom(obj).IsPrimary() != sourcePrimary {
		return errors.New("source topology role changed before write fence cleanup")
	}
	rules, _, _ := readHBA(obj)
	prefix, _ := WriterRules(roles)
	patch := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": snapshot.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": obj.GetResourceVersion()},
		{"op": "remove", "path": "/metadata/annotations/hopcharlie.dev~1write-fence-lease"},
	}
	if snapshot.Absent {
		patch = append(patch, map[string]any{"op": "remove", "path": "/spec/postgresql/pg_hba"})
	} else {
		patch = append(patch, map[string]any{"op": "replace", "path": "/spec/postgresql/pg_hba", "value": rules[len(prefix):]})
	}
	return c.patchWriteFence(ctx, patch, false)
}

func (c *Client) patchWriteFence(ctx context.Context, patch []map[string]any, dryRun bool) error {
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	options := metav1.PatchOptions{FieldManager: fieldManager}
	if dryRun {
		options.DryRun = []string{metav1.DryRunAll}
	}
	_, err = c.res().Patch(ctx, c.Name, types.JSONPatchType, b, options)
	if err != nil {
		return fmt.Errorf("source write fence patch rejected: %w", err)
	}
	return nil
}
