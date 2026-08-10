package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// operationsDDL mirrors the schema-v12 operations table + indexes, copied verbatim
// from repowire/daemon/state/database.py. Stamps user_version=12 so NewStore opens.
const operationsDDL = `
CREATE TABLE IF NOT EXISTS operations (
    operation_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    state TEXT NOT NULL,
    target_json TEXT NOT NULL DEFAULT '{}',
    strategy TEXT,
    attempts_json TEXT NOT NULL DEFAULT '[]',
    result_json TEXT NOT NULL DEFAULT '{}',
    error_json TEXT NOT NULL DEFAULT '{}',
    provenance_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    completed_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_operations_kind_updated ON operations(kind, updated_at);
CREATE INDEX IF NOT EXISTS idx_operations_state_updated ON operations(state, updated_at);
PRAGMA user_version=12;
`

func newOperationsStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ops.db")
	seed, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if _, err := seed.Exec(operationsDDL); err != nil {
		t.Fatalf("apply DDL: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateAndGetOperation(t *testing.T) {
	s := newOperationsStore(t)
	ctx := context.Background()

	op, err := s.CreateOperation(ctx, "restart_peer",
		map[string]any{"peer_id": "peer-1"},
		map[string]any{"requested_by": "alice"},
	)
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}
	if op.OperationID == "" || op.OperationID[:3] != "op-" {
		t.Errorf("operation_id = %q, want op-* prefix", op.OperationID)
	}
	if op.State != "queued" {
		t.Errorf("state = %q, want queued", op.State)
	}
	if op.CreatedAt == "" || op.CreatedAt != op.UpdatedAt {
		t.Errorf("created/updated mismatch: %q vs %q", op.CreatedAt, op.UpdatedAt)
	}
	if op.Strategy != nil {
		t.Errorf("strategy should be nil, got %v", *op.Strategy)
	}
	if op.CompletedAt != nil {
		t.Errorf("completed_at should be nil, got %v", *op.CompletedAt)
	}

	got, err := s.GetOperation(ctx, op.OperationID)
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if got == nil {
		t.Fatal("GetOperation returned nil for existing op")
	}
	if got.Target["peer_id"] != "peer-1" {
		t.Errorf("target = %v", got.Target)
	}
	if got.Provenance["requested_by"] != "alice" {
		t.Errorf("provenance = %v", got.Provenance)
	}
	if len(got.Attempts) != 0 {
		t.Errorf("attempts should be empty, got %v", got.Attempts)
	}
}

func TestGetOperationMissing(t *testing.T) {
	s := newOperationsStore(t)
	got, err := s.GetOperation(context.Background(), "op-nope")
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing op, got %+v", got)
	}
}

func TestStartCompleteOperationLifecycle(t *testing.T) {
	s := newOperationsStore(t)
	ctx := context.Background()

	op, err := s.CreateOperation(ctx, "kill_peer", nil, nil)
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	strat := "tmux"
	started, err := s.StartAttempt(ctx, op.OperationID, &strat, map[string]any{"pane": "p1"})
	if err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}
	if started.State != "running" {
		t.Errorf("state = %q, want running", started.State)
	}
	if started.Strategy == nil || *started.Strategy != "tmux" {
		t.Errorf("strategy = %v, want tmux", started.Strategy)
	}
	if len(started.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(started.Attempts))
	}
	att := started.Attempts[0]
	if att["state"] != "running" || att["strategy"] != "tmux" {
		t.Errorf("attempt = %v", att)
	}
	if att["attempt_id"] == nil || att["attempt_id"].(string)[:11] != "op-attempt-" {
		t.Errorf("attempt_id = %v", att["attempt_id"])
	}

	completed, err := s.CompleteOperation(ctx, op.OperationID, nil, map[string]any{"ok": true})
	if err != nil {
		t.Fatalf("CompleteOperation: %v", err)
	}
	if completed.State != "completed" {
		t.Errorf("state = %q, want completed", completed.State)
	}
	if completed.CompletedAt == nil || *completed.CompletedAt == "" {
		t.Error("completed_at should be set")
	}
	if completed.Result["ok"] != true {
		t.Errorf("result = %v", completed.Result)
	}
	// Strategy carried over from the start attempt (nil passed to complete).
	if completed.Strategy == nil || *completed.Strategy != "tmux" {
		t.Errorf("strategy should carry over: %v", completed.Strategy)
	}
	last := completed.Attempts[0]
	if last["state"] != "completed" || last["completed_at"] == nil {
		t.Errorf("last attempt not finalized: %v", last)
	}
}

func TestFailOperation(t *testing.T) {
	s := newOperationsStore(t)
	ctx := context.Background()

	op, err := s.CreateOperation(ctx, "spawn", nil, nil)
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}
	if _, err := s.StartAttempt(ctx, op.OperationID, nil, nil); err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}

	failed, err := s.FailOperation(ctx, op.OperationID, "", nil, map[string]any{"reason": "no pane"})
	if err != nil {
		t.Fatalf("FailOperation: %v", err)
	}
	if failed.State != "failed" {
		t.Errorf("state = %q, want failed (default)", failed.State)
	}
	if failed.Error["reason"] != "no pane" {
		t.Errorf("error = %v", failed.Error)
	}
	if failed.CompletedAt == nil {
		t.Error("completed_at should be set on fail")
	}

	// Explicit non-default terminal state.
	op2, _ := s.CreateOperation(ctx, "spawn", nil, nil)
	_, _ = s.StartAttempt(ctx, op2.OperationID, nil, nil)
	cancelled, err := s.FailOperation(ctx, op2.OperationID, "cancelled", nil, nil)
	if err != nil {
		t.Fatalf("FailOperation cancelled: %v", err)
	}
	if cancelled.State != "cancelled" {
		t.Errorf("state = %q, want cancelled", cancelled.State)
	}
}

func TestListOperationsFilters(t *testing.T) {
	s := newOperationsStore(t)
	ctx := context.Background()

	a, _ := s.CreateOperation(ctx, "restart_peer", nil, nil)
	_, _ = s.CreateOperation(ctx, "kill_peer", nil, nil)
	if _, err := s.CompleteOperation(ctx, a.OperationID, nil, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	all, err := s.ListOperations(ctx, "", "")
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(all) = %d, want 2", len(all))
	}

	byKind, err := s.ListOperations(ctx, "kill_peer", "")
	if err != nil {
		t.Fatalf("ListOperations kind: %v", err)
	}
	if len(byKind) != 1 || byKind[0].Kind != "kill_peer" {
		t.Errorf("kind filter = %+v", byKind)
	}

	byState, err := s.ListOperations(ctx, "", "completed")
	if err != nil {
		t.Fatalf("ListOperations state: %v", err)
	}
	if len(byState) != 1 || byState[0].State != "completed" {
		t.Errorf("state filter = %+v", byState)
	}

	none, err := s.ListOperations(ctx, "restart_peer", "queued")
	if err != nil {
		t.Fatalf("ListOperations both: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("restart_peer+queued should be empty (it completed), got %+v", none)
	}
}

func TestStartAttemptMissingOperation(t *testing.T) {
	s := newOperationsStore(t)
	op, err := s.StartAttempt(context.Background(), "op-missing", nil, nil)
	if err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}
	if op != nil {
		t.Errorf("expected nil for missing op, got %+v", op)
	}
}
