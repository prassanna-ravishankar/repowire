package service

import (
	"context"
	"errors"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

func addressabilityPeers() (*proto.Peer, *proto.Peer, *proto.Peer) {
	sender := &proto.Peer{PeerID: "repow-1-alpha", DisplayName: "alpha", Circle: "1", Status: proto.StatusOnline, Role: proto.RoleAgent,
		Provenance: proto.Provenance{Source: proto.SourceHook, Addressable: true}}
	child := &proto.Peer{PeerID: "repow-1-child", DisplayName: "app-2-codex", Circle: "1", Status: proto.StatusOnline, Role: proto.RoleAgent,
		Provenance: proto.Provenance{Source: proto.SourceCodexAppServer, Initiator: proto.InitiatorAgent, ParentRuntimeID: "thread-parent", Addressable: false, AddressableReason: "subagent_direct_input_denied"}}
	other := &proto.Peer{PeerID: "repow-1-beta", DisplayName: "beta", Circle: "1", Status: proto.StatusOnline, Role: proto.RoleAgent,
		Provenance: proto.Provenance{Source: proto.SourceHook, Addressable: true}}
	return sender, child, other
}

// Notify and DeliverAsk refuse a non-addressable target before touching the
// transport or the durable queue; the child keeps its own outbound notify.
func TestDeliveryRefusesNonAddressableTargetWithoutQueueing(t *testing.T) {
	sender, child, other := addressabilityPeers()
	reg := &fakeRegistry{peers: []*proto.Peer{sender, child, other}}
	f := &fakeTransport{}
	q := &fakeQueue{}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, NewAskTracker(0), q)
	ctx := context.Background()

	var na *proto.NotAddressableError
	if _, err := d.Notify(ctx, NotifyParams{FromPeer: "alpha", ToPeer: "app-2-codex", Text: "x"}); !errors.As(err, &na) || na.PeerID != child.PeerID {
		t.Fatalf("notify to child = %v, want NotAddressableError", err)
	}
	if _, err := d.DeliverAsk(ctx, DeliverAskParams{FromPeer: "alpha", ToPeer: "app-2-codex", Text: "x", CorrelationID: "ask-1"}); !errors.As(err, &na) {
		t.Fatalf("ask to child = %v, want NotAddressableError", err)
	}
	if len(f.sentTargets) != 0 || len(q.enqueued) != 0 {
		t.Fatalf("refused delivery touched transport or queue: sent=%v queued=%d", f.sentTargets, len(q.enqueued))
	}
	// Outbound from the child is still allowed (the transport fake decides delivery).
	if _, err := d.Notify(ctx, NotifyParams{FromPeer: "app-2-codex", ToPeer: "beta", Text: "reply"}); err != nil {
		if errors.As(err, &na) {
			t.Fatalf("child outbound notify was gated: %v", err)
		}
	}
}

// Broadcast skips non-addressable peers instead of counting them as failures.
func TestBroadcastSkipsNonAddressablePeers(t *testing.T) {
	sender, child, other := addressabilityPeers()
	reg := &fakeRegistry{peers: []*proto.Peer{sender, child, other}}
	f := &fakeTransport{}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, nil, nil)
	sent, failed := d.Broadcast(context.Background(), "alpha", "hello", nil, false)
	for _, name := range sent {
		if name == child.DisplayName {
			t.Fatalf("broadcast delivered to non-addressable child: sent=%v", sent)
		}
	}
	for _, fail := range failed {
		if fail.PeerID == child.PeerID {
			t.Fatalf("broadcast counted the child as a failure: %+v", failed)
		}
	}
	for _, target := range f.sentTargets {
		if target == child.PeerID {
			t.Fatalf("transport was asked to reach the child: %v", f.sentTargets)
		}
	}
}

// OpenScheduledAsk must not leave a tracker entry behind when the target
// cannot take input.
func TestOpenScheduledAskRollsBackOnNonAddressable(t *testing.T) {
	sender, child, _ := addressabilityPeers()
	reg := &fakeRegistry{peers: []*proto.Peer{sender, child}}
	f := &fakeTransport{}
	asks := NewAskTracker(0)
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, asks, nil)
	if _, err := d.OpenScheduledAsk(context.Background(), "alpha", "app-2-codex", "do it", nil, "pull"); err == nil {
		t.Fatal("scheduled ask to a non-addressable peer succeeded")
	}
	if asks.OpenCount() != 0 {
		t.Fatalf("tracker kept %d open ask(s) after refusal", asks.OpenCount())
	}
}

// CloseInboundForPeer closes only asks addressed to the peer, keeps the
// records, and reports the askers.
func TestCloseInboundForPeer(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	in, err := tr.Register(ctx, RegisterAskParams{FromPeerID: "repow-1-alpha", FromPeerName: "alpha", ToPeerID: "repow-1-child", ToPeerName: "child", Text: "q"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tr.Register(ctx, RegisterAskParams{FromPeerID: "repow-1-child", FromPeerName: "child", ToPeerID: "repow-1-alpha", ToPeerName: "alpha", Text: "q"})
	if err != nil {
		t.Fatal(err)
	}
	closed := tr.CloseInboundForPeer("repow-1-child", "peer_not_addressable")
	if len(closed) != 1 || closed[0].CorrelationID != in || closed[0].FromPeerID != "repow-1-alpha" {
		t.Fatalf("closed = %+v", closed)
	}
	if ask, ok := tr.Get(in); !ok || !ask.Closed || ask.CloseReason != "peer_not_addressable" {
		t.Fatalf("inbound ask state = %+v, %v", ask, ok)
	}
	if ask, ok := tr.Get(out); !ok || ask.Closed {
		t.Fatalf("outbound ask was closed: %+v", ask)
	}
}
