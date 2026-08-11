package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
)

func newTempStore(t *testing.T) *Store {
	t.Helper()
	// NewStore now owns migrations, so a fresh path bootstraps the full schema.
	s, err := NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestNewStoreUpgradesFromZero verifies a legacy user_version=0 DB (with
// unrelated pre-existing content) is migrated up to schemaVersion rather than
// refused — the daemon now owns migrations end to end.
func TestNewStoreUpgradesFromZero(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")
	seed, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// user_version defaults to 0; leave unrelated content to prove it survives.
	if _, err := seed.Exec(`CREATE TABLE x(a)`); err != nil {
		t.Fatalf("exec: %v", err)
	}
	_ = seed.Close()

	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("expected upgrade-from-zero to succeed, got %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", version, SchemaVersion)
	}
}

func TestAppendAndReadEvent(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	ts := time.Date(2026, 6, 29, 12, 34, 56, 789_000_000, time.UTC)
	ev := peer.Event{
		Type:      "peer_online",
		Timestamp: ts,
		PeerID:    proto.PeerID("peer-abc"),
		PeerName:  proto.DisplayName("alice"),
		SessionID: proto.PeerID("peer-abc"),
		Payload:   map[string]any{"reason": "reconnect"},
	}
	if err := s.AppendEvent(ctx, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	var (
		eventID   string
		typ       string
		timestamp string
		peerID    sql.NullString
		peerName  sql.NullString
		sessionID sql.NullString
		turnID    sql.NullString
		payload   string
	)
	row := s.db.QueryRowContext(ctx,
		`SELECT event_id, type, timestamp, peer_id, peer_name, session_id, turn_id, payload_json FROM events`)
	if err := row.Scan(&eventID, &typ, &timestamp, &peerID, &peerName, &sessionID, &turnID, &payload); err != nil {
		t.Fatalf("read back event: %v", err)
	}
	if eventID == "" {
		t.Error("event_id should have been generated")
	}
	if typ != "peer_online" {
		t.Errorf("type = %q, want peer_online", typ)
	}
	if timestamp != "2026-06-29T12:34:56.789Z" {
		t.Errorf("timestamp = %q, want 2026-06-29T12:34:56.789Z", timestamp)
	}
	if peerID.String != "peer-abc" {
		t.Errorf("peer_id = %q, want peer-abc", peerID.String)
	}
	if turnID.Valid {
		t.Errorf("turn_id should be NULL, got %q", turnID.String)
	}
	if payload != `{"reason":"reconnect"}` {
		t.Errorf("payload_json = %q", payload)
	}
	events, err := s.LoadRecentEvents(ctx, 10)
	if err != nil || len(events) != 1 || events[0]["id"] != eventID || events[0]["reason"] != "reconnect" {
		t.Fatalf("LoadRecentEvents = %#v, %v", events, err)
	}
}

func TestAppendEventEmptyPeerIDStoresNull(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	if err := s.AppendEvent(ctx, peer.Event{Type: "system"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	var peerID sql.NullString
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT peer_id, payload_json FROM events`).Scan(&peerID, &payload); err != nil {
		t.Fatalf("read: %v", err)
	}
	if peerID.Valid {
		t.Errorf("empty peer_id should be NULL, got %q", peerID.String)
	}
	if payload != "{}" {
		t.Errorf("nil payload should default to {}, got %q", payload)
	}
}

func TestMappingRoundTrip(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	path := "/work/repo"
	model := "opus"
	pid := 4242
	want := &proto.SessionMapping{
		SessionID:   proto.PeerID("peer-1"),
		DisplayName: proto.DisplayName("bob"),
		Circle:      "default",
		Backend:     proto.AgentType("claude-code"),
		Path:        &path,
		Role:        proto.PeerRole("agent"),
		UpdatedAt:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Description: "a worker",
		Model:       &model,
		AgentPID:    &pid,
	}
	if err := s.UpsertMapping(ctx, want); err != nil {
		t.Fatalf("UpsertMapping: %v", err)
	}

	// A second mapping with NULL path/model/agent_pid.
	bare := &proto.SessionMapping{
		SessionID:   proto.PeerID("peer-2"),
		DisplayName: proto.DisplayName("carol"),
		Circle:      "default",
		Backend:     proto.AgentType("codex"),
		Role:        proto.PeerRole("agent"),
		UpdatedAt:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	if err := s.UpsertMapping(ctx, bare); err != nil {
		t.Fatalf("UpsertMapping bare: %v", err)
	}

	got, err := s.LoadMappings(ctx)
	if err != nil {
		t.Fatalf("LoadMappings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d mappings, want 2", len(got))
	}

	byID := map[proto.PeerID]*proto.SessionMapping{}
	for _, m := range got {
		byID[m.SessionID] = m
	}

	m1 := byID["peer-1"]
	if m1 == nil {
		t.Fatal("peer-1 missing")
	}
	if m1.DisplayName != "bob" || m1.Backend != "claude-code" || m1.Role != "agent" {
		t.Errorf("peer-1 fields wrong: %+v", m1)
	}
	if m1.Path == nil || *m1.Path != path {
		t.Errorf("peer-1 path = %v, want %q", m1.Path, path)
	}
	if m1.Model == nil || *m1.Model != model {
		t.Errorf("peer-1 model = %v", m1.Model)
	}
	if m1.AgentPID == nil || *m1.AgentPID != pid {
		t.Errorf("peer-1 agent_pid = %v", m1.AgentPID)
	}
	if !m1.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("peer-1 updated_at = %v, want %v", m1.UpdatedAt, want.UpdatedAt)
	}

	m2 := byID["peer-2"]
	if m2 == nil {
		t.Fatal("peer-2 missing")
	}
	if m2.Path != nil || m2.Model != nil || m2.AgentPID != nil {
		t.Errorf("peer-2 nullable fields should be nil: %+v", m2)
	}

	// Delete peer-1 and confirm only peer-2 remains.
	if err := s.DeleteMapping(ctx, "peer-1"); err != nil {
		t.Fatalf("DeleteMapping: %v", err)
	}
	got, err = s.LoadMappings(ctx)
	if err != nil {
		t.Fatalf("LoadMappings after delete: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != "peer-2" {
		t.Errorf("after delete, got %+v", got)
	}
}

func TestRetireLoadCutoffAndUnretire(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Retire(ctx, "peer-old", old, false); err != nil {
		t.Fatalf("Retire old: %v", err)
	}
	if err := s.Retire(ctx, "peer-recent", recent, true); err != nil {
		t.Fatalf("Retire recent: %v", err)
	}

	cutoff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	got, err := s.LoadRetired(ctx, cutoff)
	if err != nil {
		t.Fatalf("LoadRetired: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d retired, want 1 (cutoff filters old)", len(got))
	}
	if rt, ok := got["peer-recent"]; !ok || !rt.At.Equal(recent) || !rt.Hard {
		t.Errorf("peer-recent missing or wrong time: %v %v", ok, rt)
	}
	if _, ok := got["peer-old"]; ok {
		t.Error("peer-old should be filtered by cutoff")
	}

	if err := s.Unretire(ctx, "peer-recent"); err != nil {
		t.Fatalf("Unretire: %v", err)
	}
	got, err = s.LoadRetired(ctx, old)
	if err != nil {
		t.Fatalf("LoadRetired after unretire: %v", err)
	}
	if _, ok := got["peer-recent"]; ok {
		t.Error("peer-recent should be gone after Unretire")
	}
	if _, ok := got["peer-old"]; !ok {
		t.Error("peer-old should remain")
	}
}
