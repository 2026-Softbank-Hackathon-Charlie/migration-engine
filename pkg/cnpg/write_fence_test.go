package cnpg

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const testFenceLease = "0123456789abcdef0123456789abcdef"

func fenceFixture(t *testing.T) (*Client, HBASnapshot) {
	t.Helper()
	o := cluster("source", map[string]any{"self": "source", "primary": "source", "source": "target"}, healthy())
	o.SetUID(types.UID("source-uid"))
	o.SetResourceVersion("7")
	// An authentication option must stay out of persisted evidence.
	_ = unstructured.SetNestedStringSlice(o.Object, []string{"host all all 10.0.0.0/8 ldap ldapbindpasswd=do-not-record"}, "spec", "postgresql", "pg_hba")
	c := New(newFake(o), "shop", "source")
	s, err := c.InspectWriteFence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "ldap") || strings.Contains(string(b), "record") {
		t.Fatal("HBA source leaked into snapshot")
	}
	return c, s
}

func TestSourceHBAFenceCASAndOwnedCleanup(t *testing.T) {
	c, s := fenceFixture(t)
	ctx := context.Background()
	if err := c.SetWriteFence(ctx, s, testFenceLease, []string{"app"}); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckWriteFence(ctx, s, testFenceLease, []string{"app"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ClearWriteFence(ctx, s, testFenceLease, []string{"app"}, true); err != nil {
		t.Fatal(err)
	}
	o, err := c.res().Get(ctx, c.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rules, _, _ := readHBA(o)
	if hbaHash(rules) != s.Hash || o.GetAnnotations()[WriteFenceAnnotation] != "" {
		t.Fatal("original HBA not restored")
	}
}

func TestSourceHBAFencePreservesThirdPartyEdits(t *testing.T) {
	for _, change := range []string{"hba", "lease", "uid", "role"} {
		t.Run(change, func(t *testing.T) {
			c, s := fenceFixture(t)
			ctx := context.Background()
			if err := c.SetWriteFence(ctx, s, testFenceLease, []string{"app"}); err != nil {
				t.Fatal(err)
			}
			o, _ := c.res().Get(ctx, c.Name, metav1.GetOptions{})
			switch change {
			case "hba":
				r, _, _ := readHBA(o)
				_ = unstructured.SetNestedStringSlice(o.Object, append(r, "host all audit 192.0.2.0/24 reject"), "spec", "postgresql", "pg_hba")
			case "lease":
				a := o.GetAnnotations()
				a[WriteFenceAnnotation] = "another-owner"
				o.SetAnnotations(a)
			case "uid":
				o.SetUID(types.UID("replacement"))
			case "role":
				_ = unstructured.SetNestedField(o.Object, "target", "spec", "replica", "primary")
			}
			_, _ = c.res().Update(ctx, o, metav1.UpdateOptions{})
			if err := c.ClearWriteFence(ctx, s, testFenceLease, []string{"app"}, true); err == nil {
				t.Fatal("foreign mutation was silently cleared")
			}
		})
	}
}

func TestSourceHBAFenceRejectsStaleSnapshotAndReservedRoles(t *testing.T) {
	c, s := fenceFixture(t)
	s.UID = "stale"
	if err := c.SetWriteFence(context.Background(), s, testFenceLease, []string{"app"}); err == nil {
		t.Fatal("stale identity accepted")
	}
	for _, roles := range [][]string{nil, {"postgres"}, {"pg_read_all_data"}, {"streaming_replica"}, {"cnpg_pooler_pgbouncer"}, {"app\nlocal all all trust"}, {"app", "app"}} {
		if _, err := WriterRules(roles); err == nil {
			t.Fatalf("unsafe roles accepted: %v", roles)
		}
	}
}
