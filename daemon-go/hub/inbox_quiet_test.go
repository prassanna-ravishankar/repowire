package hub

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
)

// On the real stack (#476): a notify in Telegram's quiet hours reaches the
// bridge as a surface_hold, not an item, and the bridge's surface_release
// after the window brings the item; one sent before the window's end does not.
func TestQuietHoursOverTheWire(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	var mu sync.Mutex
	now := time.Date(2026, 11, 25, 23, 0, 0, 0, london)
	s := newInboxStackWith(t, config.HumanConfig{Surfaces: map[string]config.HumanSurfaceConfig{
		"telegram": {QuietHours: &config.QuietHours{Start: "22:00", End: "07:00", Timezone: "Europe/London"}},
	}})
	s.inbox.WithClock(func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	bridge := s.connect(t, map[string]any{"display_name": "telegram", "circle": "default", "backend": "claude-code",
		"role": "service", "path": "/telegram", "capabilities": []string{proto.CapInboxSurface, proto.CapInboxHold}}, telegramSurface)
	agent := s.connect(t, map[string]any{"display_name": "alpha", "circle": "1", "backend": "claude-code", "role": "agent", "path": "/work/alpha"}, nil)

	decodeBody(t, postJSON(t, s.srv.URL+"/notify", map[string]any{"from_peer": string(agent.id), "to_peer": "prass", "text": "the nightly build passed"}))
	hold := bridge.waitFrame(ofType("surface_hold"))
	if hold["release_at"] != time.Date(2026, 11, 26, 7, 0, 0, 0, london).UTC().Format(time.RFC3339) {
		t.Fatalf("surface_hold = %+v", hold)
	}
	send := func() {
		if err := wsjson.Write(t.Context(), bridge.conn, map[string]any{"type": "surface_release"}); err != nil {
			t.Fatal(err)
		}
	}
	send() // early: the daemon checks its own clock and announces the hold again
	deadline := time.Now().Add(3 * time.Second)
	holds := 1
	for holds < 2 && time.Now().Before(deadline) {
		holds = 0
		bridge.mu.Lock()
		for _, f := range bridge.frames {
			if f["type"] == "inbox_item" {
				bridge.mu.Unlock()
				t.Fatal("an early surface_release pushed the item")
			}
			if f["type"] == "surface_hold" {
				holds++
			}
		}
		bridge.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	if holds != 2 {
		t.Fatalf("an early surface_release must announce the hold again; holds = %d", holds)
	}

	mu.Lock()
	now = time.Date(2026, 11, 26, 7, 0, 5, 0, london)
	mu.Unlock()
	send()
	pushed := bridge.waitFrame(ofType("inbox_item"))
	if item, _ := pushed["item"].(map[string]any); item["text"] != "the nightly build passed" {
		t.Fatalf("released %+v", pushed)
	}
	// GET /inbox reads presence fresh: the window has ended.
	resp, err := http.Get(s.srv.URL + "/inbox")
	if err != nil {
		t.Fatal(err)
	}
	listed := decodeBody(t, resp)
	surfaces, _ := listed["surfaces"].(map[string]any)
	if tg, _ := surfaces["telegram"].(map[string]any); tg["quiet"] != false {
		t.Fatalf("telegram presence after the window = %v", tg)
	}
}
