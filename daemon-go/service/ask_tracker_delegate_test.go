package service

import (
	"context"
	"errors"
	"testing"
	"time"

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

// delegateClaimed runs one hand-off the way the hub does: under the claim.
func delegateClaimed(t *testing.T, tr *AskTracker, cid string, p DelegateParams) (*Ask, Delegation, error) {
	t.Helper()
	if _, err := tr.Claim(cid); err != nil {
		return nil, Delegation{}, err
	}
	defer tr.Release(cid)
	return tr.Delegate(context.Background(), cid, p)
}

func TestDelegateRetargetsAndKeepsTheThread(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	cid := openAsk(t, tr)

	moved, hop, err := delegateClaimed(t, tr, cid, DelegateParams{By: "repow-b", To: "repow-c", ToName: "gamma", Note: "owns the parser"})
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
	again, _, err := delegateClaimed(t, tr, cid, DelegateParams{By: "repow-c", To: "repow-d", ToName: "delta"})
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

	if _, _, err := tr.Delegate(ctx, cid, DelegateParams{By: "repow-b", To: "repow-c", ToName: "gamma"}); !errors.Is(err, ErrNotClaimed) {
		t.Fatalf("delegate without the claim: %v", err)
	}
	for _, tc := range []struct {
		name string
		p    DelegateParams
		want error
	}{
		{"not the holder", DelegateParams{By: "repow-x", To: "repow-c", ToName: "gamma"}, ErrNotHolder},
		{"to the asker", DelegateParams{By: "repow-b", To: "repow-a", ToName: "alpha"}, ErrDelegateTarget},
		{"to the holder", DelegateParams{By: "repow-b", To: "repow-b", ToName: "beta"}, ErrDelegateTarget},
	} {
		if _, _, err := delegateClaimed(t, tr, cid, tc.p); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, _, err := delegateClaimed(t, tr, "ask-missing", DelegateParams{To: "repow-c"}); !errors.Is(err, ErrAskNotFound) {
		t.Fatalf("unknown: %v", err)
	}

	// An unidentified admin caller may delegate; the hop is still from the holder.
	_, hop, err := delegateClaimed(t, tr, cid, DelegateParams{To: "repow-c", ToName: "gamma"})
	if err != nil || hop.FromPeerID != "repow-b" {
		t.Fatalf("admin delegate: %+v %v", hop, err)
	}

	if err := tr.BeginQuiesce(ctx, "repow-q"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := delegateClaimed(t, tr, cid, DelegateParams{By: "repow-c", To: "repow-q", ToName: "quiet"}); !errors.Is(err, ErrQuiesced) {
		t.Fatalf("delegate to a quiescing peer: %v", err)
	}

	tr.Close(ctx, cid, "ack")
	if _, _, err := delegateClaimed(t, tr, cid, DelegateParams{By: "repow-c", To: "repow-d", ToName: "delta"}); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("delegate a closed ask: %v", err)
	}
}

func TestDelegationChainIsBounded(t *testing.T) {
	tr := NewAskTracker(0)
	cid := openAsk(t, tr)
	holder := proto.PeerID("repow-b")
	for i := 0; i < MaxDelegations; i++ {
		next := proto.PeerID("repow-" + string(rune('c'+i)))
		if _, _, err := delegateClaimed(t, tr, cid, DelegateParams{By: holder, To: next, ToName: proto.DisplayName(next)}); err != nil {
			t.Fatalf("hop %d: %v", i, err)
		}
		holder = next
	}
	if _, _, err := delegateClaimed(t, tr, cid, DelegateParams{By: holder, To: "repow-z", ToName: "zeta"}); !errors.Is(err, ErrDelegationLimit) {
		t.Fatalf("hop past the limit: %v", err)
	}
}

func TestUndoDelegationRestoresTheHolderUnderTheClaim(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	cid := openAsk(t, tr)
	session := "rws-b"
	tr.asks[cid].ToRepowireSessionID = &session

	before, _ := tr.Get(cid)
	if _, err := tr.Claim(cid); err != nil {
		t.Fatal(err)
	}
	_, hop, err := tr.Delegate(ctx, cid, DelegateParams{By: "repow-b", To: "repow-c", ToName: "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := tr.Get(cid)
	if !tr.UndoDelegation(ctx, cid, hop) {
		t.Fatal("undo refused")
	}
	tr.Release(cid)
	restored, _ := tr.Get(cid)
	if restored.ToPeerID != "repow-b" || restored.ToPeerName != "beta" || len(restored.Delegations) != 0 ||
		restored.ToRepowireSessionID == nil || *restored.ToRepowireSessionID != "rws-b" {
		t.Fatalf("restored = %+v", restored)
	}
	// Snapshots never see later changes.
	if len(before.Delegations) != 0 || len(snapshot.Delegations) != 1 || snapshot.ToPeerID != "repow-c" {
		t.Fatalf("snapshots changed: before %+v snapshot %+v", before.Delegations, snapshot)
	}

	// Without the claim, or once the ask has closed, nothing is undone.
	_, hop, _ = delegateClaimed(t, tr, cid, DelegateParams{By: "repow-b", To: "repow-c", ToName: "gamma"})
	if tr.UndoDelegation(ctx, cid, hop) {
		t.Fatal("undid a hop without the claim")
	}
	if _, err := tr.Claim(cid); err != nil {
		t.Fatal(err)
	}
	tr.Close(ctx, cid, "evicted")
	if tr.UndoDelegation(ctx, cid, hop) {
		t.Fatal("undid a hop on a closed ask")
	}
	tr.Release(cid)
}

func TestClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	cid := openAsk(t, tr)
	snapshot, err := tr.Claim(cid)
	if err != nil || snapshot.CorrelationID != cid || snapshot.ToPeerID != "repow-b" {
		t.Fatalf("claim: %+v %v", snapshot, err)
	}
	if _, err := tr.Claim(cid); !errors.Is(err, ErrAskResolving) {
		t.Fatalf("second claim: %v", err)
	}
	tr.Release(cid)
	if _, err := tr.Claim(cid); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	tr.Release(cid)
	tr.Close(ctx, cid, "ack")
	if _, err := tr.Claim(cid); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("claim on a closed ask: %v", err)
	}
	if _, err := tr.Claim("ask-missing"); !errors.Is(err, ErrAskNotFound) {
		t.Fatalf("claim on an unknown ask: %v", err)
	}
}

func TestAnswerRespectsTheClaim(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	q, _ := tr.Register(ctx, RegisterAskParams{FromPeerID: "repow-a", ToPeerID: "repow-b", Question: map[string]any{"kind": "text"}})
	text := "yes"
	if _, err := tr.AnswerClaimed(ctx, q, Answer{Outcome: "answered", Text: &text}); !errors.Is(err, ErrNotClaimed) {
		t.Fatalf("claimed answer without the claim: %v", err)
	}
	if _, err := tr.Claim(q); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Answer(ctx, q, Answer{Outcome: "answered", Text: &text}); !errors.Is(err, ErrAskResolving) {
		t.Fatalf("unclaimed answer during a claim: %v", err)
	}
	if got, err := tr.AnswerClaimed(ctx, q, Answer{Outcome: "answered", Text: &text}); err != nil || !got.Closed || got.CloseReason != "answered" {
		t.Fatalf("claimed answer: %+v %v", got, err)
	}
	tr.Release(q)
}

// Review #523: a runtime's completion (an ACP prompt's reply) must land only
// while its peer still holds the ask, and never alongside another resolution.
func TestRuntimeCompletionOnlyFromTheHolder(t *testing.T) {
	tr := NewAskTracker(0)
	d := &PeerDelivery{asks: tr}

	cid := openAsk(t, tr)
	ran := 0
	d.completeAsHolder(cid, "repow-b", func() { ran++ })
	if ran != 1 {
		t.Fatal("the holder's completion did not run")
	}
	if _, err := tr.Claim(cid); err != nil {
		t.Fatal("the completion kept the claim")
	}
	tr.Release(cid)

	if _, _, err := delegateClaimed(t, tr, cid, DelegateParams{By: "repow-b", To: "repow-c", ToName: "gamma"}); err != nil {
		t.Fatal(err)
	}
	d.completeAsHolder(cid, "repow-b", func() { ran++ })
	if ran != 1 {
		t.Fatal("a completion from the peer the ask was delegated away from ran")
	}

	// A completion that meets another resolution waits it out, then runs
	// under its own claim.
	if _, err := tr.Claim(cid); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(3 * holderClaimPoll)
		tr.Release(cid)
	}()
	d.completeAsHolder(cid, "repow-c", func() {
		if _, err := tr.Claim(cid); !errors.Is(err, ErrAskResolving) {
			t.Errorf("completion ran without the claim: %v", err)
		}
		ran++
	})
	if ran != 2 {
		t.Fatal("the new holder's completion did not run after the claim was released")
	}

	tr.Close(context.Background(), cid, "ack")
	d.completeAsHolder(cid, "repow-c", func() { ran++ })
	if ran != 2 {
		t.Fatal("a completion ran on a closed ask")
	}
}
