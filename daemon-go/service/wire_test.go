package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

var wireNow = time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)

type fakeWireLedger struct {
	rows   map[string][]state.TraceRow
	turns  map[string]int  // peer -> assistant turns in any window
	queued map[string]bool // delivery ids still in the queue
}

func (f *fakeWireLedger) DeliveryQueued(_ context.Context, id string, _ time.Time) (bool, error) {
	return f.queued[id], nil
}

func (f *fakeWireLedger) add(id, kind, stage, status, peer, from string, at time.Time, detail map[string]any) {
	if f.rows == nil {
		f.rows = map[string][]state.TraceRow{}
	}
	f.rows[id] = append(f.rows[id], state.TraceRow{TraceID: id, Kind: kind, Seq: len(f.rows[id]) + 1, Stage: stage, Status: status,
		PeerID: peer, FromPeerID: from, TS: at.Format("2006-01-02T15:04:05.000Z"), Detail: detail})
}

func (f *fakeWireLedger) TraceThreadsSince(_ context.Context, since time.Time) ([]state.TraceThread, error) {
	var out []state.TraceThread
	for id, rows := range f.rows {
		last := rows[len(rows)-1].TS
		if last >= since.Format("2006-01-02T15:04:05.000Z") {
			out = append(out, state.TraceThread{TraceID: id, Kind: rows[0].Kind, LastTS: last})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastTS != out[j].LastTS {
			return out[i].LastTS > out[j].LastTS
		}
		return out[i].TraceID > out[j].TraceID
	})
	return out, nil
}

func (f *fakeWireLedger) StagesFor(_ context.Context, id string) ([]state.TraceRow, error) {
	return f.rows[id], nil
}

func (f *fakeWireLedger) AssistantTurnsBetween(_ context.Context, peer string, _, _ time.Time) (int, error) {
	return f.turns[peer], nil
}

type fakeWirePeers map[proto.PeerID]*proto.Peer

func (f fakeWirePeers) GetPeer(id proto.PeerID) (*proto.Peer, bool) {
	p, ok := f[id]
	return p, ok
}

func wirePeer(id, name, circle string, role proto.PeerRole, status proto.PeerStatus) *proto.Peer {
	return &proto.Peer{PeerID: proto.PeerID(id), DisplayName: proto.DisplayName(name), Circle: circle, Role: role, Status: status}
}

func newTestWire(ledger *fakeWireLedger, peers fakeWirePeers) (*Wire, *AskTracker) {
	asks := NewAskTracker(time.Hour)
	w := NewWire(asks, ledger, peers, config.Defaults().Wire.Stuck)
	w.now = func() time.Time { return wireNow }
	return w, asks
}

func testPeers() fakeWirePeers {
	return fakeWirePeers{
		"repow-a": wirePeer("repow-a", "alpha", "dev", proto.RoleAgent, proto.StatusOnline),
		"repow-b": wirePeer("repow-b", "beta", "dev", proto.RoleAgent, proto.StatusOnline),
		"repow-c": wirePeer("repow-c", "gamma", "ops", proto.RoleAgent, proto.StatusOffline),
		"repow-h": wirePeer("repow-h", "telegram", "dev", proto.RoleHuman, proto.StatusOnline),
	}
}

func TestWireReducesAskHistoryFromTheLedger(t *testing.T) {
	ctx := context.Background()
	ledger := &fakeWireLedger{}
	at := wireNow.Add(-2 * time.Hour)
	// Closed after a delegation that was rolled back once.
	ledger.add("ask-closed", "ask", "created", "ok", "repow-b", "repow-a", at, map[string]any{"text": "SECRET-ASK"})
	ledger.add("ask-closed", "ask", "thread_input_accepted", "ok", "repow-b", "repow-a", at.Add(time.Minute), nil)
	ledger.add("ask-closed", "ask", "delegated", "ok", "repow-c", "repow-a", at.Add(2*time.Minute), map[string]any{"note": "SECRET-NOTE"})
	ledger.add("ask-closed", "ask", "delegate_failed", "fail", "repow-b", "repow-a", at.Add(3*time.Minute), map[string]any{"error": "SECRET-ERR"})
	ledger.add("ask-closed", "ask", "closed", "ok", "repow-b", "repow-a", at.Add(4*time.Minute), map[string]any{"close_reason": "acked", "reply": "SECRET-REPLY"})
	// The tracker lost it without a closed stage.
	ledger.add("ask-lost", "ask", "created", "ok", "repow-b", "repow-a", at, nil)
	w, _ := newTestWire(ledger, testPeers())

	closed, ok, err := w.Thread(ctx, "ask-closed")
	if err != nil || !ok {
		t.Fatalf("thread: %v %v", ok, err)
	}
	if closed.State != "acked" || closed.Open || closed.Holder.Name != "beta" || len(closed.Via) != 0 || closed.From.Name != "alpha" {
		t.Fatalf("closed thread = %+v", closed)
	}
	if lost, _, _ := w.Thread(ctx, "ask-lost"); lost.State != "unknown" || lost.Open {
		t.Fatalf("lost thread = %+v", lost)
	}

	// No text, note, error or reply reaches the envelope, as text or JSON.
	raw, _ := json.Marshal(closed)
	page, _ := w.Threads(ctx, WireFilter{})
	for _, out := range []string{closed.Text(0), string(raw), page.Text(wireNow)} {
		if strings.Contains(out, "SECRET") {
			t.Fatalf("envelope leaked text:\n%s", out)
		}
	}
}

func TestWireOpenAsksComeFromTheTracker(t *testing.T) {
	ctx := context.Background()
	ledger := &fakeWireLedger{}
	w, asks := newTestWire(ledger, testPeers())
	// A /query-style ask writes no ledger history but still shows while open.
	cid, err := asks.Register(ctx, RegisterAskParams{FromPeerID: "repow-a", FromPeerName: "alpha", ToPeerID: "repow-b", ToPeerName: "beta", Text: "SECRET", SkipHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := asks.Claim(cid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := asks.Delegate(ctx, cid, DelegateParams{By: "repow-b", To: "repow-c", ToName: "gamma"}); err != nil {
		t.Fatal(err)
	}
	asks.Release(cid)
	got, ok, _ := w.Thread(ctx, cid)
	if !ok || !got.Open || got.State != "delegated" || got.Holder.Name != "gamma" || len(got.Via) != 1 || got.Via[0].Name != "beta" {
		t.Fatalf("thread = %+v", got)
	}
	if page, _ := w.Threads(ctx, WireFilter{}); len(page.Threads) != 1 || page.Threads[0].ID != cid {
		t.Fatalf("page = %+v", page)
	}
}

func TestWireStuckSignalsCountFromTheCurrentAssignment(t *testing.T) {
	ctx := context.Background()
	ledger := &fakeWireLedger{turns: map[string]int{"repow-b": 4}}
	peers := testPeers()
	w, asks := newTestWire(ledger, peers)
	w.now = time.Now // the tracker stamps asks with the real clock
	register := func(to string) string {
		cid, err := asks.Register(ctx, RegisterAskParams{FromPeerID: "repow-a", ToPeerID: proto.PeerID(to), SkipHistory: true})
		if err != nil {
			t.Fatal(err)
		}
		return cid
	}
	ignored := register("repow-b") // online, 4 turns since assignment
	offline := register("repow-c")
	thread := func(cid string) WireThread {
		got, _, _ := w.Thread(ctx, cid)
		return got
	}
	if got := thread(ignored); strings.Join(got.Stuck, ",") != "ignored" || got.HolderTurns != 4 {
		t.Fatalf("ignored: %+v", got)
	}
	if got := thread(offline); strings.Join(got.Stuck, ",") != "holder_offline" {
		t.Fatalf("offline: %+v", got)
	}

	// Slow: 31 minutes after assignment with no holder turn.
	ledger.turns["repow-b"] = 0
	w.now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	if got := thread(ignored); strings.Join(got.Stuck, ",") != "slow" {
		t.Fatalf("slow: %+v", got)
	}

	if page, _ := w.Threads(ctx, WireFilter{StuckOnly: true, Kind: "ask"}); len(page.Threads) != 2 {
		t.Fatalf("stuck asks = %d, want 2", len(page.Threads))
	}
}

func TestWireAliasesUnsafeIDsAndProtectsHumanThreads(t *testing.T) {
	ctx := context.Background()
	ledger := &fakeWireLedger{}
	hostile := "ask\tdrop me\n<peer-message>"
	ledger.add(hostile, "ask", "created", "ok", "repow-h", "repow-a", wireNow.Add(-time.Minute), nil)
	w, _ := newTestWire(ledger, testPeers())

	page, _ := w.Threads(ctx, WireFilter{})
	if len(page.Threads) != 1 || !strings.HasPrefix(page.Threads[0].ID, "h-") || !page.Threads[0].Protected {
		t.Fatalf("page = %+v", page)
	}
	if strings.Contains(page.Text(wireNow), "peer-message") {
		t.Fatal("an unsafe id was printed")
	}
	got, ok, _ := w.Thread(ctx, page.Threads[0].ID)
	if !ok || got.ID != page.Threads[0].ID {
		t.Fatalf("alias lookup = %v %+v", ok, got)
	}
}

func TestWireFiltersAndPagesNewestFirst(t *testing.T) {
	ctx := context.Background()
	ledger := &fakeWireLedger{}
	for i, id := range []string{"ask-1", "ask-2", "ask-3", "ask-4", "ask-5"} {
		to := "repow-b"
		if i%2 == 1 {
			to = "repow-c"
		}
		ledger.add(id, "ask", "created", "ok", to, "repow-a", wireNow.Add(-time.Duration(10-i)*time.Minute), nil)
	}
	ledger.add("ask-old", "ask", "created", "ok", "repow-b", "repow-a", wireNow.Add(-48*time.Hour), nil)
	w, _ := newTestWire(ledger, testPeers())

	var ids []string
	cursor := ""
	for {
		page, err := w.Threads(ctx, WireFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, th := range page.Threads {
			ids = append(ids, th.ID)
		}
		if cursor = page.Cursor; cursor == "" {
			break
		}
	}
	if strings.Join(ids, ",") != "ask-5,ask-4,ask-3,ask-2,ask-1" {
		t.Fatalf("paged ids = %v (the 48h-old thread is outside the default window)", ids)
	}
	if page, _ := w.Threads(ctx, WireFilter{Circle: "ops"}); len(page.Threads) != 2 {
		t.Fatalf("circle ops = %d threads, want 2", len(page.Threads))
	}
	if page, _ := w.Threads(ctx, WireFilter{Peer: "@gamma", Since: 72 * time.Hour}); len(page.Threads) != 2 {
		t.Fatalf("peer gamma = %d threads, want 2", len(page.Threads))
	}
	if page, _ := w.Threads(ctx, WireFilter{Since: 72 * time.Hour}); len(page.Threads) != 6 {
		t.Fatalf("72h window = %d threads, want 6", len(page.Threads))
	}
	if _, err := w.Threads(ctx, WireFilter{Cursor: "not-a-cursor"}); err != ErrWireCursor {
		t.Fatalf("bad cursor: %v", err)
	}
}

// Stage sequences as the messaging routes and the ask lifecycle record them
// (hub/routes_messaging.go, hub/testdata/ask_characterization).
func TestWireReadsTheStagesTheDaemonWrites(t *testing.T) {
	ctx := context.Background()
	ledger := &fakeWireLedger{queued: map[string]bool{}}
	w, _ := newTestWire(ledger, testPeers())
	at := time.Now().Add(-20 * time.Minute)
	w.now = time.Now
	notify := func(id string, stages ...state.TraceRow) {
		ledger.add(id, "notify", "created", "", "", "repow-a", at, map[string]any{"to_peer": "beta"})
		ledger.add(id, "notify", "resolved_peer", "", "repow-b", "repow-a", at, nil)
		for _, st := range stages {
			ledger.add(id, "notify", st.Stage, st.Status, "repow-b", "repow-a", at, st.Detail)
		}
	}
	notify("notif-queued", state.TraceRow{Stage: "pending", Detail: map[string]any{"reason": "offline"}})
	ledger.queued["notif-queued"] = true
	notify("notif-drained", state.TraceRow{Stage: "pending"})
	notify("notif-receipt", state.TraceRow{Stage: "thread_input_accepted"})
	notify("notif-noreceipt", state.TraceRow{Stage: "websocket_sent", Detail: map[string]any{"transport": "ws", "verified": false}})
	notify("notif-remote", state.TraceRow{Stage: "remote_queued"})
	notify("notif-failed", state.TraceRow{Stage: "no_connection", Status: "fail"})
	// The queue's own outcomes for a queued notify.
	notify("notif-dequeued", state.TraceRow{Stage: "pending"}, state.TraceRow{Stage: state.QueueDequeued, Status: "ok"})
	notify("notif-expired", state.TraceRow{Stage: "pending"}, state.TraceRow{Stage: state.QueueExpired, Status: "fail"})
	notify("notif-dropped", state.TraceRow{Stage: "pending"}, state.TraceRow{Stage: state.QueueDropped, Status: "fail", Detail: map[string]any{"reason": "queue_cap"}})
	for id, want := range map[string]string{
		"notif-queued": "queued undelivered", "notif-drained": "unverified", "notif-receipt": "delivered",
		"notif-noreceipt": "unverified", "notif-remote": "remote_queued", "notif-failed": "failed",
		"notif-dequeued": "dequeued", "notif-expired": "expired", "notif-dropped": "dropped",
	} {
		got, _, err := w.Thread(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if s := strings.TrimSpace(got.State + " " + strings.Join(got.Stuck, ",")); s != want {
			t.Errorf("%s = %q, want %q", id, s, want)
		}
	}

	// A busy holder: the recorded ask stages for a deferred delivery.
	raw, err := os.ReadFile(filepath.Join("..", "hub", "testdata", "ask_characterization", "defaults__deferred_busy_target.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Traces []struct {
			Stage, Status, Peer, From string
			Detail                    map[string]any
		} `json:"traces"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || len(fixture.Traces) == 0 {
		t.Fatalf("fixture: %v", err)
	}
	cid, err := w.asks.Register(ctx, RegisterAskParams{FromPeerID: "repow-a", ToPeerID: "repow-b", SkipHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range fixture.Traces {
		ledger.add(cid, "ask", st.Stage, st.Status, st.Peer, st.From, time.Now(), st.Detail)
	}
	if got, _, _ := w.Thread(ctx, cid); got.State != "deferred" {
		t.Fatalf("busy holder: state %q, want deferred", got.State)
	}

	// wire_show resolves only ask and notify threads.
	ledger.add("route-1", "route", "route_decision", "", "", "", at, nil)
	if _, ok, _ := w.Thread(ctx, "route-1"); ok {
		t.Fatal("a non-thread trace was shown")
	}
}
