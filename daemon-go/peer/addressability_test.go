package peer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

type fakeQueue struct{ dropped []proto.PeerID }

func (q *fakeQueue) DropDeliveriesForPeer(_ context.Context, id proto.PeerID) (int, error) {
	q.dropped = append(q.dropped, id)
	return 2, nil
}

// CheckAccess is the shared routing gate: a non-addressable target is refused
// for everyone except itself.
func TestCheckAccessRefusesNonAddressableTarget(t *testing.T) {
	r, _ := newRegistry(t)
	ctx := context.Background()
	path := "/p"
	denied := proto.Provenance{Source: proto.SourceCodexAppServer, Addressable: false, AddressableReason: "subagent_direct_input_denied"}
	childID, childName, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentCodex, Path: &path, Role: proto.RoleAgent, Provenance: &denied})
	if err != nil {
		t.Fatal(err)
	}
	_, senderName, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: &path, Role: proto.RoleAgent, HookVersion: true})
	if err != nil {
		t.Fatal(err)
	}
	var na *proto.NotAddressableError
	if _, _, err := r.CheckAccess(ctx, string(senderName), string(childName), false, nil); !errors.As(err, &na) || na.PeerID != childID {
		t.Fatalf("CheckAccess to child = %v", err)
	}
	if _, _, err := r.CheckAccess(ctx, string(childName), string(senderName), false, nil); err != nil {
		t.Fatalf("child outbound refused: %v", err)
	}
	if _, _, err := r.CheckAccess(ctx, string(childName), string(childName), true, nil); err != nil {
		t.Fatalf("self access refused: %v", err)
	}
}

// Losing addressability while live closes open inbound asks, drops queued
// deliveries, tells each asker, and records one event. The same happens when
// the demotion arrives via re-registration instead of the provenance endpoint.
func TestDemotionFailsPendingInboundWorkLoudly(t *testing.T) {
	for _, via := range []string{"endpoint", "reregister"} {
		t.Run(via, func(t *testing.T) {
			r, _ := newRegistry(t)
			asks := newFakeAsks()
			delivery := &fakeDelivery{}
			queue := &fakeQueue{}
			r.WithReconciliation(asks, delivery, fakeProbe{}, ExperimentsConfig{}, 0, 0)
			r.WithDeliveryQueue(queue)
			ctx := context.Background()
			path := "/p"
			accepting := proto.Provenance{Source: proto.SourceCodexAppServer, Addressable: true}
			childID, childName, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentCodex, Path: &path, Role: proto.RoleAgent, Provenance: &accepting})
			if err != nil {
				t.Fatal(err)
			}
			askerID, _, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: &path, Role: proto.RoleAgent, HookVersion: true})
			if err != nil {
				t.Fatal(err)
			}
			asks.add(StashedAsk{CorrelationID: "ask-in", FromPeerID: askerID, ToPeerID: childID})
			asks.add(StashedAsk{CorrelationID: "ask-out", FromPeerID: childID, ToPeerID: askerID})

			denied := proto.Provenance{Source: proto.SourceCodexAppServer, Addressable: false, AddressableReason: "subagent_direct_input_denied"}
			switch via {
			case "endpoint":
				if found, err := r.UpdateProvenance(ctx, string(childName), denied, nil); err != nil || !found {
					t.Fatalf("UpdateProvenance = %v, %v", found, err)
				}
			default:
				if _, _, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentCodex, Path: &path, Role: proto.RoleAgent, ClaimedPeerID: &childID, Provenance: &denied}); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				delivery.mu.Lock()
				n := len(delivery.calls)
				delivery.mu.Unlock()
				if n > 0 && len(queue.dropped) > 0 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if len(asks.closedInbound) != 1 || asks.closedInbound[0].CorrelationID != "ask-in" {
				t.Fatalf("closed inbound = %+v, want only ask-in", asks.closedInbound)
			}
			if _, still := asks.asks["ask-out"]; !still {
				t.Fatal("outbound ask from the child was closed")
			}
			if len(queue.dropped) != 1 || queue.dropped[0] != childID {
				t.Fatalf("queue drops = %v", queue.dropped)
			}
			delivery.mu.Lock()
			calls := append([]deliveryCall(nil), delivery.calls...)
			delivery.mu.Unlock()
			if len(calls) != 1 || calls[0].from != childID || calls[0].to != askerID {
				t.Fatalf("asker notifications = %+v", calls)
			}
			var event map[string]any
			for _, e := range r.GetEvents() {
				if e["type"] == "peer_not_addressable" && e["peer_id"] == string(childID) {
					event = e
				}
			}
			if event == nil {
				t.Fatal("no peer_not_addressable event")
			}
			// A second identical demotion is a no-op: nothing else is closed.
			if _, err := r.UpdateProvenance(ctx, string(childName), denied, nil); err != nil {
				t.Fatal(err)
			}
			if len(asks.closedInbound) != 1 {
				t.Fatalf("repeated demotion re-closed asks: %+v", asks.closedInbound)
			}
		})
	}
}
