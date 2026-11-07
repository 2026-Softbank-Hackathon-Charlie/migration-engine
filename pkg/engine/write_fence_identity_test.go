package engine

import (
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
)

func TestDemotedSourceMissingAPIIdentifierRequiresRealSQLIdentity(t *testing.T) {
	snapshot := cnpg.HBASnapshot{UID: "source-uid", SystemIdentifier: "12345"}
	base := cnpg.Status{UID: "source-uid", Instances: 1, Self: "source", ReplicaPrimary: "target", DemotionToken: "fresh-token"}
	if err := assertDemotedSourceIdentity(&base, snapshot, "fresh-token", "12345"); err != nil {
		t.Fatalf("direct SQL identity was discarded because optional API observation is absent: %v", err)
	}
	for _, sqlID := range []string{"", "0", "99999", "not-a-system-id"} {
		if err := assertDemotedSourceIdentity(&base, snapshot, "fresh-token", sqlID); err == nil {
			t.Fatalf("unknown/mismatched SQL identifier accepted: %q", sqlID)
		}
	}
	for _, change := range []func(*cnpg.Status){func(s *cnpg.Status) { s.SystemIdentifier = "99999" }, func(s *cnpg.Status) { s.UID = "replacement" }, func(s *cnpg.Status) { s.ReplicaPrimary = "source" }, func(s *cnpg.Status) { s.DemotionToken = "different" }, func(s *cnpg.Status) { s.Instances = 2 }} {
		s := base
		change(&s)
		if err := assertDemotedSourceIdentity(&s, snapshot, "fresh-token", "12345"); err == nil {
			t.Fatal("conflicting source API identity/topology accepted")
		}
	}
	snapshot.SystemIdentifier = "0"
	if err := assertDemotedSourceIdentity(&base, snapshot, "fresh-token", "0"); err == nil {
		t.Fatal("two zero identifiers were accepted as proof")
	}
}
