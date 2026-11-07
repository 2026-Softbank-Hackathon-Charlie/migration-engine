//go:build integration

package pg

import (
	"context"
	"testing"
)

func TestSettleStatusMatchesIndividualSignals(t *testing.T) {
	db := connectEnv(t, "HOPCHARLIE_IT_PRIMARY_DSN")
	ctx := t.Context()
	recovery, err := db.InRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingRestart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	started, err := db.PostmasterStart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gotRecovery, gotPending, gotStarted, err := db.SettleStatus(ctx)
	if err != nil || gotRecovery != recovery || gotPending != pending || !gotStarted.Equal(started) || gotStarted.IsZero() {
		t.Fatalf("combined signals mismatch: recovery=%v pending=%v start=%v err=%v", gotRecovery, gotPending, gotStarted, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, _, err := db.SettleStatus(cancelled); err == nil {
		t.Fatal("cancelled observation succeeded")
	}
}
