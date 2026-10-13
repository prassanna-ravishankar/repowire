package service

import (
	"context"
	"errors"
	"log"
	"regexp"
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

// InboxStore is the persistence seam; *state.Store satisfies it.
type InboxStore interface {
	InsertInboxItem(ctx context.Context, item *state.InboxItem) error
	InboxItem(ctx context.Context, id string) (*state.InboxItem, error)
	ListInbox(ctx context.Context, filter state.InboxFilter) ([]state.InboxItem, error)
	RecordInboxPush(ctx context.Context, itemID string, push state.InboxPush) error
	InboxItemBySurfaceRef(ctx context.Context, surface, ref string) (*state.InboxItem, error)
	InboxThreadRef(ctx context.Context, cid, surface string) (string, error)
	MarkInboxRead(ctx context.Context, id, surface string) (bool, error)
	MarkInboxReplied(ctx context.Context, id, surface string) (bool, error)
	RecordTrace(ctx context.Context, traceID, kind, stage, status, deliveryID, peerID, fromPeerID string, detail map[string]any) error
}

// inboxRegistry is the slice of *peer.Registry the inbox reads and annotates.
type inboxRegistry interface {
	GetAllPeers() []*proto.Peer
	AddEvent(ctx context.Context, typ string, payload map[string]any) string
	UpdateMetadataByName(ctx context.Context, identifier string, metadata map[string]any) (bool, error)
}

// InboxReceipt is what a sender learns synchronously: the item was stored and
// which surfaces a push was started on. Per-surface outcomes land in traces.
type InboxReceipt struct {
	ItemID   string
	Surfaces []string
}

// InboxStoreParams describes one inbound item.
type InboxStoreParams struct {
	Kind          string
	TraceID       string
	TraceKind     string
	FromPeerID    string
	FromName      string
	CorrelationID string
	Text          string
	Attachments   []map[string]any
	Question      map[string]any
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

	pushTimeout time.Duration
	baseCtx     context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	closeMu     sync.Mutex
	closed      bool
}

// NewInbox wires the inbox. asks may be nil (no thread validation: items never
// carry a correlation_id then).
func NewInbox(store InboxStore, reg inboxRegistry, transport Transport, asks *AskTracker, cfg config.HumanConfig) *Inbox {
	ctx, cancel := context.WithCancel(context.Background())
	return &Inbox{store: store, reg: reg, transport: transport, asks: asks, cfg: cfg,
		pushTimeout: inboxPushTimeout, baseCtx: ctx, cancel: cancel}
}

// SetHuman records the registered human peer.
func (i *Inbox) SetHuman(id proto.PeerID, name proto.DisplayName) {
	i.mu.Lock()
	i.human, i.humanName = id, name
	i.mu.Unlock()
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
		CorrelationID: i.threadable(params.CorrelationID),
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
	if err := i.store.InsertInboxItem(ctx, item); err != nil {
		return InboxReceipt{}, err
	}
	i.trace(ctx, item, "inbox_stored", "", map[string]any{"item_id": item.ID, "kind": item.Kind})
	i.reg.AddEvent(ctx, "inbox_item", map[string]any{
		"item_id": item.ID, "kind": item.Kind, "from": item.FromName,
		"from_peer_id": item.FromPeerID, "correlation_id": item.CorrelationID,
	})

	targets := i.pushTargets(item.Kind)
	names := make([]string, 0, len(targets))
	for _, surface := range targets {
		names = append(names, string(surface.DisplayName))
	}
	snapshot := *item
	for _, surface := range targets {
		surface := surface
		i.spawn(func() { i.push(&snapshot, surface) })
	}
	return InboxReceipt{ItemID: item.ID, Surfaces: names}, nil
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
		surfaces[string(p.DisplayName)] = map[string]any{"live": connected}
	}
	_, _ = i.reg.UpdateMetadataByName(ctx, string(human), map[string]any{"surfaces": surfaces, "surfaces_live": live})
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

// pushTargets are the live surfaces this kind of item goes to.
func (i *Inbox) pushTargets(kind string) []*proto.Peer {
	human, _ := i.Human()
	var out []*proto.Peer
	for _, p := range i.reg.GetAllPeers() {
		if p.PeerID == human || !proto.IsInboxSurface(p) || !i.transport.IsConnected(p.PeerID) {
			continue
		}
		if kind == InboxBroadcast && !i.cfg.PushesBroadcast(string(p.DisplayName)) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].DisplayName < out[b].DisplayName })
	return out
}

func (i *Inbox) spawn(f func()) {
	i.closeMu.Lock()
	defer i.closeMu.Unlock()
	if i.closed {
		return
	}
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		f()
	}()
}

// push renders item on one surface and records the outcome. A surface that
// declared inbox_surface gets an inbox_item frame and must ack it; a legacy
// bridge gets today's notify frame, which proves only the socket write.
func (i *Inbox) push(item *state.InboxItem, surface *proto.Peer) {
	ctx, cancel := context.WithTimeout(i.baseCtx, i.pushTimeout+5*time.Second)
	defer cancel()
	name := string(surface.DisplayName)

	if !proto.HasCapability(surface.Metadata, proto.CapInboxSurface) {
		err := i.transport.Send(ctx, surface.PeerID, legacyInboxFrame(item, surface))
		i.recordPush(item, name, pushOutcome{verified: false, err: err, errorClass: "network"})
		return
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
		i.recordPush(item, name, pushOutcome{verified: true, err: err, errorClass: "network"})
	case ack == nil:
		i.recordPush(item, name, pushOutcome{verified: true, err: errors.New("no receipt"), errorClass: "no_receipt"})
	default:
		status, _ := ack["status"].(string)
		surfaceRef, _ := ack["surface_ref"].(string)
		errorClass, _ := ack["error_class"].(string)
		if status == state.PushPushed {
			i.recordPush(item, name, pushOutcome{verified: true, ref: surfaceRef})
			return
		}
		if errorClass == "" {
			errorClass = "other"
		}
		i.recordPush(item, name, pushOutcome{verified: true, err: errors.New(status), errorClass: errorClass})
	}
}

type pushOutcome struct {
	verified   bool
	ref        string
	err        error
	errorClass string
}

// recordPush writes the push record and its trace stage. It uses its own short
// context so an outcome observed during shutdown is still recorded; Close
// waits for it before the store closes.
func (i *Inbox) recordPush(item *state.InboxItem, surface string, out pushOutcome) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	push := state.InboxPush{Surface: surface, Status: state.PushPushed, SurfaceRef: out.ref}
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
