package engine

import (
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
)

func TestCheckTopology(t *testing.T) {
	src := &cnpg.Status{Self: "db-gke", ReplicaPrimary: "db-gke", ReplicaSource: "db-eks"}
	dst := &cnpg.Status{Self: "db-eks", ReplicaPrimary: "db-gke", ReplicaSource: "db-gke"}
	if err := checkTopology(src, dst); err != nil {
		t.Fatalf("valid topology rejected: %v", err)
	}
	bad := *dst
	bad.ReplicaPrimary = "db-eks"
	if checkTopology(src, &bad) == nil {
		t.Error("target claiming primary should be rejected")
	}
	noBack := *src
	noBack.ReplicaSource = "something-else"
	if checkTopology(&noBack, dst) == nil {
		t.Error("source not pointing back at target should be rejected")
	}
	if !src.IsPrimary() || dst.IsPrimary() {
		t.Error("IsPrimary wrong")
	}
}
