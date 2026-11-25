package hub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

type allLive struct{}

func (allLive) PIDAlive(int) bool { return true }

// The real path: a spawn's message is held while the runtime has not
// registered (it sits at a trust prompt), and the peer's first connect on the
// pane delivers it on that very socket, after the connect's queue replay.
func TestHeldSeedArrivesOnThePeersFirstConnection(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("REPOWIRE_CACHE_DIR", t.TempDir())
	withSeedTiming(t, time.Hour, time.Hour)
	path, pane := t.TempDir(), "%61"

	transport := service.NewWebSocketTransport()
	reg, err := peer.NewRegistry(context.Background(), memStore{}, allLive{}, transport)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHubWithTransport(reg, transport, "")
	tmux := &fakeTmux{evidence: map[string]*service.TmuxPaneEvidence{pane: {PaneID: pane, SessionName: "1", TmuxSession: "1:work", CurrentPath: path}}}
	h.WithSpawn(service.NewSpawnService(tmux, service.NewFileOwnership("test-host", tmux.Probe), nil, nil), reg, nil, "test-host", proto.CircleBoundarySession)
	asks := service.NewAskTracker(0)
	h.WithAskLifecycle(asks, service.NewPeerDelivery(reg, h.Router(), transport, asks, nil), reg)
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	defer srv.Close()

	h.holdSpawnSeed(context.Background(), pane, "worker", proto.AgentClaudeCode, path, "mcp-http", "SEED: review PR 400")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "connect", "display_name": "worker", "circle": "1",
		"backend": "claude-code", "path": path, "pane_id": pane, "agent_pid": 4242}); err != nil {
		t.Fatal(err)
	}
	for {
		var frame map[string]any
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			t.Fatalf("no seed frame on the new connection: %v", err)
		}
		if strings.Contains(fmt.Sprint(frame), "SEED: review PR 400") {
			if id, _ := frame["id"].(string); id != "" {
				_ = wsjson.Write(ctx, conn, map[string]any{"type": "delivery_ack", "id": id, "status": "accepted"})
			}
			return
		}
	}
}

// A follow-up queued while the runtime was registered but not yet connected
// (SessionStart registers before the ws-hook connects; a restart's gap) must
// not overtake the opening message: the seed is the first frame on the socket,
// and the queue replays behind it.
func TestHeldSeedGoesBeforeTheQueuedFollowUp(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("REPOWIRE_CACHE_DIR", t.TempDir())
	withSeedTiming(t, time.Hour, time.Hour)
	ctx := context.Background()
	path, pane, pid := t.TempDir(), "%62", 4243

	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	transport := service.NewWebSocketTransport()
	reg, err := peer.NewRegistry(ctx, memStore{}, allLive{}, transport)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHubWithTransport(reg, transport, "")
	h.store = store
	tmux := &fakeTmux{evidence: map[string]*service.TmuxPaneEvidence{pane: {PaneID: pane, SessionName: "1", TmuxSession: "1:work", CurrentPath: path}}}
	h.WithSpawn(service.NewSpawnService(tmux, service.NewFileOwnership("test-host", tmux.Probe), nil, nil), reg, nil, "test-host", proto.CircleBoundarySession)
	asks := service.NewAskTracker(0)
	delivery := service.NewPeerDelivery(reg, h.Router(), transport, asks, store)
	h.WithReadDeps(asks, store).WithAskLifecycle(asks, delivery, reg)
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	defer srv.Close()

	// Registered (as SessionStart does) but not connected.
	id, _, err := reg.AllocateAndRegister(ctx, peer.AllocateParams{Circle: "1", Backend: proto.AgentClaudeCode, Path: &path,
		Machine: "m", Role: proto.RoleAgent, AgentPID: &pid, PaneID: &pane})
	if err != nil {
		t.Fatal(err)
	}
	h.holdSpawnSeed(ctx, pane, "worker", proto.AgentClaudeCode, path, "mcp-http", "SEED: the brief")
	res, err := delivery.Notify(ctx, service.NotifyParams{FromPeer: "mcp-http", ToPeer: string(id), Text: "FOLLOW-UP: one more thing", BypassCircle: true})
	if err != nil || !res.Queued() {
		t.Fatalf("follow-up = %+v, %v; want it queued", res, err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := wsjson.Write(dialCtx, conn, map[string]any{"type": "connect", "display_name": "worker", "circle": "1",
		"backend": "claude-code", "path": path, "pane_id": pane, "agent_pid": pid, "peer_id": string(id)}); err != nil {
		t.Fatal(err)
	}
	var order []string
	for len(order) < 2 {
		var frame map[string]any
		if err := wsjson.Read(dialCtx, conn, &frame); err != nil {
			t.Fatalf("frames so far %v: %v", order, err)
		}
		text := fmt.Sprint(frame["text"])
		switch {
		case strings.Contains(text, "SEED:"):
			order = append(order, "seed")
			if did, _ := frame["delivery_id"].(string); did != "" {
				_ = wsjson.Write(dialCtx, conn, map[string]any{"type": "delivery_ack", "delivery_id": did, "status": "accepted"})
			}
		case strings.Contains(text, "FOLLOW-UP:"):
			order = append(order, "follow-up")
		}
	}
	if order[0] != "seed" {
		t.Fatalf("frame order = %v; the opening message must come first", order)
	}
}
