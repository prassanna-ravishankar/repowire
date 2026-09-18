package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/proto"
)

// Native clients (OpenCode, Pi) connect over WebSocket without hook_version.
// Their real connect frame carries explicit provenance and lands as
// source=hook; the same frame without it stays unknown (a pane-less frame,
// and the hub would not infer from placement either), and an invalid source
// is rejected.
func TestWSConnectFrameProvenance(t *testing.T) {
	h := newTestHub(t)
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	defer srv.Close()
	wsURL := "ws" + srv.URL[len("http"):]
	path := "/work/app"

	connect := func(frame map[string]any) (proto.PeerID, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.CloseNow()
		if err := wsjson.Write(ctx, c, frame); err != nil {
			t.Fatalf("write connect: %v", err)
		}
		var reply map[string]any
		if err := wsjson.Read(ctx, c, &reply); err != nil {
			t.Fatalf("read reply: %v", err)
		}
		id, _ := reply["session_id"].(string)
		errText, _ := reply["error"].(string)
		return proto.PeerID(id), errText
	}
	// Mirrors cli/assets/opencode.ts connectMsg.
	opencode := map[string]any{
		"type": "connect", "display_name": "app-opencode", "circle": "c", "circle_source": "tmux",
		"backend": "opencode", "path": path, "agent_pid": 4242,
		"provenance": map[string]any{"source": "hook", "addressable": true},
	}
	id, errText := connect(opencode)
	if errText != "" || id == "" {
		t.Fatalf("opencode connect: id=%q err=%q", id, errText)
	}
	if p, _ := h.reg.GetPeer(id); p.Source != proto.SourceHook || !p.Addressable {
		t.Fatalf("opencode provenance = %+v", p.Provenance)
	}

	legacy := map[string]any{
		"type": "connect", "display_name": "app-pi", "circle": "c", "circle_source": "tmux",
		"backend": "pi", "path": path, "agent_pid": 4243,
	}
	id, errText = connect(legacy)
	if errText != "" || id == "" {
		t.Fatalf("legacy connect: id=%q err=%q", id, errText)
	}
	if p, _ := h.reg.GetPeer(id); p.Source != proto.SourceUnknown || !p.Addressable {
		t.Fatalf("legacy frame without evidence = %+v, want unknown/addressable", p.Provenance)
	}

	bad := map[string]any{
		"type": "connect", "display_name": "app-pi", "circle": "c", "backend": "pi", "path": path,
		"provenance": map[string]any{"source": "desktop", "addressable": true},
	}
	if _, errText = connect(bad); errText == "" {
		t.Fatal("invalid provenance.source was accepted over WebSocket")
	}
}
