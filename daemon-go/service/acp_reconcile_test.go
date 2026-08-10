package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

func TestReconcileACPInflightFailsAndQueuesClosure(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	op, err := store.CreateOperation(ctx, acpAskOperationKind, map[string]any{
		"correlation_id": "ask-1", "from_peer_id": "asker-id", "from_peer_name": "asker",
		"to_peer_id": "target-id", "to_peer_name": "target",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartAttempt(ctx, op.OperationID, strPtr("acp_prompt"), nil); err != nil {
		t.Fatal(err)
	}
	if got := ReconcileACPInflight(ctx, store, 60, 10); got != 1 {
		t.Fatalf("reconciled = %d, want 1", got)
	}
	updated, err := store.GetOperation(ctx, op.OperationID)
	if err != nil || updated.State != "failed" {
		t.Fatalf("operation = %+v, %v", updated, err)
	}
	queued, err := store.ListDeliveries(ctx, "asker-id", 10, time.Time{})
	if err != nil || len(queued) != 1 || queued[0].Text != "[ack #ask-1 from @target] ACP ask lost across daemon restart; please retry." {
		t.Fatalf("queued = %+v, %v", queued, err)
	}
	if got := ReconcileACPInflight(ctx, store, 60, 10); got != 0 {
		t.Fatalf("second reconcile = %d, want 0", got)
	}
}

func TestReconcileACPInflightKeepsOperationWhenClosureCannotQueue(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	op, err := store.CreateOperation(ctx, acpAskOperationKind, map[string]any{
		"correlation_id": "ask-1", "from_peer_id": "asker-id", "from_peer_name": "asker",
		"to_peer_id": "target-id", "to_peer_name": "target",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartAttempt(ctx, op.OperationID, strPtr("acp_prompt"), nil); err != nil {
		t.Fatal(err)
	}
	if got := ReconcileACPInflight(ctx, store, 0, 0); got != 0 {
		t.Fatalf("reconciled = %d, want 0", got)
	}
	updated, err := store.GetOperation(ctx, op.OperationID)
	if err != nil || updated.State != "running" {
		t.Fatalf("operation = %+v, %v; want running for retry", updated, err)
	}
}
