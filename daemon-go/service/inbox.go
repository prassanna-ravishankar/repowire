package service

import (
	"context"
	"errors"
	"log"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// ============================================================================
// Inbox — the human peer's durable inbox (#466).
//
// Senders address the human; they never learn which surface renders it.
// Store is the only request-path cost: one INSERT plus its trace row. Pushes to
// surfaces (the Telegram bridge today) run on tracked goroutines and report
// back through the ordinary delivery_ack channel, so a slow or unreachable
// phone degrades to a surface_failed trace and never blocks the sender,
// startup, or shutdown.
//
// Trace stages written here, all on the item's own trace (the notify's
// delivery_id, or the ask's correlation_id), never with message text:
// inbox_stored, surface_pushed, surface_failed, inbox_read, inbox_replied.
// ============================================================================

// Inbox item kinds.
const (
	InboxNotify    = "notify"
	InboxAsk       = "ask"
	InboxAnswer    = "answer"
	InboxBroadcast = "broadcast"
)

// inboxPushTimeout bounds how long a push waits for a surface's receipt. The
// Telegram bridge retries transient failures well inside it.
const inboxPushTimeout = 60 * time.Second

// inboxCatchUpLimit bounds how many missed items a reconnecting surface is
// sent; the rest stay readable through the inbox.
const inboxCatchUpLimit = 20

// InboxStore is the persistence seam; *state.Store satisfies it.
type InboxStore interface {
	InsertInboxItem(ctx context.Context, item *state.InboxItem) error
	InboxItem(ctx context.Context, id string) (*state.InboxItem, error)
	InboxSeq(ctx context.Context) (int64, error)
	ListInbox(ctx context.Context, filter state.InboxFilter) ([]state.InboxItem, error)
	RecordInboxPush(ctx context.Context, itemID string, push state.InboxPush) error
	InboxItemBySurfaceRef(ctx context.Context, surface, ref string) (*state.InboxItem, error)
	InboxThreadRef(ctx context.Context, cid, surface string) (string, error)
	MarkInboxRead(ctx context.Context, id, surface string) (bool, error)
	MarkInboxReplied(ctx context.Context, id, surface string) (bool, error)
	RecordTrace(ctx context.Context, traceID, kind, stage, status, deliveryID, peerID, fromPeerID string, detail map[string]any) error
	UnrepliedInboxAsks(ctx context.Context, cid string) ([]state.InboxItem, error)
	HeldInboxItems(ctx context.Context, surface string) ([]state.InboxItem, error)
	HeldInboxCount(ctx context.Context, surface string) (int, error)
	ClaimHeldInboxPush(ctx context.Context, itemID, surface string) (bool, error)
	ResetReleasingInboxPushes(ctx context.Context) (int64, error)
}

// inboxRegistry is the slice of *peer.Registry the inbox reads and annotates.
type inboxRegistry interface {
	GetAllPeers() []*proto.Peer
	AddEvent(ctx context.Context, typ string, payload map[string]any) string
	UpdateMetadataByName(ctx context.Context, identifier string, metadata map[string]any) (bool, error)
}

// LocalSurface is a surface the daemon renders itself, with no peer of its
// own: the dashboard, over its event stream. Push returns once the surface has
// the item or fails; its error is the push's receipt.
type LocalSurface interface {
	Name() string
	Live() bool
	Push(ctx context.Context, item *state.InboxItem) error
}

// InboxReceipt is what a sender learns synchronously: the item was stored and
// which surfaces a push was started on. Per-surface outcomes land in traces.
type InboxReceipt struct {
	ItemID   string
	Surfaces []string
	// Thread is the ask the item was filed under, "" when unthreaded.
	Thread string
}

// InboxStoreParams describes one inbound item.
type InboxStoreParams struct {
	Kind          string
	TraceID       string
	TraceKind     string
	FromPeerID    string
	FromName      string
	CorrelationID string
	// ProgressOn is an ask a notify reports progress on (#477). It becomes
	// the item's thread only when the ask is threadable and the sender asked
	// or holds it; the item stays a notify either way.
	ProgressOn  string
	Text        string
	Attachments []map[string]any
	Question    map[string]any
}

// Inbox owns the human peer's items and their surface pushes.
type Inbox struct {
	store     InboxStore
	reg       inboxRegistry
	transport Transport
	asks      *AskTracker
	cfg       config.HumanConfig

	mu        sync.RWMutex
	human     proto.PeerID
	humanName proto.DisplayName
	local     []LocalSurface

	pushTimeout  time.Duration
	catchUpLimit int
	// liveMu orders Store (insert, pick live surfaces, claim their pushes)
	// against CatchUp's snapshot of what is stored and in flight, so a live
	// push is never mistaken for a missed one.
	liveMu  sync.Mutex
	pushing map[pushKey]struct{} // pushes in flight
	baseCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	closeMu sync.Mutex
	closed  bool

	// settles are AskSettled events waiting for the one drainer, which
	// publishes them in the order the tracker reported them.
	settleMu sync.Mutex
	settles  []AskSettled
	draining bool
	applied  map[string]appliedSettle // per ask, the newest Seq published

	// now is the clock quiet hours read (tests replace it). mayHold marks
	// the surfaces that may have held items, so a Store outside quiet hours
	// looks for a backlog only when one can exist; every surface starts
	// marked, since the store may hold items from before a restart.
	now func() time.Time
	// presenceMu serializes SyncPresence, so a slower call never writes an
	// older view (a stale held count) over a newer one.
	presenceMu sync.Mutex
	holdMu     sync.Mutex
	mayHold    map[string]bool
	cleared    map[string]bool // surfaces whose backlog was looked for and found empty
}

// NewInbox wires the inbox. asks may be nil (no thread validation: items never
// carry a correlation_id then).
func NewInbox(store InboxStore, reg inboxRegistry, transport Transport, asks *AskTracker, cfg config.HumanConfig) *Inbox {
	ctx, cancel := context.WithCancel(context.Background())
	return &Inbox{store: store, reg: reg, transport: transport, asks: asks, cfg: cfg,
		pushTimeout: inboxPushTimeout, catchUpLimit: inboxCatchUpLimit, pushing: map[pushKey]struct{}{},
		baseCtx: ctx, cancel: cancel, now: time.Now, mayHold: map[string]bool{}, cleared: map[string]bool{}}
}

// SetHuman records the registered human peer.
func (i *Inbox) SetHuman(id proto.PeerID, name proto.DisplayName) {
	i.mu.Lock()
	i.human, i.humanName = id, name
	i.mu.Unlock()
}

// AddSurface adds a surface the daemon renders itself.
func (i *Inbox) AddSurface(s LocalSurface) {
	i.mu.Lock()
	i.local = append(i.local, s)
	i.mu.Unlock()
}

func (i *Inbox) localSurfaces() []LocalSurface {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return append([]LocalSurface(nil), i.local...)
}

// Human returns the human peer's id and name ("" before registration).
func (i *Inbox) Human() (proto.PeerID, proto.DisplayName) {
	if i == nil {
		return "", ""
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.human, i.humanName
}

// Owns reports whether p is the human peer whose messages the inbox stores.
func (i *Inbox) Owns(p *proto.Peer) bool {
	id, _ := i.Human()
	return p != nil && id != "" && p.PeerID == id
}

// Store persists one inbound item, records inbox_stored, and starts the
// surface pushes in the background.
func (i *Inbox) Store(ctx context.Context, params InboxStoreParams) (InboxReceipt, error) {
	human, humanName := i.Human()
	if human == "" {
		return InboxReceipt{}, errors.New("inbox: human peer not registered")
	}
	item := &state.InboxItem{
		ID:            state.NewInboxItemID(),
		Direction:     state.InboxIn,
		Kind:          params.Kind,
		TraceID:       params.TraceID,
		TraceKind:     params.TraceKind,
		FromPeerID:    params.FromPeerID,
		FromName:      params.FromName,
		ToPeerID:      string(human),
		ToName:        string(humanName),
		CorrelationID: firstNonEmpty(i.threadable(params.CorrelationID), i.progressThread(params.ProgressOn, params.FromPeerID)),
		Text:          params.Text,
		Attachments:   params.Attachments,
		Question:      params.Question,
	}
	if item.TraceID == "" {
		item.TraceID = item.ID
	}
	if item.TraceKind == "" {
		item.TraceKind = params.Kind
	}
	if item.Kind == InboxAnswer && item.CorrelationID == "" {
		// A reply to a thread the daemon no longer knows is still worth
		// showing, just not threaded.
		item.Kind = InboxNotify
	}
	i.liveMu.Lock()
	if err := i.store.InsertInboxItem(ctx, item); err != nil {
		i.liveMu.Unlock()
		return InboxReceipt{}, err
	}
	var targets []surfaceTarget
	for _, target := range i.pushTargets(item.Kind) {
		if i.claim(item.ID, target.name) {
			targets = append(targets, target)
		}
	}
	i.liveMu.Unlock()
	i.trace(ctx, item, "inbox_stored", "", map[string]any{"item_id": item.ID, "kind": item.Kind})
	i.reg.AddEvent(ctx, "inbox_item", map[string]any{
		"item_id": item.ID, "kind": item.Kind, "from": item.FromName,
		"from_peer_id": item.FromPeerID, "correlation_id": item.CorrelationID,
	})

	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.name)
	}
	snapshot := *item
	now := i.now()
	for _, target := range targets {
		target := target
		hold := i.holds(&snapshot, target, now)
		if !i.spawn(func() {
			if hold {
				i.hold(&snapshot, target, now)
				return
			}
			// Outside its quiet hours, a surface's backlog lands before
			// the new item (the first of the lazy release triggers).
			i.releaseIfDue(target, now)
			i.push(&snapshot, target)
		}) {
			i.release(item.ID, target.name)
		}
	}
	return InboxReceipt{ItemID: item.ID, Surfaces: names, Thread: item.CorrelationID}, nil
}

// RecordOutbound stores what the human sent (an ask typed on a surface) so the
// recipient's reply can thread against it. ref is the surface's own message
// id, when the surface has one.
func (i *Inbox) RecordOutbound(ctx context.Context, cid string, to *proto.Peer, text, surface, ref string) {
	human, humanName := i.Human()
	if human == "" || to == nil {
		return
	}
	item := &state.InboxItem{
		Direction: state.InboxOut, Kind: InboxAsk, TraceID: cid, TraceKind: "ask",
		FromPeerID: string(human), FromName: string(humanName),
		ToPeerID: string(to.PeerID), ToName: string(to.DisplayName),
		CorrelationID: cid, Text: text,
	}
	if err := i.store.InsertInboxItem(ctx, item); err != nil {
		log.Printf("inbox: record outbound %s: %v", cid, err)
		return
	}
	if surface != "" && ref != "" {
		if err := i.store.RecordInboxPush(ctx, item.ID, state.InboxPush{Surface: surface, Status: state.PushPushed, SurfaceRef: ref}); err != nil {
			log.Printf("inbox: record outbound ref %s: %v", cid, err)
		}
	}
}

// Get returns one item ((nil, nil) when unknown).
func (i *Inbox) Get(ctx context.Context, id string) (*state.InboxItem, error) {
	return i.store.InboxItem(ctx, id)
}

// List returns inbound items, newest first.
func (i *Inbox) List(ctx context.Context, filter state.InboxFilter) ([]state.InboxItem, error) {
	return i.store.ListInbox(ctx, filter)
}

// BySurfaceRef finds the item a surface rendered as ref.
func (i *Inbox) BySurfaceRef(ctx context.Context, surface, ref string) (*state.InboxItem, error) {
	return i.store.InboxItemBySurfaceRef(ctx, surface, ref)
}

// MarkRead marks the item read for every surface; repeat calls are no-ops.
func (i *Inbox) MarkRead(ctx context.Context, item *state.InboxItem, surface string) error {
	changed, err := i.store.MarkInboxRead(ctx, item.ID, surface)
	if err != nil || !changed {
		return err
	}
	i.trace(ctx, item, "inbox_read", "", map[string]any{"item_id": item.ID, "surface": surface})
	i.reg.AddEvent(ctx, "inbox_updated", map[string]any{"item_id": item.ID, "read": true, "surface": surface})
	return nil
}

// Answerable reports whether the human can still answer an item: an ask the
// tracker holds open with the human as its holder. Anything else (a closed,
// expired or moved ask, or not an ask) takes a reply as a new message.
func (i *Inbox) Answerable(item *state.InboxItem) bool {
	if item == nil || item.Direction != state.InboxIn || item.Kind != InboxAsk || item.CorrelationID == "" || i.asks == nil {
		return false
	}
	human, _ := i.Human()
	ask, ok := i.asks.Get(item.CorrelationID)
	return ok && !ask.Closed && ask.ToPeerID == human
}

// AskSettled is the inbox's plug on the tracker's settle socket: when an ask
// the human holds closes, moves away from the human, or comes back, the
// human's unreplied items for it change answerability. Each is traced and
// published (inbox_updated), so a surface stops offering an answer that the
// daemon would refuse. One drainer on the inbox's own goroutine publishes the
// events in the order the tracker reported them, each with the state that
// event left, so the last update for an item tells the truth.
func (i *Inbox) AskSettled(ev AskSettled) {
	human, _ := i.Human()
	if human == "" || ev.Ask == nil || (ev.Ask.ToPeerID != human && ev.MovedFrom != human) {
		return
	}
	i.settleMu.Lock()
	i.settles = append(i.settles, ev)
	start := !i.draining
	i.draining = true
	i.settleMu.Unlock()
	if start && !i.spawn(i.drainSettles) {
		i.settleMu.Lock()
		i.settles, i.draining = nil, false // closed: nothing publishes any more
		i.settleMu.Unlock()
	}
}

// appliedSettle is the newest event published for an ask. A closed ask's
// entry stays briefly as a tombstone, so an older move or return that reaches
// the inbox after the close cannot publish the ask as answerable again.
type appliedSettle struct {
	seq      uint64
	closedAt time.Time
}

// settleTombstone is how long a closed ask's entry outlives the close: far
// longer than two unlocking goroutines can be apart.
const settleTombstone = time.Minute

func (i *Inbox) drainSettles() {
	human, _ := i.Human()
	for {
		i.settleMu.Lock()
		if len(i.settles) == 0 {
			i.draining = false
			i.settleMu.Unlock()
			return
		}
		ev := i.settles[0]
		i.settles = i.settles[1:]
		cid := ev.Ask.CorrelationID
		now := time.Now()
		for id, a := range i.applied {
			if !a.closedAt.IsZero() && now.Sub(a.closedAt) > settleTombstone {
				delete(i.applied, id)
			}
		}
		stale := ev.Seq <= i.applied[cid].seq
		if !stale {
			if i.applied == nil {
				i.applied = map[string]appliedSettle{}
			}
			a := appliedSettle{seq: ev.Seq}
			if ev.Ask.Closed {
				a.closedAt = now
			}
			i.applied[cid] = a
		}
		i.settleMu.Unlock()
		if stale {
			continue // a newer state of this ask was already published
		}

		ctx := i.baseCtx
		items, err := i.store.UnrepliedInboxAsks(ctx, ev.Ask.CorrelationID)
		if err != nil {
			log.Printf("inbox: settle %s: %v", ev.Ask.CorrelationID, err)
			continue
		}
		answerable := !ev.Ask.Closed && ev.Ask.ToPeerID == human
		for idx := range items {
			item := &items[idx]
			detail := map[string]any{"item_id": item.ID, "answerable": answerable, "reason": ev.Reason, "holder": string(ev.Ask.ToPeerName)}
			i.trace(ctx, item, "inbox_settled", "", detail)
			i.reg.AddEvent(ctx, "inbox_updated", detail)
		}
	}
}

// MarkReplied records that the human answered the item from surface.
func (i *Inbox) MarkReplied(ctx context.Context, item *state.InboxItem, surface string) error {
	changed, err := i.store.MarkInboxReplied(ctx, item.ID, surface)
	if err != nil || !changed {
		return err
	}
	i.trace(ctx, item, "inbox_replied", "", map[string]any{"item_id": item.ID, "surface": surface})
	i.reg.AddEvent(ctx, "inbox_updated", map[string]any{"item_id": item.ID, "read": true, "replied": true, "surface": surface})
	return nil
}

// SyncPresence publishes which surfaces are live on the human peer's
// metadata. Called when a surface connects or drops; never on a timer.
func (i *Inbox) SyncPresence(ctx context.Context) {
	i.presenceMu.Lock()
	defer i.presenceMu.Unlock()
	human, _ := i.Human()
	if human == "" {
		return
	}
	surfaces := map[string]any{}
	live := 0
	for _, p := range i.reg.GetAllPeers() {
		if !proto.IsInboxSurface(p) || p.PeerID == human {
			continue
		}
		connected := i.transport.IsConnected(p.PeerID)
		if connected {
			live++
		}
		entry := map[string]any{"live": connected}
		// Quiet hours, where the surface can be held: whether its window is
		// on now, and how many items wait for it (#476).
		if w, ok := i.window(surfaceTarget{name: string(p.DisplayName), peer: p}); ok {
			now := i.now()
			entry["quiet"] = w.Quiet(now)
			if w.Quiet(now) {
				entry["quiet_until"] = w.End(now).Format(time.RFC3339)
			}
			if n, err := i.store.HeldInboxCount(ctx, string(p.DisplayName)); err == nil {
				entry["held"] = n
			}
		}
		surfaces[string(p.DisplayName)] = entry
	}
	for _, s := range i.localSurfaces() {
		if s.Live() {
			live++
		}
		surfaces[s.Name()] = map[string]any{"live": s.Live()}
	}
	_, _ = i.reg.UpdateMetadataByName(ctx, string(human), map[string]any{"surfaces": surfaces, "surfaces_live": live})
}

// CatchUp pushes a surface that just connected the unread items it missed
// while it was offline: those stored so far with no push record for it and
// no live push in flight to it, no older than the newest item it has a record
// for written before liveSince (when it went live again: a live push recorded
// after that never moves the boundary). Only the newest catchUpLimit go,
// oldest first, so a long outage neither floods the surface nor trickles
// stale items in on later connects. Called after the surface's Connect, never
// on a timer.
func (i *Inbox) CatchUp(p *proto.Peer, liveSince time.Time) {
	human, _ := i.Human()
	if human == "" || p == nil || p.PeerID == human || !proto.IsInboxSurface(p) {
		return
	}
	target := surfaceTarget{name: string(p.DisplayName), peer: p}
	filter := state.InboxFilter{UnreadOnly: true, MissedBy: target.name, LiveSince: liveSince, Limit: i.catchUpLimit}
	if !i.cfg.PushesBroadcast(target.name) {
		filter.ExcludeKind = InboxBroadcast
	}
	// Under liveMu every item stored so far has had its live pushes claimed,
	// and every later one will find the surface live: the missed items are
	// those up to this sequence that are not in flight to it.
	i.liveMu.Lock()
	seq, err := i.store.InboxSeq(i.baseCtx)
	i.mu.Lock()
	for key := range i.pushing {
		if key.surface == target.name {
			filter.ExcludeIDs = append(filter.ExcludeIDs, key.item)
		}
	}
	i.mu.Unlock()
	i.liveMu.Unlock()
	if err != nil {
		log.Printf("inbox: catch-up for %s: %v", target.name, err)
		return
	}
	filter.MaxSeq = seq
	i.spawn(func() {
		items, err := i.store.ListInbox(i.baseCtx, filter)
		if err != nil {
			log.Printf("inbox: catch-up for %s: %v", target.name, err)
			return
		}
		now := i.now()
		for n := len(items) - 1; n >= 0 && i.baseCtx.Err() == nil; n-- {
			if !i.claimPush(i.baseCtx, items[n].ID, target.name) {
				continue
			}
			// A reconnect in the surface's quiet window holds what it
			// missed, as a live Store would have.
			if i.holds(&items[n], target, now) {
				i.hold(&items[n], target, now)
				continue
			}
			i.push(&items[n], target)
		}
	})
	// A reconnect is a release trigger too: outside its window the backlog
	// goes now, inside it the surface is told again when to come back.
	i.SurfaceRelease(p)
}

// Close cancels in-flight pushes and waits for them; later Stores push nothing.
func (i *Inbox) Close() {
	if i == nil {
		return
	}
	i.closeMu.Lock()
	i.closed = true
	i.closeMu.Unlock()
	i.cancel()
	i.wg.Wait()
}

// threadable keeps cid only when it names an ask the human is party to that
// the tracker still holds (open, or closed within its TTL). The check is in
// memory; a stale or foreign cid yields an unthreaded item, never an error,
// and never touches the ask.
func (i *Inbox) threadable(cid string) string {
	if cid == "" || i.asks == nil {
		return ""
	}
	ask, ok := i.asks.Get(cid)
	human, _ := i.Human()
	if !ok || (ask.FromPeerID != human && ask.ToPeerID != human) {
		return ""
	}
	return cid
}

// progressThread keeps cid as a progress notify's thread when it names an
// open ask between the human and sender, its asker or current holder, so a
// peer outside the ask cannot post into the human's thread and nothing posts
// under an ask already closed. One snapshot answers every test: the ask can
// be pruned at any moment. Like threadable, a cid that does not qualify
// yields an unthreaded item, never an error.
func (i *Inbox) progressThread(cid, sender string) string {
	if cid == "" || sender == "" || i.asks == nil {
		return ""
	}
	ask, ok := i.asks.Get(cid)
	if !ok || ask.Closed {
		return ""
	}
	human, _ := i.Human()
	parties := []proto.PeerID{ask.FromPeerID, ask.ToPeerID}
	if !slices.Contains(parties, human) || !slices.Contains(parties, proto.PeerID(sender)) {
		return ""
	}
	return cid
}

// surfaceTarget is one surface a push goes to: a peer that renders the inbox,
// or a surface the daemon renders itself.
type surfaceTarget struct {
	name  string
	peer  *proto.Peer
	local LocalSurface
}

// pushTargets are the live surfaces this kind of item goes to.
func (i *Inbox) pushTargets(kind string) []surfaceTarget {
	human, _ := i.Human()
	var out []surfaceTarget
	for _, p := range i.reg.GetAllPeers() {
		if p.PeerID == human || !proto.IsInboxSurface(p) || !i.transport.IsConnected(p.PeerID) {
			continue
		}
		out = append(out, surfaceTarget{name: string(p.DisplayName), peer: p})
	}
	for _, s := range i.localSurfaces() {
		if s.Live() {
			out = append(out, surfaceTarget{name: s.Name(), local: s})
		}
	}
	if kind == InboxBroadcast {
		kept := out[:0]
		for _, t := range out {
			if i.cfg.PushesBroadcast(t.name) {
				kept = append(kept, t)
			}
		}
		out = kept
	}
	sort.Slice(out, func(a, b int) bool { return out[a].name < out[b].name })
	return out
}

func (i *Inbox) spawn(f func()) bool {
	i.closeMu.Lock()
	defer i.closeMu.Unlock()
	if i.closed {
		return false
	}
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		f()
	}()
	return true
}

// push renders item on one surface and records the outcome. The caller has
// claimed the push; push releases the claim once the outcome is recorded. A daemon-rendered
// surface reports its own receipt. A surface that declared inbox_surface gets
// an inbox_item frame and must ack it; a legacy bridge gets today's notify
// frame, which proves only the socket write.
func (i *Inbox) push(item *state.InboxItem, target surfaceTarget) {
	defer i.release(item.ID, target.name)
	i.recordPush(item, target.name, i.deliver(item, target))
}

// deliver renders item on one surface and reports the outcome, recording
// nothing: push records it for item, a digest for each item it carries.
func (i *Inbox) deliver(item *state.InboxItem, target surfaceTarget) pushOutcome {
	ctx, cancel := context.WithTimeout(i.baseCtx, i.pushTimeout+5*time.Second)
	defer cancel()
	name, surface := target.name, target.peer

	if target.local != nil {
		pushCtx, cancelPush := context.WithTimeout(ctx, i.pushTimeout)
		defer cancelPush()
		out := pushOutcome{verified: true}
		if out.err = target.local.Push(pushCtx, item); out.err != nil {
			out.errorClass = "network"
			if errors.Is(out.err, context.DeadlineExceeded) {
				out.errorClass = "no_receipt"
			}
		}
		return out
	}

	if !proto.HasCapability(surface.Metadata, proto.CapInboxSurface) {
		err := i.transport.Send(ctx, surface.PeerID, legacyInboxFrame(item, surface))
		return pushOutcome{verified: false, err: err, errorClass: "network"}
	}

	ref := ""
	if item.CorrelationID != "" {
		var err error
		if ref, err = i.store.InboxThreadRef(ctx, item.CorrelationID, name); err != nil {
			log.Printf("inbox: thread ref for %s on %s: %v", item.ID, name, err)
		}
	}
	frame := map[string]any{
		"type":         string(proto.FrameInboxItem),
		"delivery_id":  item.ID,
		"item":         inboxItemWire(item),
		"reply_to_ref": ref,
	}
	ack, err := i.transport.SendAndWaitDeliveryAck(ctx, surface.PeerID, frame, i.pushTimeout)
	switch {
	case err != nil:
		return pushOutcome{verified: true, err: err, errorClass: "network"}
	case ack == nil:
		return pushOutcome{verified: true, err: errors.New("no receipt"), errorClass: "no_receipt"}
	}
	status, _ := ack["status"].(string)
	errorClass, _ := ack["error_class"].(string)
	out := pushOutcome{verified: true, refs: receiptRefs(ack)}
	if status != state.PushPushed {
		if errorClass == "" {
			errorClass = "other"
		}
		out.err, out.errorClass = errors.New(status), errorClass
	}
	return out
}

type pushKey struct{ item, surface string }

// claim takes the push of item to surface; false while another is in flight.
func (i *Inbox) claim(itemID, surface string) bool {
	key := pushKey{itemID, surface}
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, busy := i.pushing[key]; busy {
		return false
	}
	i.pushing[key] = struct{}{}
	return true
}

func (i *Inbox) release(itemID, surface string) {
	i.mu.Lock()
	delete(i.pushing, pushKey{itemID, surface})
	i.mu.Unlock()
}

// claimPush claims a catch-up push, which goes once per item and surface: it
// fails while another push of the item is in flight and once one is recorded.
// An unreadable record does not block the push: a duplicate beats a loss.
func (i *Inbox) claimPush(ctx context.Context, itemID, surface string) bool {
	if !i.claim(itemID, surface) {
		return false
	}
	item, err := i.store.InboxItem(ctx, itemID)
	if err != nil {
		log.Printf("inbox: push record for %s on %s: %v", itemID, surface, err)
		return true
	}
	if item != nil && item.PushedTo(surface) {
		i.release(itemID, surface)
		return false
	}
	return true
}

// receiptRefs lists the messages a surface rendered, first (the thread
// anchor) first. A failed push can still carry the refs of a partial render.
func receiptRefs(ack map[string]any) []string {
	var refs []string
	if ref, _ := ack["surface_ref"].(string); ref != "" {
		refs = append(refs, ref)
	}
	list, _ := ack["surface_refs"].([]any)
	for _, v := range list {
		if ref, _ := v.(string); ref != "" && (len(refs) == 0 || ref != refs[0]) {
			refs = append(refs, ref)
		}
	}
	return refs
}

type pushOutcome struct {
	verified   bool
	refs       []string
	err        error
	errorClass string
}

// recordPush writes the push record and its trace stage. It uses its own short
// context so an outcome observed during shutdown is still recorded; Close
// waits for it before the store closes.
func (i *Inbox) recordPush(item *state.InboxItem, surface string, out pushOutcome) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	push := state.InboxPush{Surface: surface, Status: state.PushPushed}
	if len(out.refs) > 0 {
		push.SurfaceRef, push.Refs = out.refs[0], out.refs[1:]
	}
	detail := map[string]any{"item_id": item.ID, "surface": surface, "verified": out.verified}
	stage, status := "surface_pushed", ""
	if out.err != nil {
		push.Status, push.ErrorClass = state.PushFailed, out.errorClass
		detail["error_class"] = out.errorClass
		stage, status = "surface_failed", "fail"
	}
	if err := i.store.RecordInboxPush(ctx, item.ID, push); err != nil {
		log.Printf("inbox: record push %s on %s: %v", item.ID, surface, err)
	}
	i.trace(ctx, item, stage, status, detail)
}

func (i *Inbox) trace(ctx context.Context, item *state.InboxItem, stage, status string, detail map[string]any) {
	human, _ := i.Human()
	if err := i.store.RecordTrace(ctx, item.TraceID, item.TraceKind, stage, status, item.TraceID, string(human), item.FromPeerID, detail); err != nil {
		log.Printf("inbox: trace %s %s: %v", item.ID, stage, err)
	}
}

// inboxItemWire is the item as surfaces see it.
func inboxItemWire(item *state.InboxItem) map[string]any {
	wire := map[string]any{
		"id": item.ID, "kind": item.Kind, "from": item.FromName, "from_peer_id": item.FromPeerID,
		"text": item.Text, "created_at": item.CreatedAt,
	}
	if item.CorrelationID != "" {
		wire["correlation_id"] = item.CorrelationID
	}
	if len(item.Attachments) > 0 {
		wire["attachments"] = item.Attachments
	}
	if item.Question != nil {
		wire["question"] = item.Question
	}
	return wire
}

// legacyInboxFrame renders an item for a bridge that predates inbox_item: the
// notify/ask frame it already understands.
func legacyInboxFrame(item *state.InboxItem, surface *proto.Peer) map[string]any {
	typ := proto.FrameNotify
	switch item.Kind {
	case InboxAsk:
		typ = proto.FrameAsk
	case InboxBroadcast:
		typ = proto.FrameBroadcast
	}
	frame := map[string]any{
		"type": string(typ), "from_peer": item.FromName, "to_peer": string(surface.DisplayName),
		"text": item.Text,
	}
	if item.Kind == InboxAsk {
		frame["correlation_id"] = item.CorrelationID
		if item.Question != nil {
			frame["question"] = item.Question
		}
	}
	if len(item.Attachments) > 0 {
		frame["attachments"] = item.Attachments
	}
	return frame
}

// humanNameRe is the display-name rule every peer obeys (hub/ws.go).
var humanNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// RegisterHumanPeer registers the daemon-owned human peer and points the
// registry's surface alias at it. The durable mapping keeps its peer_id stable
// across restarts. It is in-process, so liveness sweeps never demote it: the
// inbox always accepts, and live surfaces are published as metadata.
func RegisterHumanPeer(ctx context.Context, reg *peer.Registry, name, machine string) (proto.PeerID, proto.DisplayName, error) {
	if !humanNameRe.MatchString(name) {
		log.Printf("WARNING human.name %q is not a valid peer name; using \"human\"", name)
		name = "human"
	}
	path := "/human"
	display := proto.DisplayName(name)
	id, assigned, err := reg.AllocateAndRegister(ctx, peer.AllocateParams{
		Circle:               "default",
		Backend:              proto.AgentHuman,
		Role:                 proto.RoleHuman,
		Path:                 &path,
		PreferredDisplayName: &display,
		Machine:              machine,
		Metadata:             map[string]any{"in_process": true},
	})
	if err != nil {
		return "", "", err
	}
	reg.SetHumanPeer(id)
	return id, assigned, nil
}
