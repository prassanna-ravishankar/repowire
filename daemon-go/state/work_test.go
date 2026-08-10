package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// workDDL is the tracked_work table + indexes, copied verbatim from schema-v12 in
// repowire/daemon/state/database.py. It stamps user_version=12 so NewStore opens.
const workDDL = `
CREATE TABLE IF NOT EXISTS tracked_work (
    work_id TEXT PRIMARY KEY,
    title TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL DEFAULT 'general',
    state TEXT NOT NULL,
    state_reason TEXT,
    phase TEXT,
    progress_json TEXT NOT NULL DEFAULT '{}',
    progress_events_json TEXT NOT NULL DEFAULT '[]',
    owner_peer_id TEXT,
    assigned_peer_id TEXT,
    repowire_session_id TEXT,
    correlation_id TEXT,
    circle TEXT,
    created_by_peer_id TEXT,
    source_kind TEXT,
    source_id TEXT,
    scope TEXT,
    visibility TEXT NOT NULL DEFAULT 'circle',
    request_json TEXT NOT NULL DEFAULT '{}',
    deadline_at TEXT,
    expires_at TEXT,
    result_summary TEXT,
    result_data_json TEXT NOT NULL DEFAULT '{}',
    error_json TEXT NOT NULL DEFAULT '{}',
    artifacts_json TEXT NOT NULL DEFAULT '[]',
    provenance_json TEXT NOT NULL DEFAULT '{}',
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    cancel_requested_at TEXT,
    cancel_requested_by_peer_id TEXT,
    cancellation_reason TEXT,
    completed_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tracked_work_state ON tracked_work(state);
CREATE INDEX IF NOT EXISTS idx_tracked_work_owner_updated ON tracked_work(owner_peer_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_tracked_work_session_updated ON tracked_work(repowire_session_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_tracked_work_circle_updated ON tracked_work(circle, updated_at);
PRAGMA user_version=12;
`

func newWorkStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seed, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if _, err := seed.Exec(workDDL); err != nil {
		t.Fatalf("apply work DDL: %v", err)
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

func TestCreateGetWork(t *testing.T) {
	s := newWorkStore(t)
	ctx := context.Background()

	w, err := s.CreateWork(ctx, WorkCreate{
		Title:       "investigate flake",
		OwnerPeerID: strp("peer-A"),
		Circle:      strp("default"),
		Request:     map[string]any{"prompt": "look into it"},
	})
	if err != nil {
		t.Fatalf("CreateWork: %v", err)
	}
	if w.State != "queued" {
		t.Fatalf("new work state = %q, want queued", w.State)
	}
	if w.Kind != "general" || w.Visibility != "circle" {
		t.Fatalf("defaults not applied: kind=%q visibility=%q", w.Kind, w.Visibility)
	}

	got, err := s.GetWork(ctx, w.WorkID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if got == nil {
		t.Fatal("GetWork returned nil for an existing row")
	}
	if got.Title != "investigate flake" || *got.OwnerPeerID != "peer-A" {
		t.Fatalf("round-trip mismatch: title=%q owner=%v", got.Title, got.OwnerPeerID)
	}
	if got.Request["prompt"] != "look into it" {
		t.Fatalf("request_json round-trip mismatch: %#v", got.Request)
	}

	missing, err := s.GetWork(ctx, "work-nope")
	if err != nil {
		t.Fatalf("GetWork(missing): %v", err)
	}
	if missing != nil {
		t.Fatal("GetWork(missing) should be nil")
	}
}

func TestListWorkFilters(t *testing.T) {
	s := newWorkStore(t)
	ctx := context.Background()

	for _, c := range []WorkCreate{
		{OwnerPeerID: strp("peer-A"), Circle: strp("c1"), RepowireSessionID: strp("sess-1")},
		{OwnerPeerID: strp("peer-B"), Circle: strp("c1"), RepowireSessionID: strp("sess-2")},
		{OwnerPeerID: strp("peer-A"), Circle: strp("c2"), RepowireSessionID: strp("sess-1")},
	} {
		if _, err := s.CreateWork(ctx, c); err != nil {
			t.Fatalf("CreateWork: %v", err)
		}
	}

	byOwner, err := s.ListWork(ctx, WorkFilter{OwnerPeerID: strp("peer-A")})
	if err != nil {
		t.Fatalf("ListWork owner: %v", err)
	}
	if len(byOwner) != 2 {
		t.Fatalf("owner filter = %d rows, want 2", len(byOwner))
	}

	byCircle, err := s.ListWork(ctx, WorkFilter{Circle: strp("c1")})
	if err != nil {
		t.Fatalf("ListWork circle: %v", err)
	}
	if len(byCircle) != 2 {
		t.Fatalf("circle filter = %d rows, want 2", len(byCircle))
	}

	bySession, err := s.ListWork(ctx, WorkFilter{RepowireSessionID: strp("sess-1")})
	if err != nil {
		t.Fatalf("ListWork session: %v", err)
	}
	if len(bySession) != 2 {
		t.Fatalf("session filter = %d rows, want 2", len(bySession))
	}

	all, err := s.ListWork(ctx, WorkFilter{})
	if err != nil {
		t.Fatalf("ListWork all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered = %d rows, want 3", len(all))
	}

	if _, err := s.ListWork(ctx, WorkFilter{State: strp("bogus")}); err == nil {
		t.Fatal("ListWork with invalid state should error")
	}
}

func TestUpdateWorkStateTransitions(t *testing.T) {
	s := newWorkStore(t)
	ctx := context.Background()

	w, err := s.CreateWork(ctx, WorkCreate{Title: "job"})
	if err != nil {
		t.Fatalf("CreateWork: %v", err)
	}

	running, err := s.UpdateWorkState(ctx, w.WorkID, WorkUpdate{
		State:        "running",
		Phase:        strp("executing"),
		ProgressNote: strp("started"),
	})
	if err != nil {
		t.Fatalf("UpdateWorkState running: %v", err)
	}
	if running.State != "running" || *running.Phase != "executing" {
		t.Fatalf("running transition mismatch: state=%q phase=%v", running.State, running.Phase)
	}
	if len(running.ProgressEvents) != 1 {
		t.Fatalf("progress_note should append one event, got %d", len(running.ProgressEvents))
	}
	if running.CompletedAt != nil {
		t.Fatal("non-terminal state must not set completed_at")
	}

	done, err := s.UpdateWorkState(ctx, w.WorkID, WorkUpdate{
		State:         "completed",
		ResultSummary: strp("all green"),
		ResultData:    map[string]any{"passed": true},
	})
	if err != nil {
		t.Fatalf("UpdateWorkState completed: %v", err)
	}
	if !done.Terminal() || done.CompletedAt == nil {
		t.Fatalf("completed should be terminal with completed_at set: %#v", done)
	}
	if *done.ResultSummary != "all green" {
		t.Fatalf("result_summary mismatch: %v", done.ResultSummary)
	}

	if _, err := s.UpdateWorkState(ctx, w.WorkID, WorkUpdate{State: "running"}); err == nil {
		t.Fatal("changing a terminal work's state should error")
	}

	none, err := s.UpdateWorkState(ctx, "work-missing", WorkUpdate{State: "running"})
	if err != nil {
		t.Fatalf("UpdateWorkState(missing): %v", err)
	}
	if none != nil {
		t.Fatal("UpdateWorkState(missing) should be nil")
	}
}

func TestCancelWork(t *testing.T) {
	s := newWorkStore(t)
	ctx := context.Background()

	// queued -> cancelled directly
	queued, err := s.CreateWork(ctx, WorkCreate{Title: "q"})
	if err != nil {
		t.Fatalf("CreateWork: %v", err)
	}
	cancelled, err := s.CancelWork(ctx, queued.WorkID, strp("peer-X"), "")
	if err != nil {
		t.Fatalf("CancelWork queued: %v", err)
	}
	if cancelled.State != "cancelled" || !cancelled.CancelRequested || cancelled.CompletedAt == nil {
		t.Fatalf("queued cancel should be terminal cancelled: %#v", cancelled)
	}
	if *cancelled.CancellationReason != "cancel_requested" {
		t.Fatalf("default reason mismatch: %v", cancelled.CancellationReason)
	}

	// in-flight -> flagged but not state-changed
	running, err := s.CreateWork(ctx, WorkCreate{Title: "r"})
	if err != nil {
		t.Fatalf("CreateWork: %v", err)
	}
	if _, err := s.UpdateWorkState(ctx, running.WorkID, WorkUpdate{State: "running"}); err != nil {
		t.Fatalf("to running: %v", err)
	}
	flagged, err := s.CancelWork(ctx, running.WorkID, nil, "user_abort")
	if err != nil {
		t.Fatalf("CancelWork running: %v", err)
	}
	if flagged.State != "running" || !flagged.CancelRequested {
		t.Fatalf("in-flight cancel should flag without changing state: %#v", flagged)
	}
	if *flagged.CancellationReason != "user_abort" {
		t.Fatalf("reason mismatch: %v", flagged.CancellationReason)
	}
}

func TestAcquireForDispatchAndRetry(t *testing.T) {
	s := newWorkStore(t)
	ctx := context.Background()

	w, err := s.CreateWork(ctx, WorkCreate{Title: "dispatchable"})
	if err != nil {
		t.Fatalf("CreateWork: %v", err)
	}

	acquired, err := s.AcquireForDispatch(ctx, w.WorkID, AcquireOptions{
		RunnerOwnerID: "runner-1",
		LeaseUntil:    "2099-01-01T00:00:00+00:00",
	})
	if err != nil {
		t.Fatalf("AcquireForDispatch: %v", err)
	}
	if acquired == nil || acquired.State != "dispatching" {
		t.Fatalf("acquire should move queued->dispatching: %#v", acquired)
	}
	if runnerCurrentAttempt(acquired) == "" {
		t.Fatal("acquire should record a current attempt id")
	}

	// queued is consumed; a second non-retry acquire on dispatching is rejected.
	again, err := s.AcquireForDispatch(ctx, w.WorkID, AcquireOptions{RunnerOwnerID: "runner-1", LeaseUntil: "2099-01-01T00:00:00+00:00"})
	if err != nil {
		t.Fatalf("AcquireForDispatch second: %v", err)
	}
	if again != nil {
		t.Fatal("dispatching work should not be acquirable without retry")
	}

	// Drive it to failed (a runner-managed terminal transition needs the
	// current attempt id), then retry should re-acquire.
	attemptID := runnerCurrentAttempt(acquired)
	if _, err := s.UpdateWorkState(ctx, w.WorkID, WorkUpdate{State: "failed", AttemptID: &attemptID}); err != nil {
		t.Fatalf("to failed: %v", err)
	}
	retried, err := s.AcquireForDispatch(ctx, w.WorkID, AcquireOptions{
		RunnerOwnerID: "runner-2",
		LeaseUntil:    "2099-01-01T00:00:00+00:00",
		Retry:         true,
	})
	if err != nil {
		t.Fatalf("AcquireForDispatch retry: %v", err)
	}
	if retried == nil || retried.State != "dispatching" {
		t.Fatalf("retry should re-acquire failed work: %#v", retried)
	}

	// cancel-requested work is never acquired.
	cw, err := s.CreateWork(ctx, WorkCreate{Title: "cancel-me"})
	if err != nil {
		t.Fatalf("CreateWork: %v", err)
	}
	if _, err := s.UpdateWorkState(ctx, cw.WorkID, WorkUpdate{State: "running"}); err != nil {
		t.Fatalf("to running: %v", err)
	}
	if _, err := s.CancelWork(ctx, cw.WorkID, nil, ""); err != nil {
		t.Fatalf("CancelWork: %v", err)
	}
	noAcq, err := s.AcquireForDispatch(ctx, cw.WorkID, AcquireOptions{RunnerOwnerID: "r", LeaseUntil: "2099-01-01T00:00:00+00:00", Retry: true})
	if err != nil {
		t.Fatalf("AcquireForDispatch cancelled: %v", err)
	}
	if noAcq != nil {
		t.Fatal("cancel-requested work must not be acquired")
	}
}
