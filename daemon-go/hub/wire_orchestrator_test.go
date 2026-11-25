package hub

import (
	"context"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// An orchestrator acts only on an unprotected ask whose every recorded party,
// and a reassign's destination, is in the circle it holds the role for. Its
// actions are attributed to it, and it becomes a recorded party itself.
func TestOrchestratorActionsFollowTheThreadRecords(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma, delta := alphaPeer(), betaPeer(), gammaPeer(), deltaPeer()
	orch := peerWith("repow-default-oooo", "orch", "default", proto.StatusOnline)
	orch.Role = proto.RoleOrchestrator
	// A peer registered without a circle is in default.
	gamma.Circle = ""
	outsider := peerWith("repow-ops-pppp", "ops-orch", "ops", proto.StatusOnline)
	outsider.Role = proto.RoleOrchestrator
	epsilon := peerWith("repow-ops-eeee", "epsilon", "ops", proto.StatusOnline)
	human := peerWith("repow-default-hhhh", "prass", "default", proto.StatusOnline)
	human.Role = proto.RoleHuman
	r := newDelegateRig(t, alpha, beta, gamma, delta, orch, outsider, epsilon, human)
	r.h.deliveryTraces = r.store
	w := service.NewWire(r.asks, r.store, r.reg, r.store, config.Defaults().Wire.Stuck)
	r.h.WithWire(w)
	by := func(p *proto.Peer) wireActor { return wireActor{peer: p} }
	refused := func(name string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "wire_refused") || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v, want a wire_refused naming %q", name, err, want)
		}
	}

	cid := r.ask(t, alpha, beta, "who owns the release?")
	_, err := r.h.wireAction(ctx, by(outsider), "nudge", cid, WireActionRequest{})
	refused("an orchestrator of another circle", err, "was in circle default, not ops")
	_, err = r.h.reassignAsk(ctx, by(orch), cid, "epsilon", "x")
	refused("a destination outside the circle", err, "epsilon is in circle ops")
	_, err = r.h.reassignAsk(ctx, by(orch), cid, "prass", "x")
	refused("a human destination", err, "human")

	if _, err := r.h.wireAction(ctx, by(orch), "nudge", cid, WireActionRequest{Note: "any news?"}); err != nil {
		t.Fatal(err)
	}
	if !hasPrefixed(frameTexts(r.wire.received(beta.PeerID)), "[nudge from @orch: ask #"+cid) {
		t.Fatalf("beta received %q", frameTexts(r.wire.received(beta.PeerID)))
	}

	res, err := r.h.wireAction(ctx, by(orch), "reassign", cid, WireActionRequest{To: "gamma", Reason: "release owner"})
	if err != nil || res.Holder != "gamma" {
		t.Fatalf("reassign: %+v %v", res, err)
	}
	if !hasPrefixed(frameTexts(r.wire.received(gamma.PeerID)), "[reassigned by @orch from @beta for @alpha: release owner]") ||
		!hasPrefixed(frameTexts(r.wire.received(alpha.PeerID)), "[your ask #"+cid+" moved from @beta to @gamma by @orch: release owner]") {
		t.Fatalf("notices: gamma %q, alpha %q", frameTexts(r.wire.received(gamma.PeerID)), frameTexts(r.wire.received(alpha.PeerID)))
	}
	ask, _ := r.asks.Get(cid)
	if ask.Delegations[0].By != string(orch.PeerID) {
		t.Fatalf("hop by %q", ask.Delegations[0].By)
	}
	th, _, _ := w.Thread(ctx, cid)
	if th.Protected || th.Provenance[len(th.Provenance)-1].PeerID != orch.PeerID {
		t.Fatalf("the acting orchestrator is not recorded: %q %+v", th.ProtectedBy, th.Provenance)
	}
	if !strings.Contains(th.ActionsText(), `cancel_ask	{"reason":"<why, at most 200 characters>","thread_id":"`+cid+`"}`) {
		t.Fatalf("actions:%s", th.ActionsText())
	}

	if _, err := r.h.setWireVisibility(ctx, "delta", service.WirePrivate); err != nil {
		t.Fatal(err)
	}
	_, err = r.h.reassignAsk(ctx, by(orch), cid, "delta", "x")
	refused("a private destination", err, "private")

	if _, err := r.h.wireAction(ctx, by(orch), "cancel", cid, WireActionRequest{Reason: "no longer needed"}); err != nil {
		t.Fatal(err)
	}
	if !hasPrefixed(frameTexts(r.wire.received(alpha.PeerID)), "[ask #"+cid+" cancelled by @orch] no longer needed") {
		t.Fatalf("alpha received %q", frameTexts(r.wire.received(alpha.PeerID)))
	}

	// A private party protects the thread from every orchestrator action.
	private := r.ask(t, alpha, delta, "and the changelog?")
	for _, action := range []string{"nudge", "cancel", "reassign"} {
		_, err := r.h.wireAction(ctx, by(orch), action, private, WireActionRequest{To: "beta", Reason: "x"})
		refused(action+" on a protected thread", err, "protected (private)")
	}
	// The operator is not bound by the thread rules.
	if _, err := r.h.wireAction(ctx, operator, "nudge", private, WireActionRequest{}); err != nil {
		t.Fatalf("operator nudge: %v", err)
	}
}
