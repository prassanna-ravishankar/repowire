package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// sessionBindingsDDL is the schema-v12 session_bindings table, copied verbatim
// from repowire/daemon/state/database.py. It stamps user_version=12 so NewStore
// opens.
const sessionBindingsDDL = `
CREATE TABLE IF NOT EXISTS session_bindings (
    repowire_session_id TEXT PRIMARY KEY,
    peer_id TEXT,
    current_executor_peer_id TEXT,
    backend TEXT NOT NULL,
    project_path TEXT NOT NULL,
    runtime_session_id TEXT,
    runtime_source_uri TEXT,
    source_cursor TEXT,
    provenance TEXT NOT NULL DEFAULT '{}',
    resume_capability TEXT NOT NULL DEFAULT '{}',
    status TEXT NOT NULL,
    metadata TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_session_bindings_peer ON session_bindings(peer_id);
CREATE INDEX IF NOT EXISTS idx_session_bindings_runtime ON session_bindings(backend, runtime_session_id);
CREATE INDEX IF NOT EXISTS idx_session_bindings_backend_project ON session_bindings(backend, project_path);
CREATE INDEX IF NOT EXISTS idx_session_bindings_source_uri ON session_bindings(runtime_source_uri);
PRAGMA user_version=12;
`

func newBindingStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seed, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if _, err := seed.Exec(sessionBindingsDDL); err != nil {
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

func sbPtr(v string) *string { return &v }

func TestUpsertObservationCreatesBinding(t *testing.T) {
	s := newBindingStore(t)
	ctx := context.Background()

	b, err := s.UpsertObservation(ctx, Observation{
		PeerID:           sbPtr("peer-1"),
		Backend:          "claude-code",
		ProjectPath:      sbPtr("/work/repo"),
		RuntimeSessionID: sbPtr("rt-abc"),
		SourceCursor:     map[string]any{"offset": float64(10)},
		Provenance:       map[string]any{"src": "hook"},
		Metadata:         map[string]any{"k": "v"},
	})
	if err != nil {
		t.Fatalf("UpsertObservation: %v", err)
	}
	if !strings.HasPrefix(b.RepowireSessionID, "rw-session-") {
		t.Errorf("repowire_session_id = %q, want rw-session- prefix", b.RepowireSessionID)
	}
	if b.PeerID == nil || *b.PeerID != "peer-1" {
		t.Errorf("peer_id = %v", b.PeerID)
	}
	if b.CurrentExecutorPeerID == nil || *b.CurrentExecutorPeerID != "peer-1" {
		t.Errorf("current_executor_peer_id = %v", b.CurrentExecutorPeerID)
	}
	if b.Status != BindingActive {
		t.Errorf("status = %q, want active", b.Status)
	}
	if b.CreatedAt == "" || b.LastSeenAt == "" {
		t.Errorf("timestamps unset: created=%q last_seen=%q", b.CreatedAt, b.LastSeenAt)
	}

	got, err := s.GetSessionBinding(ctx, b.RepowireSessionID)
	if err != nil {
		t.Fatalf("GetSessionBinding: %v", err)
	}
	if got == nil {
		t.Fatal("binding not found")
	}
	if got.RuntimeSessionID == nil || *got.RuntimeSessionID != "rt-abc" {
		t.Errorf("runtime_session_id = %v", got.RuntimeSessionID)
	}
	if got.SourceCursor["offset"] != float64(10) {
		t.Errorf("source_cursor = %v", got.SourceCursor)
	}
	if got.Provenance["src"] != "hook" {
		t.Errorf("provenance = %v", got.Provenance)
	}
	if got.Metadata["k"] != "v" {
		t.Errorf("metadata = %v", got.Metadata)
	}
}

func TestUpsertObservationUpdatesExistingByRuntimeSession(t *testing.T) {
	s := newBindingStore(t)
	ctx := context.Background()

	first, err := s.UpsertObservation(ctx, Observation{
		PeerID:           sbPtr("peer-1"),
		Backend:          "claude-code",
		ProjectPath:      sbPtr("/work/repo"),
		RuntimeSessionID: sbPtr("rt-abc"),
		Metadata:         map[string]any{"a": "1"},
		ObservedAt:       "2026-01-01T00:00:00.000000+00:00",
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	second, err := s.UpsertObservation(ctx, Observation{
		PeerID:           sbPtr("peer-1"),
		Backend:          "claude-code",
		ProjectPath:      sbPtr("/work/repo"),
		RuntimeSessionID: sbPtr("rt-abc"),
		Metadata:         map[string]any{"b": "2"},
		ObservedAt:       "2026-02-02T00:00:00.000000+00:00",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.RepowireSessionID != first.RepowireSessionID {
		t.Errorf("expected same binding id, got %q vs %q", second.RepowireSessionID, first.RepowireSessionID)
	}
	if second.CreatedAt != first.CreatedAt {
		t.Errorf("created_at should be preserved: %q vs %q", second.CreatedAt, first.CreatedAt)
	}
	if second.LastSeenAt != "2026-02-02T00:00:00.000000+00:00" {
		t.Errorf("last_seen_at = %q", second.LastSeenAt)
	}
	if second.Metadata["a"] != "1" || second.Metadata["b"] != "2" {
		t.Errorf("metadata should merge: %v", second.Metadata)
	}

}

func TestGetByRuntimeSessionScoping(t *testing.T) {
	s := newBindingStore(t)
	ctx := context.Background()

	if _, err := s.UpsertObservation(ctx, Observation{
		PeerID: sbPtr("peer-1"), Backend: "claude-code", ProjectPath: sbPtr("/a"), RuntimeSessionID: sbPtr("rt-1"),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := s.GetByRuntimeSession(ctx, "rt-1", nil, nil)
	if err != nil {
		t.Fatalf("GetByRuntimeSession: %v", err)
	}
	if got == nil {
		t.Fatal("expected match for rt-1")
	}
	got, err = s.GetByRuntimeSession(ctx, "rt-1", sbPtr("codex"), nil)
	if err != nil {
		t.Fatalf("GetByRuntimeSession scoped: %v", err)
	}
	if got != nil {
		t.Errorf("backend mismatch should not match, got %+v", got)
	}
	got, err = s.GetByRuntimeSession(ctx, "rt-1", sbPtr("claude-code"), sbPtr("/other"))
	if err != nil {
		t.Fatalf("GetByRuntimeSession project: %v", err)
	}
	if got != nil {
		t.Errorf("project mismatch should not match, got %+v", got)
	}
}

func TestListByPeerAndBackendProjectAndSourceURI(t *testing.T) {
	s := newBindingStore(t)
	ctx := context.Background()

	if _, err := s.UpsertObservation(ctx, Observation{
		PeerID: sbPtr("peer-1"), Backend: "claude-code", ProjectPath: sbPtr("/a"), RuntimeSessionID: sbPtr("rt-1"),
	}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	if _, err := s.UpsertObservation(ctx, Observation{
		PeerID: sbPtr("peer-1"), Backend: "claude-code", ProjectPath: sbPtr("/a"), RuntimeSessionID: sbPtr("rt-2"),
	}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if _, err := s.UpsertObservation(ctx, Observation{
		PeerID: sbPtr("peer-2"), Backend: "codex", ProjectPath: sbPtr("/b"), RuntimeSourceURI: sbPtr("file:///x.jsonl"),
	}); err != nil {
		t.Fatalf("upsert 3: %v", err)
	}

	byPeer, err := s.ListBindingsByPeer(ctx, "peer-1")
	if err != nil {
		t.Fatalf("ListBindingsByPeer: %v", err)
	}
	if len(byPeer) != 2 {
		t.Errorf("peer-1 bindings = %d, want 2", len(byPeer))
	}

	bySrc, err := s.GetBySourceURI(ctx, "file:///x.jsonl")
	if err != nil {
		t.Fatalf("GetBySourceURI: %v", err)
	}
	if bySrc == nil || bySrc.PeerID == nil || *bySrc.PeerID != "peer-2" {
		t.Errorf("GetBySourceURI = %+v", bySrc)
	}
}

func TestGetSessionBindingMissingReturnsNil(t *testing.T) {
	s := newBindingStore(t)
	got, err := s.GetSessionBinding(context.Background(), "nope")
	if err != nil {
		t.Fatalf("GetSessionBinding: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing binding, got %+v", got)
	}
}

func TestGetSessionBindingRejectsCorruptJSON(t *testing.T) {
	s := newBindingStore(t)
	ctx := context.Background()
	binding, err := s.UpsertObservation(ctx, Observation{Backend: "codex", ProjectPath: sbPtr("/work/repo")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE session_bindings SET metadata = '{' WHERE repowire_session_id = ?`, binding.RepowireSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSessionBinding(ctx, binding.RepowireSessionID); err == nil || !strings.Contains(err.Error(), "decode session binding metadata") {
		t.Fatalf("corrupt metadata error = %v", err)
	}
}
