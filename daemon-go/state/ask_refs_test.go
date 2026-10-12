package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/peer"
)

func TestAskRefsMergesJournalAndTracesOncePerCorrelationID(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	ask := func(cid, from, to string) peer.Event {
		return peer.Event{Type: "ask", Timestamp: at, Payload: map[string]any{"correlation_id": cid, "from_peer_id": from, "to_peer_id": to}}
	}
	for _, e := range []peer.Event{ask("ask-old", "a", "b"), ask("ask-both", "a", "c"), {Type: "chat_turn", Timestamp: at, Payload: map[string]any{"correlation_id": "ask-noise"}}} {
		if err := s.AppendEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// A trace for an ask also in the journal, one only in the ledger, and a notify (not an ask).
	for _, tr := range [][]string{{"ask-both", "ask", "created", "a", "c"}, {"ask-new", "ask", "created", "d", "b"}, {"ask-new", "ask", "routed", "d", "b"}, {"notif-1", "notify", "created", "d", "b"}} {
		if err := s.RecordTrace(ctx, tr[0], tr[1], tr[2], "ok", tr[0], tr[4], tr[3], nil); err != nil {
			t.Fatal(err)
		}
	}
	refs, err := s.AskRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]AskRef{}
	for _, r := range refs {
		got[r.CorrelationID] = r
	}
	if len(refs) != 3 || got["ask-old"].To != "b" || got["ask-both"].To != "c" || got["ask-new"] != (AskRef{"ask-new", "d", "b"}) {
		t.Fatalf("refs = %+v", refs)
	}
}
