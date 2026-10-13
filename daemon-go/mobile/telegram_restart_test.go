package mobile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/repowire/repowire/daemon-go/state"
)

// TestTelegramRestartHorizonUsesDeliveryOrder (final review of #478): pushes
// run concurrently, so the item created later can be shown first. After a
// restart the tap horizon is the highest message Telegram showed (101), not
// the ref of the newest-created item (100). Real SQLite behind the daemon.
func TestTelegramRestartHorizonUsesDeliveryOrder(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, v := range []struct{ id, created, ref string }{
		{"older", "2026-10-13T10:00:00.000Z", "101"},
		{"newer", "2026-10-13T10:00:01.000Z", "100"},
	} {
		item := &state.InboxItem{ID: v.id, Kind: "notify", FromName: "a", Text: "shown", CreatedAt: v.created}
		if err := store.InsertInboxItem(ctx, item); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordInboxPush(ctx, item.ID, state.InboxPush{Surface: "telegram", Status: state.PushPushed, SurfaceRef: v.ref}); err != nil {
			t.Fatal(err)
		}
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("surface") != "telegram" || r.URL.Query().Get("limit") != "1" {
			t.Errorf("seed query = %s", r.URL.RawQuery)
		}
		items, err := store.ListInbox(ctx, state.InboxFilter{Surface: "telegram", Limit: 1})
		if err != nil {
			t.Error(err)
		}
		out := []map[string]any{}
		for _, item := range items {
			pushes := []map[string]any{}
			for _, push := range item.Pushes {
				pushes = append(pushes, map[string]any{"surface": push.Surface, "status": push.Status, "surface_ref": push.SurfaceRef})
			}
			out = append(out, map[string]any{"id": item.ID, "pushes": pushes})
		}
		writeJSONBody(w, map[string]any{"items": out})
	}))
	t.Cleanup(daemon.Close)
	bot, _ := newSurfaceBot(t, "http://127.0.0.1:1", daemon.URL)
	if got := bot.tapHorizon(ctx, 100); got != 101 {
		t.Fatalf("after restart horizon = %d, want the highest shown ref 101", got)
	}
}

func writeJSONBody(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
