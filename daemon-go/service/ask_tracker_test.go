package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

func strp(s string) *string { return &s }

// reg is a tiny helper that registers a default inbound ask alice->bob.
func reg(t *testing.T, tr *AskTracker, p RegisterAskParams) string {
	t.Helper()
	cid, err := tr.Register(context.Background(), p)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return cid
}

func defaultAsk(from, to proto.PeerID) RegisterAskParams {
	return RegisterAskParams{
		FromPeerID:   from,
		FromPeerName: proto.DisplayName(string(from)),
		ToPeerID:     to,
		ToPeerName:   proto.DisplayName(string(to)),
		Text:         "ping",
	}
}

func TestRegisterMintsCorrelationID(t *testing.T) {
	tr := NewAskTracker(0)
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	if len(cid) == 0 || cid[:4] != "ask-" {
		t.Fatalf("expected minted ask- cid, got %q", cid)
	}
	ask, ok := tr.Get(cid)
	if !ok {
		t.Fatal("ask not found after register")
	}
	if ask.ReplyDelivery != "push" {
		t.Fatalf("default ReplyDelivery=push, got %q", ask.ReplyDelivery)
	}
	if ask.Closed {
		t.Fatal("new ask should be open")
	}
}

func TestRegisterRetryPreservesEntry(t *testing.T) {
	tr := NewAskTracker(0)
	p := defaultAsk("alice", "bob")
	p.CorrelationID = "ask-fixed"
	cid := reg(t, tr, p)
	// close it, then re-register the same cid: existing (closed) entry preserved.
	tr.Close(context.Background(), cid, "ack")
	p.Text = "different"
	cid2 := reg(t, tr, p)
	if cid2 != "ask-fixed" {
		t.Fatalf("retry should return same cid, got %q", cid2)
	}
	ask, _ := tr.Get(cid2)
	if !ask.Closed || ask.Text != "ping" {
		t.Fatalf("retry must preserve existing entry, got closed=%v text=%q", ask.Closed, ask.Text)
	}
}

func TestGetSnapshotIsCopy(t *testing.T) {
	tr := NewAskTracker(0)
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	snap, _ := tr.Get(cid)
	snap.Text = "mutated"
	again, _ := tr.Get(cid)
	if again.Text != "ping" {
		t.Fatal("Get must return a snapshot copy, not the live pointer")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	tr := NewAskTracker(0)
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	ask, ok := tr.Close(context.Background(), cid, "ack")
	if !ok || ask.CloseReason != "ack" {
		t.Fatalf("first close should succeed with reason, got ok=%v", ok)
	}
	if _, ok := tr.Close(context.Background(), cid, "ack_with_msg"); ok {
		t.Fatal("second close must be a no-op (false)")
	}
	if _, ok := tr.Close(context.Background(), "missing", "ack"); ok {
		t.Fatal("close of unknown cid must be false")
	}
}

func TestPendingForPeerDirectionsAndOrder(t *testing.T) {
	tr := NewAskTracker(0)
	c1 := reg(t, tr, defaultAsk("alice", "bob"))
	time.Sleep(2 * time.Millisecond)
	reg(t, tr, defaultAsk("alice", "bob"))
	time.Sleep(2 * time.Millisecond)
	// outbound from carol to bob (newest)
	cNewest := reg(t, tr, defaultAsk("carol", "bob"))

	inbound, err := tr.PendingForPeer(context.Background(), "bob", 10, "inbound")
	if err != nil {
		t.Fatal(err)
	}
	if len(inbound) != 3 {
		t.Fatalf("bob inbound = 3, got %d", len(inbound))
	}
	// newest first, strictly descending CreatedAt
	if inbound[0].CorrelationID != cNewest {
		t.Fatalf("newest ask must sort first; got %q", inbound[0].CorrelationID)
	}
	for i := 1; i < len(inbound); i++ {
		if inbound[i-1].CreatedAt.Before(inbound[i].CreatedAt) {
			t.Fatal("PendingForPeer must be newest-first")
		}
	}

	outbound, _ := tr.PendingForPeer(context.Background(), "alice", 10, "outbound")
	if len(outbound) != 2 {
		t.Fatalf("alice outbound = 2, got %d", len(outbound))
	}

	both, _ := tr.PendingForPeer(context.Background(), "alice", 10, "both")
	if len(both) != 2 {
		t.Fatalf("alice both = 2, got %d", len(both))
	}

	// cap
	capped, _ := tr.PendingForPeer(context.Background(), "bob", 1, "inbound")
	if len(capped) != 1 || capped[0].CorrelationID != cNewest {
		t.Fatalf("cap=1 should yield newest only, got %d", len(capped))
	}

	// closed asks drop out
	tr.Close(context.Background(), c1, "ack")
	after, _ := tr.PendingForPeer(context.Background(), "bob", 10, "inbound")
	if len(after) != 2 {
		t.Fatalf("after close, bob inbound = 2, got %d", len(after))
	}

	if _, err := tr.PendingForPeer(context.Background(), "bob", 10, "sideways"); err == nil {
		t.Fatal("invalid direction must error")
	}
}

func TestAnswerValidatesAndCloses(t *testing.T) {
	tr := NewAskTracker(0)
	p := defaultAsk("alice", "bob")
	p.Question = map[string]any{
		"kind": "choice",
		"options": []any{
			map[string]any{"id": "allow", "title": "Allow"},
			map[string]any{"id": "deny", "title": "Deny"},
		},
	}
	cid := reg(t, tr, p)

	// invalid: unknown option
	if _, err := tr.Answer(context.Background(), cid, Answer{Outcome: "answered", OptionID: strp("nope")}); err == nil {
		t.Fatal("unknown option_id must be rejected")
	}
	// invalid: choice with no option_id
	if _, err := tr.Answer(context.Background(), cid, Answer{Outcome: "answered"}); err == nil {
		t.Fatal("choice answer without option_id must be rejected")
	}
	// valid
	ask, err := tr.Answer(context.Background(), cid, Answer{Outcome: "answered", OptionID: strp("allow")})
	if err != nil {
		t.Fatalf("valid answer: %v", err)
	}
	if !ask.Closed || ask.CloseReason != "answered" || ask.Answer == nil {
		t.Fatalf("answered ask must close as answered with Answer set")
	}
	// second answer rejected
	if _, err := tr.Answer(context.Background(), cid, Answer{Outcome: "denied"}); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("second answer must be ErrAlreadyAnswered, got %v", err)
	}
	// unknown cid
	if _, err := tr.Answer(context.Background(), "missing", Answer{Outcome: "acknowledged"}); !errors.Is(err, ErrAskNotFound) {
		t.Fatalf("unknown cid must be ErrAskNotFound, got %v", err)
	}
}

func TestAnswerNonSelectingOutcomesBypassValidation(t *testing.T) {
	tr := NewAskTracker(0)
	p := defaultAsk("alice", "bob")
	p.Question = map[string]any{"kind": "choice", "options": []any{map[string]any{"id": "allow"}}}
	cid := reg(t, tr, p)
	// denied/timed_out/cancelled/acknowledged need no option_id
	if _, err := tr.Answer(context.Background(), cid, Answer{Outcome: "denied"}); err != nil {
		t.Fatalf("denied should bypass option validation: %v", err)
	}
}

func TestWaitForAnswerResolvedByAnswer(t *testing.T) {
	tr := NewAskTracker(0)
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	done := make(chan Answer, 1)
	go func() {
		a, _ := tr.WaitForAnswer(context.Background(), cid, 2*time.Second, nil)
		done <- a
	}()
	time.Sleep(10 * time.Millisecond)
	if _, err := tr.Answer(context.Background(), cid, Answer{Outcome: "answered", Text: strp("hi")}); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-done:
		if a.Outcome != "answered" {
			t.Fatalf("waiter got %q", a.Outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not resolve")
	}
}

func TestWaitForAnswerTimeoutAppliesDefaultAndRecords(t *testing.T) {
	tr := NewAskTracker(0)
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	def := &Answer{Outcome: "timed_out", Message: strp("no answer")}
	a, err := tr.WaitForAnswer(context.Background(), cid, 20*time.Millisecond, def)
	if err != nil {
		t.Fatal(err)
	}
	if a.Outcome != "timed_out" {
		t.Fatalf("timeout should apply default, got %q", a.Outcome)
	}
	// the ledger must reflect the recorded answer (closed as answered)
	ask, _ := tr.Get(cid)
	if !ask.Closed || ask.Answer == nil || ask.Answer.Outcome != "timed_out" {
		t.Fatalf("timeout path must record the answer; got closed=%v answer=%v", ask.Closed, ask.Answer)
	}
}

func TestWaitForAnswerInvalidDefaultFailsLoud(t *testing.T) {
	tr := NewAskTracker(0)
	p := defaultAsk("alice", "bob")
	p.Question = map[string]any{"kind": "choice", "options": []any{map[string]any{"id": "allow"}}}
	cid := reg(t, tr, p)
	// default with outcome "answered" but no option_id is invalid for a choice
	def := &Answer{Outcome: "answered"}
	if _, err := tr.WaitForAnswer(context.Background(), cid, 10*time.Millisecond, def); err == nil {
		t.Fatal("invalid default_answer must fail loud before the wait")
	}
}

func TestWaitForResolutionTimeoutRecordsNothing(t *testing.T) {
	tr := NewAskTracker(0)
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	got, err := tr.WaitForResolution(context.Background(), cid, 20*time.Millisecond, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("timeout must return (nil,nil)")
	}
	ask, _ := tr.Get(cid)
	if ask.Closed {
		t.Fatal("ask must stay open after WaitForResolution timeout")
	}
	if ask.ReplyDelivery != "pull" {
		t.Fatalf("pull=true must switch delivery to pull, got %q", ask.ReplyDelivery)
	}
}

func TestWaitForResolutionResolves(t *testing.T) {
	tr := NewAskTracker(0)
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	done := make(chan *Ask, 1)
	go func() {
		a, _ := tr.WaitForResolution(context.Background(), cid, 2*time.Second, true)
		done <- a
	}()
	time.Sleep(10 * time.Millisecond)
	tr.Close(context.Background(), cid, "ack")
	select {
	case a := <-done:
		if a == nil || !a.Closed {
			t.Fatal("resolution should return the closed ask")
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForResolution did not return")
	}
}

func TestCaptureReplyOnceOnly(t *testing.T) {
	tr := NewAskTracker(0)
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	tr.CaptureReply(context.Background(), cid, "first", nil)
	tr.CaptureReply(context.Background(), cid, "second", []map[string]any{{"k": "v"}})
	ask, _ := tr.Get(cid)
	if ask.ReplyText == nil || *ask.ReplyText != "first" {
		t.Fatalf("capture must be once-only, got %v", ask.ReplyText)
	}
	if ask.ReplyAttachments != nil {
		t.Fatal("second capture (with attachments) must be a no-op")
	}
}

func TestQuiesceBlocksRegister(t *testing.T) {
	tr := NewAskTracker(0)
	if err := tr.BeginQuiesce(context.Background(), "bob"); err != nil {
		t.Fatalf("BeginQuiesce on clean peer: %v", err)
	}
	// concurrent begin is exclusive
	if err := tr.BeginQuiesce(context.Background(), "bob"); !errors.Is(err, ErrQuiesced) {
		t.Fatalf("double quiesce must be ErrQuiesced, got %v", err)
	}
	// register to or from a quiescing peer is refused
	if _, err := tr.Register(context.Background(), defaultAsk("alice", "bob")); !errors.Is(err, ErrQuiesced) {
		t.Fatalf("register to quiesced peer must be ErrQuiesced, got %v", err)
	}
	if _, err := tr.Register(context.Background(), defaultAsk("bob", "alice")); !errors.Is(err, ErrQuiesced) {
		t.Fatalf("register from quiesced peer must be ErrQuiesced, got %v", err)
	}
	tr.EndQuiesce(context.Background(), "bob")
	tr.EndQuiesce(context.Background(), "bob") // idempotent
	if _, err := tr.Register(context.Background(), defaultAsk("alice", "bob")); err != nil {
		t.Fatalf("after EndQuiesce register should succeed: %v", err)
	}
}

func TestBeginQuiesceRefusesWithOpenAsks(t *testing.T) {
	tr := NewAskTracker(0)
	reg(t, tr, defaultAsk("alice", "bob"))
	if err := tr.BeginQuiesce(context.Background(), "bob"); !errors.Is(err, ErrQuiesceHasOpen) {
		t.Fatalf("open asks must block quiesce with ErrQuiesceHasOpen, got %v", err)
	}
}

func TestPendingReplyStashAndRedeliver(t *testing.T) {
	tr := NewAskTracker(0)
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	id := &AskerIdentity{DisplayName: "alice", Circle: "c", Backend: proto.AgentClaudeCode, Path: "/p", Machine: "m"}
	if !tr.SetPendingReply(context.Background(), cid, "[ack] hi", id, false) {
		t.Fatal("stash on open ask should succeed")
	}
	// snapshot for asker (pass-1)
	if got := tr.TakePendingRepliesForAsker("alice"); len(got) != 1 {
		t.Fatalf("pass-1 snapshot = 1, got %d", len(got))
	}
	// pass-2 identity match, original peer not live
	matches := tr.TakeOrphanPendingRepliesMatching(*id, map[proto.PeerID]struct{}{})
	if len(matches) != 1 {
		t.Fatalf("orphan match = 1, got %d", len(matches))
	}
	// if original peer is live, it is NOT an orphan
	live := map[proto.PeerID]struct{}{"alice": {}}
	if len(tr.TakeOrphanPendingRepliesMatching(*id, live)) != 0 {
		t.Fatal("live asker must not be treated as orphan")
	}
	// rebind + close
	if !tr.RebindAndClose(context.Background(), cid, "alice2", "ack_with_msg") {
		t.Fatal("rebind should apply on open ask")
	}
	ask, _ := tr.Get(cid)
	if ask.FromPeerID != "alice2" || !ask.Closed || ask.PendingReply != nil {
		t.Fatalf("rebind must rewrite from, close, clear stash; got %+v", ask)
	}
	if tr.RebindAndClose(context.Background(), cid, "alice3", "x") {
		t.Fatal("rebind on closed ask must be false")
	}
}

func TestSetPendingReplyOnClosedAskRejectedUnlessAnsweredQuestion(t *testing.T) {
	tr := NewAskTracker(0)
	// plain ask, closed -> rejected even with allowAnsweredQuestion
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	tr.Close(context.Background(), cid, "ack")
	if tr.SetPendingReply(context.Background(), cid, "x", nil, true) {
		t.Fatal("closed plain ask must reject stash")
	}
	// answered structured question -> allowed only with the escape hatch
	p := defaultAsk("alice", "bob")
	p.Question = map[string]any{"kind": "acknowledge"}
	qcid := reg(t, tr, p)
	tr.Answer(context.Background(), qcid, Answer{Outcome: "acknowledged"})
	if tr.SetPendingReply(context.Background(), qcid, "x", nil, false) {
		t.Fatal("answered question stash must be refused without the flag")
	}
	if !tr.SetPendingReply(context.Background(), qcid, "x", nil, true) {
		t.Fatal("answered question stash must be allowed with the flag")
	}
}

func TestMarkPendingReplyDelivered(t *testing.T) {
	tr := NewAskTracker(0)
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	// no stash yet
	if tr.MarkPendingReplyDelivered(context.Background(), cid, nil, "ack_with_msg") {
		t.Fatal("mark without a stash must be false")
	}
	tr.SetPendingReply(context.Background(), cid, "r", nil, false)
	newID := proto.PeerID("alice2")
	if !tr.MarkPendingReplyDelivered(context.Background(), cid, &newID, "ack_with_msg") {
		t.Fatal("mark with stash should succeed")
	}
	ask, _ := tr.Get(cid)
	if ask.FromPeerID != "alice2" || !ask.Closed || ask.PendingReply != nil {
		t.Fatalf("mark must rebind, close, clear; got %+v", ask)
	}
}

func TestSnapshotsForLossEmission(t *testing.T) {
	tr := NewAskTracker(0)
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	tr.SetPendingReply(context.Background(), cid, "r", nil, false)
	if got := tr.SnapshotPendingRepliesForPeer("bob"); len(got) != 1 {
		t.Fatalf("snapshot for involved peer = 1, got %d", len(got))
	}
	if got := tr.SnapshotPendingRepliesForPeer("zzz"); len(got) != 0 {
		t.Fatalf("snapshot for uninvolved peer = 0, got %d", len(got))
	}
}

func TestEvictExpiredRespectsStashed(t *testing.T) {
	tr := NewAskTracker(time.Hour)
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	stashCid := reg(t, tr, defaultAsk("carol", "bob"))
	tr.SetPendingReply(context.Background(), stashCid, "r", nil, false)
	// force both asks past TTL
	tr.mu.Lock()
	old := time.Now().UTC().Add(-2 * time.Hour)
	tr.asks[cid].CreatedAt = old
	tr.asks[stashCid].CreatedAt = old
	tr.mu.Unlock()

	// expired snapshot only counts stashed
	if got := tr.SnapshotExpiredPendingReplies(); len(got) != 1 {
		t.Fatalf("expired stashed snapshot = 1, got %d", len(got))
	}
	// include_stashed=false leaves the stashed ask
	if n := tr.EvictExpired(context.Background(), false); n != 1 {
		t.Fatalf("evict(includeStashed=false) should drop only the plain ask, got %d", n)
	}
	if _, ok := tr.Get(stashCid); !ok {
		t.Fatal("stashed-expired ask must survive includeStashed=false")
	}
	// include_stashed=true reaps it
	if n := tr.EvictExpired(context.Background(), true); n != 1 {
		t.Fatalf("evict(includeStashed=true) should drop the stashed ask, got %d", n)
	}
	if _, ok := tr.Get(stashCid); ok {
		t.Fatal("stashed ask must be gone after includeStashed=true")
	}
}

func TestForgetPeerDropsBothDirections(t *testing.T) {
	tr := NewAskTracker(0)
	reg(t, tr, defaultAsk("alice", "bob")) // bob inbound
	reg(t, tr, defaultAsk("bob", "carol")) // bob outbound
	reg(t, tr, defaultAsk("alice", "carol"))
	n := tr.ForgetPeer(context.Background(), "bob")
	if n != 2 {
		t.Fatalf("ForgetPeer(bob) should drop 2, got %d", n)
	}
	if tr.TotalCount() != 1 {
		t.Fatalf("one ask should remain, got %d", tr.TotalCount())
	}
}

func TestForgetPeerCancelsWaiter(t *testing.T) {
	tr := NewAskTracker(0)
	cid := reg(t, tr, defaultAsk("alice", "bob"))
	done := make(chan Answer, 1)
	go func() {
		a, _ := tr.WaitForAnswer(context.Background(), cid, 2*time.Second, nil)
		done <- a
	}()
	time.Sleep(10 * time.Millisecond)
	tr.ForgetPeer(context.Background(), "bob")
	select {
	case a := <-done:
		if a.Outcome != "cancelled" {
			t.Fatalf("forget must cancel the waiter, got %q", a.Outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("forget did not cancel the blocking waiter")
	}
}

func TestOpenAndTotalCount(t *testing.T) {
	tr := NewAskTracker(0)
	c1 := reg(t, tr, defaultAsk("alice", "bob"))
	reg(t, tr, defaultAsk("alice", "bob"))
	tr.Close(context.Background(), c1, "ack")
	if tr.OpenCount() != 1 {
		t.Fatalf("OpenCount = 1, got %d", tr.OpenCount())
	}
	if tr.TotalCount() != 2 {
		t.Fatalf("TotalCount = 2, got %d", tr.TotalCount())
	}
}

func TestListByParent(t *testing.T) {
	tr := NewAskTracker(0)
	parent := "ask-parent"
	c1 := defaultAsk("alice", "bob")
	c1.ParentID = &parent
	reg(t, tr, c1)
	c2 := defaultAsk("alice", "carol")
	c2.ParentID = &parent
	reg(t, tr, c2)
	reg(t, tr, defaultAsk("alice", "dave")) // no parent
	if got := tr.ListByParent(parent); len(got) != 2 {
		t.Fatalf("ListByParent = 2, got %d", len(got))
	}
}
