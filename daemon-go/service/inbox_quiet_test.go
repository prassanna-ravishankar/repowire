package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

var london, _ = time.LoadLocation("Europe/London")

// quietFixture is an inbox whose telegram surface can be held (inbox_hold)
// with quiet hours 22:00 to 07:00 London, on a clock the test moves.
type quietFixture struct {
	*inboxFixture
	mu  sync.Mutex
	clk time.Time
}

func newQuietFixture(t *testing.T, digest bool, bypass []string, hold bool) *quietFixture {
	t.Helper()
	cfg := config.HumanConfig{Name: "prass", Surfaces: map[string]config.HumanSurfaceConfig{
		"telegram": {QuietHours: &config.QuietHours{Start: "22:00", End: "07:00", Timezone: "Europe/London", Bypass: bypass}, Digest: digest},
	}}
	f := &quietFixture{inboxFixture: newInboxFixture(t, cfg)}
	if hold {
		f.telegram.Metadata["capabilities"] = []string{proto.CapInboxSurface, proto.CapInboxHold}
	}
	f.at(23, 0)
	f.inbox.WithClock(func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.clk })
	return f
}

func (f *quietFixture) at(h, m int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	day := 25
	if h < 12 {
		day = 26 // the morning after
	}
	f.clk = time.Date(2026, 11, day, h, m, 0, 0, london)
}

func (f *quietFixture) store(t *testing.T, params InboxStoreParams) string {
	t.Helper()
	if params.Kind == "" {
		params.Kind = InboxNotify
	}
	params.FromPeerID, params.FromName = string(f.alpha.PeerID), "alpha"
	receipt, err := f.inbox.Store(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	f.settle()
	return receipt.ItemID
}

// framesOf lists telegram's frames of one type, in order.
func (f *quietFixture) framesOf(typ proto.FrameType) []map[string]any {
	var out []map[string]any
	for _, frame := range f.transport.framesFor(f.telegram.PeerID) {
		if frame["type"] == string(typ) {
			out = append(out, frame)
		}
	}
	return out
}

func (f *quietFixture) pushedIDs() []string {
	var ids []string
	for _, frame := range f.framesOf(proto.FrameInboxItem) {
		ids = append(ids, frame["delivery_id"].(string))
	}
	return ids
}

func (f *quietFixture) status(t *testing.T, id string) string {
	t.Helper()
	item, err := f.inbox.store.InboxItem(context.Background(), id)
	if err != nil || item == nil {
		t.Fatalf("item %s: %v", id, err)
	}
	for _, p := range item.Pushes {
		if p.Surface == "telegram" {
			return p.Status
		}
	}
	return ""
}

func (f *quietFixture) release() {
	f.inbox.SurfaceRelease(f.telegram)
	f.settle()
}

// In the window an item is held, not pushed, and the surface is told when
// the window ends, as an absolute UTC instant.
func TestQuietHoursHoldAndTellTheSurface(t *testing.T) {
	f := newQuietFixture(t, false, nil, true)
	id := f.store(t, InboxStoreParams{Text: "build is green"})
	if ids := f.pushedIDs(); len(ids) != 0 {
		t.Fatalf("pushed in quiet hours: %v", ids)
	}
	if got := f.status(t, id); got != state.PushHeld {
		t.Fatalf("push status = %q, want held", got)
	}
	holds := f.framesOf(proto.FrameSurfaceHold)
	want := time.Date(2026, 11, 26, 7, 0, 0, 0, london).UTC().Format(time.RFC3339)
	if len(holds) != 1 || holds[0]["release_at"] != want || holds[0]["held"] != 1 {
		t.Fatalf("surface_hold = %v, want held 1, release_at %s", holds, want)
	}
	// slack has no window: pushed as before.
	if len(f.transport.framesFor(f.slack.PeerID)) != 1 {
		t.Fatalf("slack frames = %v", f.transport.framesFor(f.slack.PeerID))
	}
}

// A blocking question is pushed even in the window (holding it would let its
// deadline decide), read from the question itself; an ask is held unless
// "ask" is a bypass class.
func TestQuietHoursBypass(t *testing.T) {
	f := newQuietFixture(t, false, nil, true)
	blocking := f.store(t, InboxStoreParams{Kind: InboxAsk, Text: "approve Bash?", Question: map[string]any{"kind": "choice", "scope": "tool_permission"}})
	deadline := f.store(t, InboxStoreParams{Kind: InboxAsk, Text: "deploy?", Question: map[string]any{"kind": "choice", "blocking": true}})
	ask := f.store(t, InboxStoreParams{Kind: InboxAsk, Text: "which name?"})
	if got := strings.Join(f.pushedIDs(), ","); got != blocking+","+deadline {
		t.Fatalf("pushed %s, want the two blocking questions", got)
	}
	if f.status(t, ask) != state.PushHeld {
		t.Fatal("a plain ask was not held")
	}

	g := newQuietFixture(t, false, []string{config.BypassBlocking, config.BypassAsk}, true)
	if id := g.store(t, InboxStoreParams{Kind: InboxAsk, Text: "which name?"}); g.status(t, id) != state.PushPushed {
		t.Fatal("an ask was held although ask is a bypass class")
	}
	h := newQuietFixture(t, false, []string{}, true)
	if id := h.store(t, InboxStoreParams{Kind: InboxAsk, Text: "approve?", Question: map[string]any{"blocking": true}}); h.status(t, id) != state.PushHeld {
		t.Fatal("an empty bypass list must hold even a blocking question")
	}
}

// surface_release is not trusted: in the window nothing is released and the
// hold is announced again; after it the backlog goes, once.
func TestSurfaceReleaseChecksTheDaemonsOwnWindow(t *testing.T) {
	f := newQuietFixture(t, false, nil, true)
	id := f.store(t, InboxStoreParams{Text: "nightly report"})
	f.at(23, 30)
	f.release()
	if ids := f.pushedIDs(); len(ids) != 0 {
		t.Fatalf("an early surface_release pushed %v", ids)
	}
	if holds := f.framesOf(proto.FrameSurfaceHold); len(holds) != 2 {
		t.Fatalf("surface_hold frames = %d, want the hold announced again", len(holds))
	}
	f.at(7, 1)
	f.release()
	f.release() // idempotent
	if ids := f.pushedIDs(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("after the window: pushed %v, want %s once", ids, id)
	}
	if f.status(t, id) != state.PushPushed {
		t.Fatal("a released item's record is not pushed")
	}
}

// The next Store outside the window releases the backlog before the new item.
func TestStoreReleasesTheBacklogFirst(t *testing.T) {
	f := newQuietFixture(t, false, nil, true)
	held := f.store(t, InboxStoreParams{Text: "first"})
	f.at(8, 0)
	fresh := f.store(t, InboxStoreParams{Text: "second"})
	if got := strings.Join(f.pushedIDs(), ","); got != held+","+fresh {
		t.Fatalf("pushed %s, want the held item before the new one", got)
	}
}

// A reconnect re-announces the hold inside the window and releases after it.
func TestReconnectReleasesOrReannounces(t *testing.T) {
	f := newQuietFixture(t, false, nil, true)
	id := f.store(t, InboxStoreParams{Text: "ping"})
	f.inbox.CatchUp(f.telegram, f.clk)
	f.settle()
	if len(f.pushedIDs()) != 0 || len(f.framesOf(proto.FrameSurfaceHold)) != 2 {
		t.Fatalf("reconnect in the window: pushed %v, holds %d", f.pushedIDs(), len(f.framesOf(proto.FrameSurfaceHold)))
	}
	f.at(9, 0)
	f.inbox.CatchUp(f.telegram, f.clk)
	f.settle()
	if ids := f.pushedIDs(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("reconnect after the window: pushed %v", ids)
	}
}

// An item the human already handled is skipped at release, not pushed.
func TestReleaseSkipsHandledItems(t *testing.T) {
	f := newQuietFixture(t, false, nil, true)
	read := f.store(t, InboxStoreParams{Text: "seen on the dashboard"})
	stale := f.store(t, InboxStoreParams{Kind: InboxAsk, Text: "an ask nobody holds"}) // no open ask: not answerable
	item, _ := f.inbox.store.InboxItem(context.Background(), read)
	if err := f.inbox.MarkRead(context.Background(), item, "dashboard"); err != nil {
		t.Fatal(err)
	}
	f.at(7, 30)
	f.release()
	if ids := f.pushedIDs(); len(ids) != 0 {
		t.Fatalf("pushed handled items: %v", ids)
	}
	if f.status(t, read) != state.PushSkipped || f.status(t, stale) != state.PushSkipped {
		t.Fatalf("statuses %q / %q, want skipped", f.status(t, read), f.status(t, stale))
	}
}

// With digest on, held items other than asks go as one message, and asks
// still go one by one; the digest's refs map to no item.
func TestDigestRelease(t *testing.T) {
	f := newQuietFixture(t, true, nil, true)
	var notes []string
	for _, text := range []string{"one", "two", "three"} {
		notes = append(notes, f.store(t, InboxStoreParams{Text: text}))
	}
	cid, _ := f.asks.Register(context.Background(), RegisterAskParams{FromPeerID: f.alpha.PeerID, ToPeerID: f.human.PeerID, Text: "merge?"})
	ask := f.store(t, InboxStoreParams{Kind: InboxAsk, Text: "merge?", CorrelationID: cid})
	f.at(7, 5)
	f.release()
	frames := f.framesOf(proto.FrameInboxItem)
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want one digest and one ask", len(frames))
	}
	digest, _ := frames[0]["item"].(map[string]any)
	text, _ := digest["text"].(string)
	if !strings.HasPrefix(frames[0]["delivery_id"].(string), "digest-") || !strings.Contains(text, "3 messages") || !strings.Contains(text, "two") || !strings.Contains(text, "reply on an item") {
		t.Fatalf("digest frame = %v", frames[0])
	}
	if frames[1]["delivery_id"] != ask {
		t.Fatalf("second frame = %v, want the ask itself", frames[1]["delivery_id"])
	}
	for _, id := range notes {
		item, _ := f.inbox.store.InboxItem(context.Background(), id)
		if f.status(t, id) != state.PushPushed || item.Pushes[0].SurfaceRef != "" {
			t.Fatalf("digest member %s: %+v (pushed, with no ref a reply could thread to)", id, item.Pushes)
		}
	}
}

// Every release trigger at once pushes a held item exactly once.
func TestRacingReleasesPushOnce(t *testing.T) {
	f := newQuietFixture(t, false, nil, true)
	id := f.store(t, InboxStoreParams{Text: "once"})
	f.at(7, 10)
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); f.inbox.SurfaceRelease(f.telegram) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); f.inbox.CatchUp(f.telegram, f.clk) }()
	wg.Wait()
	f.settle()
	count := 0
	for _, pushed := range f.pushedIDs() {
		if pushed == id {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("pushed %d times", count)
	}
}

// A release the daemon stopped in the middle of is held again at startup
// and released by the next trigger.
func TestRecoverReleasesAfterAStop(t *testing.T) {
	f := newQuietFixture(t, false, nil, true)
	id := f.store(t, InboxStoreParams{Text: "interrupted"})
	if ok, err := f.inbox.store.ClaimHeldInboxPush(context.Background(), id, "telegram"); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	f.inbox.RecoverReleases(context.Background())
	if f.status(t, id) != state.PushHeld {
		t.Fatalf("status after recovery = %q", f.status(t, id))
	}
	f.at(7, 0)
	f.release()
	if ids := f.pushedIDs(); len(ids) != 1 {
		t.Fatalf("pushed %v after recovery", ids)
	}
}

// A surface that cannot be told when to come back (no inbox_hold) is never
// held: pushed as before, quiet hours or not.
func TestSurfaceWithoutInboxHoldIsNeverHeld(t *testing.T) {
	f := newQuietFixture(t, false, nil, false)
	id := f.store(t, InboxStoreParams{Text: "late"})
	if f.status(t, id) != state.PushPushed || len(f.framesOf(proto.FrameSurfaceHold)) != 0 {
		t.Fatalf("status %q, holds %d", f.status(t, id), len(f.framesOf(proto.FrameSurfaceHold)))
	}
}

// The human peer's surfaces say whether a surface is quiet and how much waits.
func TestPresenceReportsQuietAndHeld(t *testing.T) {
	f := newQuietFixture(t, false, nil, true)
	f.store(t, InboxStoreParams{Text: "a"})
	f.store(t, InboxStoreParams{Text: "b"})
	surfaces, _ := f.human.Metadata["surfaces"].(map[string]any)
	tg, _ := surfaces["telegram"].(map[string]any)
	if tg["quiet"] != true || tg["held"] != 2 {
		t.Fatalf("telegram presence = %v", tg)
	}
}

// A surface that reconnects inside its window is held what it missed, as a
// live Store would have held it.
func TestReconnectInTheWindowHoldsWhatItMissed(t *testing.T) {
	f := newQuietFixture(t, false, nil, true)
	f.transport.mu.Lock()
	f.transport.connected[f.telegram.PeerID] = false
	f.transport.mu.Unlock()
	id := f.store(t, InboxStoreParams{Text: "sent while the bridge was down"})
	f.transport.mu.Lock()
	f.transport.connected[f.telegram.PeerID] = true
	f.transport.mu.Unlock()
	f.at(23, 30)
	f.inbox.CatchUp(f.telegram, f.clk.Add(-time.Hour))
	f.settle()
	if ids := f.pushedIDs(); len(ids) != 0 || f.status(t, id) != state.PushHeld {
		t.Fatalf("reconnect in the window pushed %v (status %q), want held", ids, f.status(t, id))
	}
}

// A release whose push fails returns the item to held, and the next trigger
// delivers it: a failure never strands an item the human did not see.
func TestFailedReleaseIsHeldAgain(t *testing.T) {
	f := newQuietFixture(t, false, nil, true)
	id := f.store(t, InboxStoreParams{Text: "retry me"})
	f.transport.mu.Lock()
	f.transport.receipts[f.telegram.PeerID] = func(map[string]any) map[string]any {
		return map[string]any{"status": "failed", "error_class": "network"}
	}
	f.transport.mu.Unlock()
	f.at(7, 0)
	f.release()
	if got := f.status(t, id); got != state.PushHeld {
		t.Fatalf("after a failed release the status is %q, want held", got)
	}
	f.transport.mu.Lock()
	f.transport.receipts[f.telegram.PeerID] = func(map[string]any) map[string]any {
		return map[string]any{"status": "pushed", "surface_ref": "777"}
	}
	f.transport.mu.Unlock()
	f.release()
	if got := f.status(t, id); got != state.PushPushed {
		t.Fatalf("the retry left status %q", got)
	}
}
