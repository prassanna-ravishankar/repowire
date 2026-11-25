package peer

import (
	"context"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// The residual identity matrix (#443). Already covered elsewhere:
//   - duplicate persisted mapping tuples fail closed:
//     TestSessionIdentity_AmbiguousTupleMints (live, and from mappings after a
//     restart) and TestRegistrationDecisionAmbiguousCandidates;
//   - Codex certificate-bound reconnects: codexbridge
//     TestRegisterRestoresThreadIdentityFromBirthCertificate and
//     TestRegisterRetiredClaimFallsBackToBirthCertificate, hub
//     TestValidatedRuntimeCertificateRecoversOnlySoftRetirement;
//   - Codex sibling threads in one folder: TestSessionIdentity_CodexSibling*.
// These close the rest.

// Two Claude sessions in one folder stay two peers across a daemon restart,
// whichever reconnects first and however: by its session (a SessionStart
// hook) or by claiming its peer_id (a ws-hook or channel reconnect).
func TestMatrixSamePathPeersStayDistinctAcrossRestart(t *testing.T) {
	for _, how := range []string{"session", "claim"} {
		h := newSessionHarness(t)
		a, aName := h.register(proto.AgentClaudeCode, "/p/app", "sess-a", 100)
		b, bName := h.register(proto.AgentClaudeCode, "/p/app", "sess-b", 200)
		if a == b || aName == bName {
			t.Fatalf("%s: one folder, one identity: %s/%s %s/%s", how, a, aName, b, bName)
		}
		h.offline(a, 100)
		h.offline(b, 200)
		h.restart()

		back := func(id proto.PeerID, session string, pid int) (proto.PeerID, proto.DisplayName) {
			if how == "session" {
				return h.register(proto.AgentClaudeCode, "/p/app", session, pid)
			}
			h.live.alive[pid] = true
			got, name, err := h.r.AllocateAndRegister(context.Background(), AllocateParams{Circle: "work", Backend: proto.AgentClaudeCode,
				Path: ptr("/p/app"), Machine: "m", Role: proto.RoleAgent, ClaimedPeerID: &id, AgentPID: &pid})
			if err != nil {
				t.Fatal(err)
			}
			return got, name
		}
		// The second session comes back first, in new processes.
		if got, name := back(b, "sess-b", 201); got != b || name != bName {
			t.Fatalf("%s: b came back as %s/%s, want %s/%s", how, got, name, b, bName)
		}
		if got, name := back(a, "sess-a", 101); got != a || name != aName {
			t.Fatalf("%s: a came back as %s/%s, want %s/%s", how, got, name, a, aName)
		}
	}
}

// A runtime session id that rotates in a live process (Claude's /clear) stays
// on that process's peer, and the new binding points there, never at the
// sibling in the same folder; after a restart each session finds its own
// peer, the rotated one included.
func TestMatrixSessionRotationDoesNotCrossBind(t *testing.T) {
	h := newSessionHarness(t)
	a, _ := h.register(proto.AgentClaudeCode, "/p/app", "sess-a1", 100)
	b, _ := h.register(proto.AgentClaudeCode, "/p/app", "sess-b", 200)

	if got, _ := h.register(proto.AgentClaudeCode, "/p/app", "sess-a2", 100); got != a {
		t.Fatalf("rotation moved a's process to %s", got)
	}
	if bound, _, _ := h.idx.PeerForRuntimeSession(context.Background(), proto.AgentClaudeCode, "sess-a2"); bound != a {
		t.Fatalf("the rotated session is bound to %s, want %s", bound, a)
	}
	if got, _ := h.register(proto.AgentClaudeCode, "/p/app", "sess-b", 200); got != b {
		t.Fatalf("the sibling moved to %s", got)
	}

	h.offline(a, 100)
	h.offline(b, 200)
	h.restart()
	if got, _ := h.register(proto.AgentClaudeCode, "/p/app", "sess-b", 201); got != b {
		t.Fatalf("after restart sess-b found %s, want %s", got, b)
	}
	if got, _ := h.register(proto.AgentClaudeCode, "/p/app", "sess-a2", 101); got != a {
		t.Fatalf("after restart the rotated session found %s, want %s", got, a)
	}
}

// The experimental channel registers with no pid, session or certificate.
// Its reconnect claims the peer_id it was assigned, which keeps two channel
// sessions in one folder apart, also from durable mappings after a restart,
// whichever reconnects first.
func TestMatrixChannelReconnectKeepsItsClaimedIdentity(t *testing.T) {
	h := newSessionHarness(t)
	channel := func(claim *proto.PeerID) (proto.PeerID, proto.DisplayName) {
		id, name, err := h.r.AllocateAndRegister(context.Background(), AllocateParams{Circle: "work", Backend: proto.AgentClaudeCode,
			Path: ptr("/p/app"), Machine: "m", Role: proto.RoleAgent, ClaimedPeerID: claim})
		if err != nil {
			t.Fatal(err)
		}
		return id, name
	}
	a, aName := channel(nil)
	b, bName := channel(nil)
	if a == b {
		t.Fatal("two channel sessions share an identity")
	}
	for _, restart := range []bool{false, true} {
		h.offline(a, 0)
		h.offline(b, 0)
		if restart {
			h.restart()
		}
		if got, name := channel(&b); got != b || name != bName {
			t.Fatalf("restart=%v: b reconnected as %s/%s, want %s/%s", restart, got, name, b, bName)
		}
		if got, name := channel(&a); got != a || name != aName {
			t.Fatalf("restart=%v: a reconnected as %s/%s, want %s/%s", restart, got, name, a, aName)
		}
	}
}
