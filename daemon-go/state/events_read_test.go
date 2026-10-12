package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/peer"
)

func TestEventsOfTypesFiltersSortsAndStripsText(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for _, e := range []peer.Event{
		{Type: "chat_turn", Timestamp: base.Add(2 * time.Minute), PeerID: "p", Payload: map[string]any{"role": "assistant", "text": "long words", "tool_calls": []any{"x"}}},
		{Type: "ask", Timestamp: base.Add(time.Minute), Payload: map[string]any{"text": "keep me", "to": "q"}},
		{Type: "peer_status", Timestamp: base, PeerID: "p", Payload: map[string]any{"status": "online"}},
	} {
		if err := s.AppendEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.EventsOfTypes(ctx, "ask", "chat_turn")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Type != "ask" || rows[1].Type != "chat_turn" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Payload["text"] != "keep me" {
		t.Errorf("ask text dropped: %v", rows[0].Payload)
	}
	if _, ok := rows[1].Payload["text"]; ok || rows[1].Payload["role"] != "assistant" || rows[1].PeerID != "p" {
		t.Errorf("chat turn not stripped: %+v", rows[1])
	}
}
