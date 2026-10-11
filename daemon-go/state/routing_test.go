package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
)

func TestDescriptionSetAtMigratesAndRoundTrips(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v14.db")
	seed, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`CREATE TABLE peer_session_mappings (
		session_id TEXT PRIMARY KEY, display_name TEXT NOT NULL, circle TEXT NOT NULL, backend TEXT NOT NULL,
		path TEXT, role TEXT NOT NULL, updated_at TEXT, description TEXT NOT NULL DEFAULT '', model TEXT,
		agent_pid INTEGER, provenance TEXT NOT NULL DEFAULT '{}');
		INSERT INTO peer_session_mappings(session_id, display_name, circle, backend, role, description)
		VALUES ('repow-old', 'old', 'c', 'codex', 'agent', 'legacy');
		PRAGMA user_version=14;`); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()

	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("migrate v14: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	mappings, err := s.LoadMappings(ctx)
	if err != nil || len(mappings) != 1 || mappings[0].DescriptionSetAt != nil {
		t.Fatalf("legacy row = %+v, %v", mappings, err)
	}

	setAt := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	m := mappings[0]
	m.DescriptionSetAt = &setAt
	if err := s.UpsertMapping(ctx, m); err != nil {
		t.Fatal(err)
	}
	mappings, _ = s.LoadMappings(ctx)
	if mappings[0].DescriptionSetAt == nil || !mappings[0].DescriptionSetAt.Equal(setAt) {
		t.Fatalf("round trip = %v", mappings[0].DescriptionSetAt)
	}
}

func TestRoutingLedgerQueries(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-time.Minute)
	for _, stage := range []string{"route_decision", "created", "closed"} {
		if err := s.RecordTrace(ctx, "ask-1", "ask", stage, "", "", "repow-b", "repow-a", map[string]any{"stage": stage}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.TracesWithStage(ctx, "route_decision", start)
	if err != nil || len(rows) != 1 || rows[0].TraceID != "ask-1" || rows[0].Detail["stage"] != "route_decision" {
		t.Fatalf("decisions = %+v, %v", rows, err)
	}

	for i, role := range []string{"assistant", "user", "assistant"} {
		ev := peer.Event{Type: "chat_turn", Timestamp: start.Add(time.Duration(i+1) * time.Second), PeerID: proto.PeerID("repow-b"), Payload: map[string]any{"role": role}}
		if err := s.AppendEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	other := peer.Event{Type: "chat_turn", Timestamp: start.Add(2 * time.Second), PeerID: proto.PeerID("repow-c"), Payload: map[string]any{"role": "assistant"}}
	_ = s.AppendEvent(ctx, other)
	n, err := s.AssistantTurnsBetween(ctx, "repow-b", start, start.Add(10*time.Second))
	if err != nil || n != 2 {
		t.Fatalf("turns = %d, %v", n, err)
	}
	if n, _ := s.AssistantTurnsBetween(ctx, "repow-b", start.Add(2*time.Second), start.Add(10*time.Second)); n != 1 {
		t.Fatalf("window must be (from, to], got %d", n)
	}
}
