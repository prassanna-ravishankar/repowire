package hooks

import "testing"

func TestReconnectClaimsTheConfirmedIdentity(t *testing.T) {
	id, name := confirmedIdentity("repow-1-e9cb2c1d", "repowire-4-claude-code",
		map[string]any{"type": "connected", "session_id": "repow-1-05ee4cfe", "display_name": "repowire-2-claude-code"})
	if id != "repow-1-05ee4cfe" || name != "repowire-2-claude-code" {
		t.Fatalf("next claim = %s %s; want the confirmed identity", id, name)
	}
	if id, name := confirmedIdentity("repow-1-a", "n", map[string]any{"type": "connected"}); id != "repow-1-a" || name != "n" {
		t.Fatalf("a frame without identity changed the claim to %s %s", id, name)
	}
}
