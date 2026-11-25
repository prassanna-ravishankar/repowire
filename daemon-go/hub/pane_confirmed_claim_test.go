package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// The 2026-11-25 rollout: the pane's hook was started as repow-1-e9cb2c1d and
// still claimed it, while the pane metadata it rewrites on every connect named
// the identity it last confirmed for the same agent pid.
func TestPaneBackedConnectClaimsTheConfirmedIdentity(t *testing.T) {
	t.Setenv("REPOWIRE_CACHE_DIR", t.TempDir())
	pid := os.Getpid()
	raw, _ := json.Marshal(map[string]any{"peer_id": "repow-1-05ee4cfe", "agent_pid": pid})
	if err := os.WriteFile(hooks.WSHookMetaPath("%29"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	pane, stale := "%29", proto.PeerID("repow-1-e9cb2c1d")
	other := pid + 1

	if got := paneConfirmedClaim(&pane, &pid, &stale); got == nil || *got != "repow-1-05ee4cfe" {
		t.Fatalf("claim = %v; want the pane's confirmed identity", got)
	}
	if got := paneConfirmedClaim(&pane, &pid, nil); got == nil || *got != "repow-1-05ee4cfe" {
		t.Fatalf("claimless connect = %v; want the pane's confirmed identity", got)
	}
	if got := paneConfirmedClaim(&pane, &other, &stale); got != &stale {
		t.Fatalf("another process = %v; want the frame's own claim", got)
	}
	if got := paneConfirmedClaim(nil, &pid, &stale); got != &stale {
		t.Fatalf("pane-less = %v; want the frame's own claim", got)
	}
}

// mappedStore is a registry store that starts with durable mappings, as after
// a daemon restart.
type mappedStore struct {
	memStore
	mappings []*proto.SessionMapping
}

func (s mappedStore) LoadMappings(context.Context) ([]*proto.SessionMapping, error) {
	return s.mappings, nil
}

type liveSet map[int]bool

func (l liveSet) PIDAlive(pid int) bool { return l[pid] }

// End to end through HandleWS after a restart: the hook's frame still claims
// the id it was started with, the pane metadata names the identity it last
// confirmed for the same agent, and the connect lands on that identity rather
// than on the mapping that holds the folder name.
func TestRestartedDaemonReconnectsAPaneUnderItsLatestIdentity(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("REPOWIRE_CACHE_DIR", t.TempDir())
	path, pane, pid := t.TempDir(), "%29", os.Getpid()
	mapping := func(id, name string, agentPID int) *proto.SessionMapping {
		return &proto.SessionMapping{SessionID: proto.PeerID(id), DisplayName: proto.DisplayName(name), Circle: "1",
			Backend: proto.AgentClaudeCode, Path: &path, Role: proto.RoleAgent, UpdatedAt: time.Now().UTC(), AgentPID: &agentPID}
	}
	store := mappedStore{mappings: []*proto.SessionMapping{
		mapping("repow-1-01abdd10", baseName(path)+"-claude-code", 500),
		mapping("repow-1-05ee4cfe", baseName(path)+"-2-claude-code", pid),
	}}

	for _, tc := range []struct {
		name string
		meta map[string]any
		want proto.PeerID
	}{
		{"metadata names the latest identity", map[string]any{"peer_id": "repow-1-05ee4cfe", "agent_pid": pid}, "repow-1-05ee4cfe"},
		{"without metadata the stale claim falls to name reclaim", nil, "repow-1-01abdd10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(hooks.WSHookMetaPath(pane))
			if tc.meta != nil {
				raw, _ := json.Marshal(tc.meta)
				if err := os.WriteFile(hooks.WSHookMetaPath(pane), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			transport := service.NewWebSocketTransport()
			reg, err := peer.NewRegistry(context.Background(), store, liveSet{pid: true}, transport)
			if err != nil {
				t.Fatal(err)
			}
			h := NewHubWithTransport(reg, transport, "")
			tmux := &fakeTmux{evidence: map[string]*service.TmuxPaneEvidence{pane: {PaneID: pane, SessionName: "1", TmuxSession: "1:work", CurrentPath: path}}}
			h.WithSpawn(service.NewSpawnService(tmux, service.NewFileOwnership("test-host", tmux.Probe), nil, nil), reg, nil, "test-host", proto.CircleBoundarySession)
			srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if err := wsjson.Write(ctx, conn, map[string]any{"type": "connect", "display_name": "repowire-4-claude-code", "circle": "1",
				"backend": "claude-code", "path": path, "pane_id": pane, "peer_id": "repow-1-e9cb2c1d", "agent_pid": pid}); err != nil {
				t.Fatal(err)
			}
			var resp map[string]any
			if err := wsjson.Read(ctx, conn, &resp); err != nil {
				t.Fatal(err)
			}
			if resp["type"] != "connected" || proto.PeerID(fmt.Sprint(resp["session_id"])) != tc.want {
				t.Fatalf("connect = %v; want %s", resp, tc.want)
			}
		})
	}
}

func baseName(path string) string { return filepath.Base(path) }
