package hub

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/proto"
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
