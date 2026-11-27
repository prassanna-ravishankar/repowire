package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Migration 20 resolves the names existing schedules were made with to the
// peer_ids they address: a known peer_id stays, a name held by exactly one
// known peer (in the schedule's circle, when set) resolves, and an ambiguous
// or unknown name is left to address by name.
func TestMigrationResolvesScheduleEndsToPeerIDs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := formatTS(time.Now())
	for _, m := range [][3]string{
		{"repow-dev-orch", "orchestrator-claude-code", "dev"},
		{"repow-dev-alpha", "alpha", "dev"},
		{"repow-dev-twin1", "twin", "dev"},
		{"repow-dev-twin2", "twin", "dev"},
		{"repow-ops-alpha", "alpha", "ops"},
	} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO peer_session_mappings(session_id, display_name, circle, backend, role, updated_at) VALUES (?, ?, ?, 'claude-code', 'agent', ?)`, m[0], m[1], m[2], now); err != nil {
			t.Fatal(err)
		}
	}
	legacy := func(id, from, to string, circle any) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schedules(schedule_id, from_peer, to_peer, text, kind, circle, fire_at, created_at, updated_at) VALUES (?, ?, ?, 't', 'notify', ?, ?, ?, ?)`, id, from, to, circle, now, now, now); err != nil {
			t.Fatal(err)
		}
	}
	legacy("by-name", "alpha", "orchestrator-claude-code", "dev")
	legacy("by-id", "telegram", "repow-dev-orch", nil)
	legacy("ambiguous", "orchestrator-claude-code", "twin", nil)
	legacy("unscoped-alpha", "orchestrator-claude-code", "alpha", nil)
	if _, err := s.db.ExecContext(ctx, `PRAGMA user_version=19`); err != nil { // a database from before migration 20
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = NewStore(path) // opening an older database migrates it
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := map[string][2]string{ // schedule -> {from_peer_id, to_peer_id}
		"by-name":        {"repow-dev-alpha", "repow-dev-orch"},
		"by-id":          {"", "repow-dev-orch"},
		"ambiguous":      {"repow-dev-orch", ""},
		"unscoped-alpha": {"repow-dev-orch", ""}, // alpha is in two circles
	}
	for id, ends := range want {
		got, err := s.GetSchedule(ctx, id)
		if err != nil || got == nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got.FromPeerID != ends[0] || got.ToPeerID != ends[1] {
			t.Errorf("%s: ends = %q -> %q, want %q -> %q", id, got.FromPeerID, got.ToPeerID, ends[0], ends[1])
		}
	}
}
