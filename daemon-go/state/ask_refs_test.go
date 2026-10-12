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

// The history scan must not stall the rest of the store: with it paused
// mid-iteration, a write on the store's own connection still completes.
func TestAskRefsScanDoesNotBlockStoreWrites(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for _, cid := range []string{"ask-1", "ask-2", "ask-3"} {
		if err := s.AppendEvent(ctx, peer.Event{Type: "ask", Timestamp: at, Payload: map[string]any{"correlation_id": cid, "from_peer_id": "a", "to_peer_id": "b"}}); err != nil {
			t.Fatal(err)
		}
	}
	atRow, release := make(chan struct{}), make(chan struct{})
	scanDone := make(chan error, 1)
	go func() {
		first := true
		scanDone <- s.askRefs(ctx, func(AskRef) error {
			if first {
				first = false
				close(atRow)
				<-release // hold the scan open mid-iteration
			}
			return nil
		})
	}()
	<-atRow
	wrote := make(chan time.Duration, 1)
	go func() {
		began := time.Now()
		_ = s.AppendEvent(ctx, peer.Event{Type: "chat_turn", Timestamp: at, PeerID: "p", Payload: map[string]any{"role": "assistant"}})
		wrote <- time.Since(began)
	}()
	select {
	case d := <-wrote:
		if d > time.Second {
			t.Fatalf("write took %s beside a running scan", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a store write waited on the running AskRefs scan")
	}
	close(release)
	if err := <-scanDone; err != nil {
		t.Fatal(err)
	}
}
