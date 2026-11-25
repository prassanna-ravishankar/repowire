package hub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
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
