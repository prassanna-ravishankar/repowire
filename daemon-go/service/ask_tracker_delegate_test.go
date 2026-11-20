package service

import (
	"context"
	"errors"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

func openAsk(t *testing.T, tr *AskTracker) string {
	t.Helper()
	cid, err := tr.Register(context.Background(), RegisterAskParams{
		FromPeerID: "repow-a", FromPeerName: "alpha", ToPeerID: "repow-b", ToPeerName: "beta", Text: "review?",
		Attachments: []map[string]any{{"id": "att-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return cid
}

func TestDelegateRetargetsAndKeepsTheThread(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	cid := openAsk(t, tr)

	moved, hop, err := tr.Delegate(ctx, cid, DelegateParams{By: "repow-b", To: "repow-c", ToName: "gamma", Note: "owns the parser"})
	if err != nil {
		t.Fatal(err)
	}
	if moved.CorrelationID != cid || moved.FromPeerID != "repow-a" || moved.ToPeerID != "repow-c" || moved.ToPeerName != "gamma" ||
		len(moved.Delegations) != 1 || hop.FromPeerID != "repow-b" || hop.Note != "owns the parser" || len(moved.Attachments) != 1 {
		t.Fatalf("moved = %+v hop = %+v", moved, hop)
	}
	if got, _ := tr.PendingForPeer(ctx, "repow-b", -1, "inbound"); len(got) != 0 {
		t.Fatalf("the delegator is still reminded: %+v", got)
	}
	if got, _ := tr.PendingForPeer(ctx, "repow-c", -1, "inbound"); len(got) != 1 || got[0].CorrelationID != cid {
		t.Fatalf("the new holder is not reminded: %+v", got)
	}
	if !moved.HeldBefore("repow-b") || moved.HeldBefore("repow-c") || moved.HeldBefore("repow-a") {
		t.Fatal("HeldBefore misreports the chain")
	}
	if got := moved.ReplyFrame("ack"); got != "[ack #"+cid+" from @gamma via @beta]" {
		t.Fatalf("reply frame = %q", got)
	}
	again, _, err := tr.Delegate(ctx, cid, DelegateParams{By: "repow-c", To: "repow-d", ToName: "delta"})
	if err != nil {
		t.Fatal(err)
	}
	if got := again.ReplyFrame("declined"); got != "[declined #"+cid+" from @delta via @beta, @gamma]" {
		t.Fatalf("chained reply frame = %q", got)
	}
}

func TestDelegateRefusals(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	cid := openAsk(t, tr)
	for _, tc := range []struct {
		name string
		p    DelegateParams
		want error
	}{
		{"not the holder", DelegateParams{By: "repow-x", To: "repow-c", ToName: "gamma"}, ErrNotHolder},
		{"to the asker", DelegateParams{By: "repow-b", To: "repow-a", ToName: "alpha"}, ErrDelegateTarget},
		{"to the holder", DelegateParams{By: "repow-b", To: "repow-b", ToName: "beta"}, ErrDelegateTarget},
	} {
		if _, _, err := tr.Delegate(ctx, cid, tc.p); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, _, err := tr.Delegate(ctx, "ask-missing", DelegateParams{To: "repow-c"}); !errors.Is(err, ErrAskNotFound) {
		t.Fatalf("unknown: %v", err)
	}

	// An unidentified admin caller may delegate; the hop is still from the holder.
	_, hop, err := tr.Delegate(ctx, cid, DelegateParams{To: "repow-c", ToName: "gamma"})
	if err != nil || hop.FromPeerID != "repow-b" {
		t.Fatalf("admin delegate: %+v %v", hop, err)
	}

	if err := tr.BeginResolve(cid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.Delegate(ctx, cid, DelegateParams{By: "repow-c", To: "repow-d", ToName: "delta"}); !errors.Is(err, ErrAskResolving) {
		t.Fatalf("delegate during a resolution: %v", err)
	}
	tr.EndResolve(cid)

	if err := tr.BeginQuiesce(ctx, "repow-q"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.Delegate(ctx, cid, DelegateParams{By: "repow-c", To: "repow-q", ToName: "quiet"}); !errors.Is(err, ErrQuiesced) {
		t.Fatalf("delegate to a quiescing peer: %v", err)
	}

	tr.Close(ctx, cid, "ack")
	if _, _, err := tr.Delegate(ctx, cid, DelegateParams{By: "repow-c", To: "repow-d", ToName: "delta"}); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("delegate a closed ask: %v", err)
	}
}

func TestDelegationChainIsBounded(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	cid := openAsk(t, tr)
	holder := proto.PeerID("repow-b")
	for i := 0; i < MaxDelegations; i++ {
		next := proto.PeerID("repow-" + string(rune('c'+i)))
		if _, _, err := tr.Delegate(ctx, cid, DelegateParams{By: holder, To: next, ToName: proto.DisplayName(next)}); err != nil {
			t.Fatalf("hop %d: %v", i, err)
		}
		holder = next
	}
	if _, _, err := tr.Delegate(ctx, cid, DelegateParams{By: holder, To: "repow-z", ToName: "zeta"}); !errors.Is(err, ErrDelegationLimit) {
		t.Fatalf("hop past the limit: %v", err)
	}
}

func TestUndoDelegationRestoresOnlyTheLastHop(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	cid := openAsk(t, tr)
	session := "rws-b"
	tr.asks[cid].ToRepowireSessionID = &session

	before, _ := tr.Get(cid)
	_, hop, err := tr.Delegate(ctx, cid, DelegateParams{By: "repow-b", To: "repow-c", ToName: "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	if !tr.UndoDelegation(ctx, cid, hop) {
		t.Fatal("undo refused")
	}
	restored, _ := tr.Get(cid)
	if restored.ToPeerID != "repow-b" || restored.ToPeerName != "beta" || len(restored.Delegations) != 0 ||
		restored.ToRepowireSessionID == nil || *restored.ToRepowireSessionID != "rws-b" {
		t.Fatalf("restored = %+v", restored)
	}
	if tr.UndoDelegation(ctx, cid, hop) {
		t.Fatal("a second undo applied")
	}

	// A hop that is no longer the last one is not undone.
	_, first, _ := tr.Delegate(ctx, cid, DelegateParams{By: "repow-b", To: "repow-c", ToName: "gamma"})
	snapshot, _ := tr.Get(cid)
	if _, _, err := tr.Delegate(ctx, cid, DelegateParams{By: "repow-c", To: "repow-d", ToName: "delta"}); err != nil {
		t.Fatal(err)
	}
	if tr.UndoDelegation(ctx, cid, first) {
		t.Fatal("undid a hop that is not the last")
	}
	// Snapshots never see later hops.
	if len(before.Delegations) != 0 || len(snapshot.Delegations) != 1 {
		t.Fatalf("snapshots changed: before %d snapshot %d", len(before.Delegations), len(snapshot.Delegations))
	}
}

func TestResolutionClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	cid := openAsk(t, tr)
	if err := tr.BeginResolve(cid); err != nil {
		t.Fatal(err)
	}
	if err := tr.BeginResolve(cid); !errors.Is(err, ErrAskResolving) {
		t.Fatalf("second claim: %v", err)
	}
	tr.EndResolve(cid)
	if err := tr.BeginResolve(cid); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	tr.EndResolve(cid)
	tr.Close(ctx, cid, "ack")
	if err := tr.BeginResolve(cid); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("claim on a closed ask: %v", err)
	}
	if err := tr.BeginResolve("ask-missing"); !errors.Is(err, ErrAskNotFound) {
		t.Fatalf("claim on an unknown ask: %v", err)
	}

	q, _ := tr.Register(ctx, RegisterAskParams{FromPeerID: "repow-a", ToPeerID: "repow-b", Question: map[string]any{"kind": "text"}})
	if err := tr.BeginResolve(q); err != nil {
		t.Fatal(err)
	}
	text := "yes"
	if _, err := tr.Answer(ctx, q, Answer{Outcome: "answered", Text: &text}); !errors.Is(err, ErrAskResolving) {
		t.Fatalf("answer during a claimed resolution: %v", err)
	}
}
