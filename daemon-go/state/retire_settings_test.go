package state

import (
	"context"
	"testing"
	"time"
)

// A hard retirement closes the id for good, so the peer's own settings go
// with it; a soft one may still be reclaimed, so they stay (#605).
func TestHardRetireDropsThePeersSettings(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()
	for _, id := range []string{"peer-closed", "peer-soft", "peer-live"} {
		if err := s.SetPeerWireVisibility(ctx, id, "private"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetPeerMessageDelivery(ctx, id, "pull"); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if err := s.Retire(ctx, "peer-soft", now, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Retire(ctx, "peer-closed", now, true); err != nil {
		t.Fatal(err)
	}
	vis, err := s.PeerWireVisibilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	modes, err := s.PeerMessageDeliveries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]map[string]string{"visibility": vis, "delivery": modes} {
		if _, ok := got["peer-closed"]; ok || len(got) != 2 || got["peer-soft"] == "" || got["peer-live"] == "" {
			t.Fatalf("%s after retirements: %v", name, got)
		}
	}
}

// Every table declared closedWithPeer is keyed by peer_id, or Retire's
// delete would fail on it.
func TestClosedWithPeerTablesHaveAPeerID(t *testing.T) {
	s := newTempStore(t)
	n := 0
	for _, tr := range tableRetentions {
		if !tr.closedWithPeer {
			continue
		}
		for _, table := range tr.tables {
			n++
			var found int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'peer_id'`, table).Scan(&found); err != nil || found != 1 {
				t.Fatalf("%s: peer_id column %d %v", table, found, err)
			}
		}
	}
	if n == 0 {
		t.Fatal("no table is declared closedWithPeer")
	}
}
