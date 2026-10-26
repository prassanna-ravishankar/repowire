package state

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func remoteStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func inbound(id string) RemoteInbound {
	return RemoteInbound{ID: id, Kind: "ask", FromMachine: "laptop", From: RemotePrincipal{PeerID: "p1", DisplayName: "alice", Circle: "proj"}, ToPeerID: "t1", Text: "hi", CreatedAt: 1000}
}

func TestStoreEpochIsStable(t *testing.T) {
	s := remoteStore(t)
	a, _ := s.StoreEpoch(context.Background())
	b, _ := s.StoreEpoch(context.Background())
	if a == "" || a != b {
		t.Fatalf("epochs %q %q", a, b)
	}
}

func TestCreateInboundIsIdempotentAndRejectsADifferentEnvelope(t *testing.T) {
	s, ctx := remoteStore(t), context.Background()
	first, created, err := s.CreateInbound(ctx, inbound("ask-1"))
	if err != nil || !created || first.State != RemoteDispatching {
		t.Fatalf("first %+v %v %v", first, created, err)
	}
	again, created, err := s.CreateInbound(ctx, inbound("ask-1"))
	if err != nil || created || again.State != RemoteDispatching {
		t.Fatalf("retry %+v %v %v", again, created, err)
	}
	for _, change := range []func(*RemoteInbound){
		func(r *RemoteInbound) { r.Text = "other" },
		func(r *RemoteInbound) { r.Kind = "notify" },
		func(r *RemoteInbound) { r.From.Circle = "elsewhere" },
		func(r *RemoteInbound) { r.FromMachine = "desktop" },
	} {
		r := inbound("ask-1")
		change(&r)
		if _, _, err := s.CreateInbound(ctx, r); !errors.Is(err, ErrRemoteConflict) {
			t.Fatalf("changed envelope: %v", err)
		}
	}
}

func TestReadFencesAnUnknownIDAndTheLateCreateDispatchesNothing(t *testing.T) {
	s, ctx := remoteStore(t), context.Background()
	got, err := s.ReadInbound(ctx, "ask-2", "laptop", "p1", 1000, 500)
	if err != nil || got.State != RemoteSettled || got.CloseReason != "not_delivered" {
		t.Fatalf("fence %+v %v", got, err)
	}
	late, created, err := s.CreateInbound(ctx, inbound("ask-2"))
	if err != nil || created || late.CloseReason != "not_delivered" {
		t.Fatalf("late create %+v %v %v", late, created, err)
	}
	if _, err := s.ReadInbound(ctx, "ask-2", "laptop", "someone-else", 1000, 500); !errors.Is(err, ErrRemoteNotFound) {
		t.Fatalf("fence read by another asker: %v", err)
	}
	if _, err := s.ReadInbound(ctx, "ask-3", "laptop", "p1", 400, 500); !errors.Is(err, ErrRemoteExpired) {
		t.Fatalf("past the horizon: %v", err)
	}
}

func TestReadInboundOnlyByTheRecordedOrigin(t *testing.T) {
	s, ctx := remoteStore(t), context.Background()
	_, _, _ = s.CreateInbound(ctx, inbound("ask-4"))
	for _, origin := range [][2]string{{"desktop", "p1"}, {"laptop", "p2"}} {
		if _, err := s.ReadInbound(ctx, "ask-4", origin[0], origin[1], 1000, 500); !errors.Is(err, ErrRemoteNotFound) {
			t.Fatalf("%v read: %v", origin, err)
		}
	}
}

// A create and a fencing read racing on one id: exactly one wins, and the
// other sees its result.
func TestCreateAndFenceExcludeEachOther(t *testing.T) {
	s, ctx := remoteStore(t), context.Background()
	for i := range 40 {
		id := fmt.Sprintf("ask-race-%d", i)
		var wg sync.WaitGroup
		var created bool
		var read RemoteInbound
		wg.Add(2)
		go func() { defer wg.Done(); _, created, _ = s.CreateInbound(ctx, inbound(id)) }()
		go func() { defer wg.Done(); read, _ = s.ReadInbound(ctx, id, "laptop", "p1", 1000, 500) }()
		wg.Wait()
		fenced := read.CloseReason == "not_delivered"
		if created == fenced {
			t.Fatalf("%s: created=%v fenced=%v", id, created, fenced)
		}
	}
}

func TestSettlementsAreFirstWins(t *testing.T) {
	s, ctx := remoteStore(t), context.Background()
	_, _, _ = s.CreateInbound(ctx, inbound("ask-5"))
	if ok, _ := s.AdvanceInbound(ctx, "ask-5", RemoteDispatching, RemoteOpen); !ok {
		t.Fatal("advance")
	}
	reply := "42"
	if ok, _ := s.SettleInbound(ctx, "ask-5", "ack_with_msg", &reply, 2000); !ok {
		t.Fatal("first settle")
	}
	if ok, _ := s.SettleInbound(ctx, "ask-5", "evicted", nil, 3000); ok {
		t.Fatal("second settle won")
	}
	if ok, _ := s.AdvanceInbound(ctx, "ask-5", RemoteDispatching, RemoteOpen); ok {
		t.Fatal("advanced a settled record")
	}
	got, _ := s.GetInbound(ctx, "ask-5")
	if got.CloseReason != "ack_with_msg" || *got.Reply != "42" {
		t.Fatalf("got %+v", got)
	}

	out := RemoteOutbound{ID: "ask-6", Machine: "desktop", StoreEpoch: "e", ToPeerID: "t", ToName: "bob", From: inbound("").From, Text: "hi", CreatedAt: 1000}
	if err := s.InsertOutbound(ctx, out); err != nil {
		t.Fatal(err)
	}
	// The settlement push lands before the create response times out.
	if ok, _ := s.SettleOutbound(ctx, "ask-6", "ack_with_msg", &reply); !ok {
		t.Fatal("settle outbound")
	}
	if ok, _ := s.MarkOutboundUncertain(ctx, "ask-6"); ok {
		t.Fatal("late uncertain overwrote a result")
	}
	if ok, _ := s.SettleOutbound(ctx, "ask-6", "not_delivered", nil); ok {
		t.Fatal("second outbound settle won")
	}
	undelivered, _ := s.UndeliveredOutbound(ctx, "p1", 8)
	if len(undelivered) != 1 || undelivered[0].State != RemoteSettled {
		t.Fatalf("undelivered %+v", undelivered)
	}
	_ = s.MarkOutboundDelivered(ctx, "ask-6")
	if undelivered, _ := s.UndeliveredOutbound(ctx, "", 8); len(undelivered) != 0 {
		t.Fatalf("still undelivered %+v", undelivered)
	}
}

func TestOutboundChecksRotateAndRespectTheCutoff(t *testing.T) {
	s, ctx := remoteStore(t), context.Background()
	for i := range 3 {
		_ = s.InsertOutbound(ctx, RemoteOutbound{ID: fmt.Sprintf("ask-c%d", i), Machine: "m", From: RemotePrincipal{PeerID: "p1"}, CreatedAt: int64(1000 + i)})
	}
	first, _ := s.ClaimOutboundChecks(ctx, "p1", 100, 100, 2)
	second, _ := s.ClaimOutboundChecks(ctx, "p1", 100, 101, 2)
	if len(first) != 2 || len(second) != 1 || second[0].ID != "ask-c2" {
		t.Fatalf("first %v second %v", first, second)
	}
	if again, _ := s.ClaimOutboundChecks(ctx, "p1", 100, 102, 2); len(again) != 0 {
		t.Fatalf("rechecked inside the cutoff: %v", again)
	}
}

func TestPruneRemoteDropsRecordsAndFences(t *testing.T) {
	s, ctx := remoteStore(t), context.Background()
	_, _, _ = s.CreateInbound(ctx, inbound("ask-7"))
	_, _ = s.ReadInbound(ctx, "ask-8", "laptop", "p1", 1000, 500)
	if err := s.PruneRemote(ctx, 1001); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetInbound(ctx, "ask-7"); !errors.Is(err, ErrRemoteNotFound) {
		t.Fatalf("record survived: %v", err)
	}
	if _, created, _ := s.CreateInbound(ctx, inbound("ask-8")); !created {
		t.Fatal("fence survived prune")
	}
}
