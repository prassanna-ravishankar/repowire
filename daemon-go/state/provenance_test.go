package state

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// A mapping row written before migration 14 hydrates as unknown/addressable,
// and a classified row round-trips through the JSON column.
func TestMappingProvenanceMigratesToUnknownAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	s, err := NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO peer_session_mappings(session_id, display_name, circle, backend, role, description) VALUES ('repow-1-old', 'old', '1', 'codex', 'agent', '')`); err != nil {
		t.Fatal(err)
	}
	child := &proto.SessionMapping{SessionID: "repow-1-child", DisplayName: "child", Circle: "1", Backend: "codex", Role: "agent",
		Provenance: proto.Provenance{Source: proto.SourceCodexAppServer, ParentRuntimeID: "parent-thread", Ephemeral: true, Addressable: false, AddressableReason: "subagent_direct_input_denied"}}
	if err := s.UpsertMapping(ctx, child); err != nil {
		t.Fatal(err)
	}
	rows, err := s.LoadMappings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[proto.PeerID]*proto.SessionMapping{}
	for _, m := range rows {
		byID[m.SessionID] = m
	}
	if got := byID["repow-1-old"].Provenance; got != proto.DefaultProvenance() {
		t.Fatalf("legacy row provenance = %+v, want unknown/addressable", got)
	}
	if got := byID["repow-1-child"].Provenance; got != child.Provenance {
		t.Fatalf("round trip = %+v, want %+v", got, child.Provenance)
	}
}
