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

// Review #523: a runtime's completion (an ACP prompt's reply) lands only
// while its peer holds the ask, under the claim, and is never lost to a reply
// or a hand-off holding the ask meanwhile.
func TestRuntimeCompletionOnlyFromTheHolder(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	d := &PeerDelivery{asks: tr}
	ran := make(chan string, 4)
	complete := func(who string) func() {
		return func() {
			if _, err := tr.Claim(openCID(tr)); !errors.Is(err, ErrAskResolving) {
				t.Errorf("%s's completion ran without the claim: %v", who, err)
			}
			ran <- who
		}
	}
	waitRan := func(want string) {
		t.Helper()
		select {
		case got := <-ran:
			if got != want {
				t.Fatalf("completion %q ran, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("completion %q never ran", want)
		}
		// ran is signalled from inside the completion, which still holds the
		// claim until it returns: wait for the release before the next step
		// claims the ask.
		cid := openCID(tr)
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
			tr.mu.Lock()
			_, held := tr.resolving[cid]
			tr.mu.Unlock()
			if !held {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("completion %q never released its claim", want)
			}
		}
	}
	noneRan := func() {
		t.Helper()
		select {
		case got := <-ran:
			t.Fatalf("completion %q ran", got)
		case <-time.After(50 * time.Millisecond):
		}
	}

	cid := openAsk(t, tr)
	d.completeAsHolder(cid, "repow-b", complete("beta"))
	waitRan("beta")

	// A completion arriving during a hand-off that fails lands once the ask
	// is back with its holder.
	if _, err := tr.Claim(cid); err != nil {
		t.Fatal(err)
	}
	_, hop, err := tr.Delegate(ctx, cid, DelegateParams{By: "repow-b", To: "repow-c", ToName: "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	d.completeAsHolder(cid, "repow-b", complete("beta"))
	noneRan()
	if !tr.UndoDelegation(ctx, cid, hop) {
		t.Fatal("undo refused")
	}
	tr.Release(cid)
	waitRan("beta")

	// After a hand-off that succeeds, the former holder's completion is dropped.
	if _, _, err := delegateClaimed(t, tr, cid, DelegateParams{By: "repow-b", To: "repow-c", ToName: "gamma"}); err != nil {
		t.Fatal(err)
	}
	d.completeAsHolder(cid, "repow-b", complete("beta"))
	noneRan()
	d.completeAsHolder(cid, "repow-c", complete("gamma"))
	waitRan("gamma")

	// Nothing runs on a closed ask, even work that was waiting for it.
	if _, err := tr.Claim(cid); err != nil {
		t.Fatal(err)
	}
	d.completeAsHolder(cid, "repow-c", complete("gamma"))
	tr.Close(ctx, cid, "ack")
	tr.Release(cid)
	noneRan()
}

// openCID returns the tracker's one ask id.
func openCID(tr *AskTracker) string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for cid := range tr.asks {
		return cid
	}
	return ""
}

// Review #523: a question's timeout commits its default under the claim, so
// what WaitForAnswer returns is what was recorded, even when the timeout
// meets a hand-off holding the ask.
func TestQuestionTimeoutCommitsTheDefaultAfterAClaim(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	q, _ := tr.Register(ctx, RegisterAskParams{FromPeerID: "repow-a", ToPeerID: "repow-b", Question: map[string]any{"kind": "text"}})
	if _, err := tr.Claim(q); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		tr.Release(q)
	}()
	ans, err := tr.WaitForAnswer(ctx, q, 20*time.Millisecond, nil)
	if err != nil || ans.Outcome != "timed_out" {
		t.Fatalf("timeout answer = %+v %v", ans, err)
	}
	if got, _ := tr.Get(q); !got.Closed || got.Answer == nil || got.Answer.Outcome != "timed_out" {
		t.Fatalf("the returned default was not recorded: %+v", got)
	}
}

// Review #523: a holder's result waiting for an offline asker pins the ask.
func TestStashedResultCannotBeDelegated(t *testing.T) {
	tr := NewAskTracker(0)
	cid := openAsk(t, tr)
	tr.SetPendingReply(context.Background(), cid, "[ack #x from @beta] done", nil, false)
	if _, _, err := delegateClaimed(t, tr, cid, DelegateParams{By: "repow-b", To: "repow-c", ToName: "gamma"}); !errors.Is(err, ErrResultWaiting) {
		t.Fatalf("delegating a stashed result: %v", err)
	}
}

// Review #523: every waiter on an ask learns its terminal state, not just
// the first; the shared one-value channel used to strand the rest.
func TestEveryWaiterReturnsTheRecordedAnswer(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	q, _ := tr.Register(ctx, RegisterAskParams{CorrelationID: "q-1", FromPeerID: "repow-a", ToPeerID: "repow-b", Question: map[string]any{"kind": "text"}})
	type result struct {
		ans Answer
		err error
	}
	answers := make(chan result, 2)
	for _, timeout := range []time.Duration{10 * time.Millisecond, 30 * time.Millisecond} {
		go func(timeout time.Duration) {
			ans, err := tr.WaitForAnswer(ctx, q, timeout, nil)
			answers <- result{ans, err}
		}(timeout)
	}
	resolved := make(chan *Ask, 1)
	go func() {
		ask, _ := tr.WaitForResolution(ctx, q, 2*time.Second, false)
		resolved <- ask
	}()
	for i := 0; i < 2; i++ {
		select {
		case r := <-answers:
			if r.err != nil || r.ans.Outcome != "timed_out" {
				t.Fatalf("waiter %d: %+v %v", i, r.ans, r.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d never returned", i)
		}
	}
	if ask := <-resolved; ask == nil || !ask.Closed || ask.Answer == nil || ask.Answer.Outcome != "timed_out" {
		t.Fatalf("resolution waiter = %+v", ask)
	}
}

// Review #523: work queued for an ask never runs against a new ask that
// reuses its id after the first was evicted.
func TestQueuedWorkIsBoundToItsAsk(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	cid, _ := tr.Register(ctx, RegisterAskParams{CorrelationID: "ask-reused", FromPeerID: "repow-a", ToPeerID: "repow-b"})
	if _, err := tr.Claim(cid); err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{}, 1)
	tr.RunClaimed(cid, func(*Ask) { ran <- struct{}{} })
	tr.ForgetPeer(ctx, "repow-b")
	if _, err := tr.Register(ctx, RegisterAskParams{CorrelationID: cid, FromPeerID: "repow-a", ToPeerID: "repow-b"}); err != nil {
		t.Fatal(err)
	}
	tr.Release(cid)
	select {
	case <-ran:
		t.Fatal("work queued for the evicted ask ran on its replacement")
	case <-time.After(50 * time.Millisecond):
	}

	// Work Release already handed off is checked against the instance too.
	other, _ := tr.Register(ctx, RegisterAskParams{CorrelationID: "ask-other", FromPeerID: "repow-a", ToPeerID: "repow-c"})
	first, _ := tr.Get(other)
	tr.mu.Lock()
	instance := tr.asks[other]
	tr.mu.Unlock()
	tr.ForgetPeer(ctx, "repow-c")
	_, _ = tr.Register(ctx, RegisterAskParams{CorrelationID: other, FromPeerID: "repow-a", ToPeerID: "repow-c"})
	tr.runClaimedOn(other, instance, func(*Ask) { ran <- struct{}{} })
	select {
	case <-ran:
		t.Fatalf("handed-off work for %s ran on its replacement", first.CorrelationID)
	case <-time.After(50 * time.Millisecond):
	}
}

// Review #523: an ask evicted while claimed keeps its id until the claimant
// releases, so the claimant's last write cannot land on a replacement.
func TestEvictionUnderAClaimCannotLeakIntoAReplacement(t *testing.T) {
	ctx := context.Background()
	tr := NewAskTracker(0)
	q, _ := tr.Register(ctx, RegisterAskParams{CorrelationID: "q-reuse", FromPeerID: "repow-a", ToPeerID: "repow-b", Question: map[string]any{"kind": "text"}})
	entered, proceed, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	released := make(chan struct{})
	go func() {
		tr.RunClaimed(q, func(*Ask) {
			close(entered)
			<-proceed
			old := "old default"
			_, err := tr.AnswerClaimed(ctx, q, Answer{Outcome: "answered", Text: &old})
			finished <- err
		})
		close(released)
	}()
	<-entered
	tr.ForgetPeer(ctx, "repow-b")
	// Same-id registration under the claim finds the closed instance.
	if _, err := tr.Register(ctx, RegisterAskParams{CorrelationID: q, FromPeerID: "repow-a", ToPeerID: "repow-b", Question: map[string]any{"kind": "text"}}); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	if err := <-finished; !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("the claimant's write after eviction: %v", err)
	}
	<-released
	if got, ok := tr.Get(q); ok {
		t.Fatalf("the evicted ask outlived its claim: %+v", got)
	}
	// After release the id is free, and a new ask under it is untouched.
	if _, err := tr.Register(ctx, RegisterAskParams{CorrelationID: q, FromPeerID: "repow-a", ToPeerID: "repow-b", Question: map[string]any{"kind": "text"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := tr.Get(q); got.Closed || got.Answer != nil {
		t.Fatalf("replacement = %+v", got)
	}
}
