package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// ============================================================================
// Wire view (rfcs/wire-observability.md): a read-only, envelope-only view of
// ask and notify threads for orchestrators and the operator.
//
// It keeps no state of its own. Open asks come from the AskTracker, history
// from the delivery-trace ledger, whether a notify still waits from the
// delivery queue, names, circles and liveness from the registry, and holder
// turns from the chat-turn journal; retention bounds all of them. The envelope never carries message text, delegation notes or
// reasons. Later pieces (reassign, cancel, bodies) plug in through Resolve
// and Thread.
// ============================================================================

// WireLedger is the delivery-trace ledger seam; *state.Store satisfies it.
type WireLedger interface {
	TraceThreadsSince(ctx context.Context, since time.Time) ([]state.TraceThread, error)
	StagesFor(ctx context.Context, traceID string) ([]state.TraceRow, error)
	AssistantTurnsBetween(ctx context.Context, peerID string, from, to time.Time) (int, error)
	// DeliveryQueued is the delivery queue's answer for a notify traced as
	// pending: still waiting, or gone (drained or expired).
	DeliveryQueued(ctx context.Context, deliveryID string, now time.Time) (bool, error)
}

// WirePeers resolves a party; *peer.Registry satisfies it.
type WirePeers interface {
	GetPeer(id proto.PeerID) (*proto.Peer, bool)
}

// Wire is the thread view.
type Wire struct {
	asks   *AskTracker
	ledger WireLedger
	peers  WirePeers
	stuck  config.WireStuckConfig
	now    func() time.Time
}

// NewWire returns the wire view over the daemon's existing records.
func NewWire(asks *AskTracker, ledger WireLedger, peers WirePeers, stuck config.WireStuckConfig) *Wire {
	return &Wire{asks: asks, ledger: ledger, peers: peers, stuck: stuck, now: time.Now}
}

// WireParty is one party to a thread, as the registry knows it now. Known is
// false for a peer the registry no longer has.
type WireParty struct {
	PeerID proto.PeerID      `json:"peer_id"`
	Name   proto.DisplayName `json:"display_name"`
	Circle string            `json:"circle"`
	Role   proto.PeerRole    `json:"role"`
	Status proto.PeerStatus  `json:"status"`
	Known  bool              `json:"known"`
}

// WireStage is one ledger stage, envelope only.
type WireStage struct {
	Seq         int       `json:"seq"`
	At          string    `json:"at"`
	Stage       string    `json:"stage"`
	Status      string    `json:"status"`
	Peer        WireParty `json:"peer"`
	CloseReason string    `json:"close_reason,omitempty"`
}

// WireThread is one thread's envelope.
type WireThread struct {
	// ID is the thread id to show and to pass back: the correlation or
	// delivery id, or an h- alias when that id is not plainly printable.
	ID          string      `json:"id"`
	Kind        string      `json:"kind"` // ask | notify
	State       string      `json:"state"`
	Open        bool        `json:"open"`
	From        WireParty   `json:"from"`
	Holder      WireParty   `json:"holder"`        // the ask's current holder, or the notify's recipient
	Via         []WireParty `json:"via,omitempty"` // earlier holders, oldest first
	CreatedAt   time.Time   `json:"created_at"`
	AssignedAt  time.Time   `json:"assigned_at"` // the current holder's assignment
	LastAt      time.Time   `json:"last_at"`
	HolderTurns int         `json:"holder_turns"` // assistant turns since assignment; -1 when not counted
	Stuck       []string    `json:"stuck,omitempty"`
	// Protected marks a thread a party of which is the human, a surface
	// service, or unknown to the registry: bodies and interventions will
	// refuse it (rfcs/wire-observability.md section 2).
	Protected bool        `json:"protected"`
	Stages    []WireStage `json:"stages,omitempty"` // Thread only
}

// WireFilter narrows Threads. Zero values mean no filter.
type WireFilter struct {
	Circle    string
	Peer      string // a party's peer id or display name
	Kind      string
	State     string
	StuckOnly bool
	Since     time.Duration // default 24h
	Limit     int           // default 50, at most 100
	Cursor    string
}

// WirePage is one page of threads; Cursor resumes after the last one.
type WirePage struct {
	Threads []WireThread `json:"threads"`
	Cursor  string       `json:"cursor,omitempty"`
}

// ErrWireCursor rejects a cursor this view did not issue.
var ErrWireCursor = errors.New("invalid wire cursor")

const (
	wireDefaultSince = 24 * time.Hour
	wireDefaultLimit = 50
	wireMaxLimit     = 100
	wireMaxScan      = 1000 // threads reduced per call before handing back a cursor
)

// Delivery stages that confirm the current assignment reached its holder.
var wireDelivered = map[string]bool{"thread_input_accepted": true, "pane_injected": true, "remote_delivered": true, "inbox_stored": true}

type wireCandidate struct {
	id   string
	kind string
	last time.Time
}

// Threads lists threads newest activity first.
func (w *Wire) Threads(ctx context.Context, f WireFilter) (WirePage, error) {
	since := f.Since
	if since <= 0 {
		since = wireDefaultSince
	}
	limit := f.Limit
	if limit <= 0 {
		limit = wireDefaultLimit
	}
	limit = min(limit, wireMaxLimit)
	after, err := decodeWireCursor(f.Cursor)
	if err != nil {
		return WirePage{}, err
	}
	candidates, err := w.candidates(ctx, w.now().Add(-since))
	if err != nil {
		return WirePage{}, err
	}
	page := WirePage{Threads: []WireThread{}}
	scanned := 0
	for i, c := range candidates {
		if after != nil && !c.before(*after) {
			continue
		}
		t, ok, err := w.reduce(ctx, c.id, c.kind, false)
		if err != nil {
			return WirePage{}, err
		}
		scanned++
		if ok && f.matches(t) {
			page.Threads = append(page.Threads, t)
		}
		more := i < len(candidates)-1
		if more && (len(page.Threads) == limit || scanned == wireMaxScan) {
			page.Cursor = encodeWireCursor(c)
			break
		}
	}
	return page, nil
}

// Thread returns one thread with its stages. id is a thread id as Threads
// shows it (an h- alias included).
func (w *Wire) Thread(ctx context.Context, id string) (WireThread, bool, error) {
	traceID, ok, err := w.Resolve(ctx, id)
	if err != nil || !ok {
		return WireThread{}, false, err
	}
	return w.reduce(ctx, traceID, "", true)
}

// Resolve maps a thread id as the view shows it (an h- alias included) to
// the ask correlation id or notify delivery id it stands for. ok is false
// for an id that names no ask or notify thread. Actions that take a thread
// id from the view (delegate, reassign, cancel) resolve it here.
func (w *Wire) Resolve(ctx context.Context, id string) (string, bool, error) {
	if !isWireAlias(id) {
		if _, tracked := w.asks.Get(id); tracked {
			return id, true, nil
		}
		rows, err := w.ledger.StagesFor(ctx, id)
		if err != nil {
			return "", false, err
		}
		return id, len(rows) > 0 && (rows[0].Kind == "ask" || rows[0].Kind == "notify"), nil
	}
	candidates, err := w.candidates(ctx, time.Time{})
	if err != nil {
		return "", false, err
	}
	for _, c := range candidates {
		if wireAlias(c.id) == id {
			return c.id, true, nil
		}
	}
	return "", false, nil
}

// candidates are the ledger's threads active since since, plus open asks the
// ledger has no history for (/query, ask_many children), newest first.
func (w *Wire) candidates(ctx context.Context, since time.Time) ([]wireCandidate, error) {
	rows, err := w.ledger.TraceThreadsSince(ctx, since)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(rows))
	out := make([]wireCandidate, 0, len(rows))
	for _, r := range rows {
		seen[r.TraceID] = true
		out = append(out, wireCandidate{id: r.TraceID, kind: r.Kind, last: parseWireTS(r.LastTS)})
	}
	for _, ask := range w.asks.OpenAsks() {
		if !seen[ask.CorrelationID] {
			out = append(out, wireCandidate{id: ask.CorrelationID, kind: "ask", last: assignedAt(ask)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[j].before(out[i]) })
	return out, nil
}

// before reports whether c sorts after o (older activity, then smaller id).
func (c wireCandidate) before(o wireCandidate) bool {
	if !c.last.Equal(o.last) {
		return c.last.Before(o.last)
	}
	return c.id < o.id
}

func assignedAt(ask *Ask) time.Time {
	if n := len(ask.Delegations); n > 0 {
		return ask.Delegations[n-1].At
	}
	return ask.CreatedAt
}

func (f WireFilter) matches(t WireThread) bool {
	if f.Kind != "" && t.Kind != f.Kind {
		return false
	}
	if f.State != "" && t.State != f.State {
		return false
	}
	if f.StuckOnly && len(t.Stuck) == 0 {
		return false
	}
	parties := append([]WireParty{t.From, t.Holder}, t.Via...)
	if f.Circle != "" && !anyParty(parties, func(p WireParty) bool { return p.Known && p.Circle == f.Circle }) {
		return false
	}
	if name := strings.TrimPrefix(f.Peer, "@"); name != "" &&
		!anyParty(parties, func(p WireParty) bool { return string(p.PeerID) == name || string(p.Name) == name }) {
		return false
	}
	return true
}

func anyParty(parties []WireParty, fn func(WireParty) bool) bool {
	for _, p := range parties {
		if p.PeerID != "" && fn(p) {
			return true
		}
	}
	return false
}

// reduce folds a thread's ledger stages and, for an ask, its tracker entry
// into an envelope. ok is false when neither knows the thread.
func (w *Wire) reduce(ctx context.Context, traceID, kind string, withStages bool) (WireThread, bool, error) {
	rows, err := w.ledger.StagesFor(ctx, traceID)
	if err != nil {
		return WireThread{}, false, err
	}
	ask, tracked := w.asks.Get(traceID)
	if len(rows) == 0 && !tracked {
		return WireThread{}, false, nil
	}
	if kind == "" {
		kind = "ask"
		if len(rows) > 0 {
			kind = rows[0].Kind
		}
	}
	t := WireThread{ID: wireDisplayID(traceID), Kind: kind, HolderTurns: -1}
	if kind == "notify" {
		if err := w.reduceNotify(ctx, &t, traceID, rows); err != nil {
			return WireThread{}, false, err
		}
	} else {
		w.reduceAsk(&t, rows, ask, tracked)
	}
	if t.Open && kind == "ask" {
		w.askStuck(ctx, &t, ask)
	}
	parties := append([]WireParty{t.From, t.Holder}, t.Via...)
	t.Protected = anyParty(parties, func(p WireParty) bool {
		return !p.Known || p.Role == proto.RoleHuman || p.Role == proto.RoleService
	})
	if withStages {
		t.Stages = make([]WireStage, 0, len(rows))
		for _, r := range rows {
			st := WireStage{Seq: r.Seq, At: r.TS, Stage: r.Stage, Status: r.Status, Peer: w.party(proto.PeerID(r.PeerID))}
			if reason, ok := r.Detail["close_reason"].(string); ok {
				st.CloseReason = reason
			}
			t.Stages = append(t.Stages, st)
		}
	}
	return t, true, nil
}

type wireAssignment struct {
	holder   proto.PeerID
	at       time.Time
	delivery string // delivered | deferred | ""
}

func (w *Wire) reduceAsk(t *WireThread, rows []state.TraceRow, ask *Ask, tracked bool) {
	var asker proto.PeerID
	var assigns []wireAssignment
	closed, reason := false, ""
	for _, r := range rows {
		at := parseWireTS(r.TS)
		if t.CreatedAt.IsZero() {
			t.CreatedAt = at
		}
		t.LastAt = at
		if asker == "" && r.FromPeerID != "" {
			asker = proto.PeerID(r.FromPeerID)
		}
		switch {
		case r.Stage == "delegated":
			assigns = append(assigns, wireAssignment{holder: proto.PeerID(r.PeerID), at: at})
		case r.Stage == "delegate_failed":
			if len(assigns) > 1 {
				assigns = assigns[:len(assigns)-1] // the hop was rolled back
			}
		case len(assigns) == 0 && r.PeerID != "":
			assigns = append(assigns, wireAssignment{holder: proto.PeerID(r.PeerID), at: t.CreatedAt})
		}
		if n := len(assigns); n > 0 {
			cur := &assigns[n-1]
			switch {
			case wireDelivered[r.Stage]:
				cur.delivery = "delivered"
			case r.Stage == "websocket_sent" && r.Detail["transport"] == "deferred" && cur.delivery == "":
				cur.delivery = "deferred" // a busy holder; its Stop hook surfaces the ask
			}
		}
		if r.Stage == "closed" {
			closed = true
			reason, _ = r.Detail["close_reason"].(string)
			if reason == "" && r.Status == "fail" {
				reason = "send_failed"
			}
		}
	}
	if tracked {
		// The tracker is authoritative for an open ask: its holder and hops.
		asker = ask.FromPeerID
		if t.CreatedAt.IsZero() || ask.CreatedAt.Before(t.CreatedAt) {
			t.CreatedAt = ask.CreatedAt
		}
		live := []wireAssignment{{holder: ask.ToPeerID, at: ask.CreatedAt}}
		if len(ask.Delegations) > 0 {
			live[0].holder = ask.Delegations[0].FromPeerID
			for _, hop := range ask.Delegations {
				live = append(live, wireAssignment{holder: hop.ToPeerID, at: hop.At})
			}
		}
		if n := len(assigns); n > 0 && assigns[n-1].holder == live[len(live)-1].holder {
			live[len(live)-1].delivery = assigns[n-1].delivery
		}
		assigns = live
		if last := assignedAt(ask); last.After(t.LastAt) {
			t.LastAt = last
		}
		if ask.Closed && !closed {
			closed, reason = true, ask.CloseReason
		}
	}
	t.From = w.party(asker)
	if n := len(assigns); n > 0 {
		t.Holder = w.party(assigns[n-1].holder)
		t.AssignedAt = assigns[n-1].at
		for _, a := range assigns[:n-1] {
			t.Via = append(t.Via, w.party(a.holder))
		}
	}
	switch {
	case closed:
		t.State = reason
		if t.State == "" {
			t.State = "closed"
		}
	case !tracked:
		t.State = "unknown" // the tracker lost it (eviction, restart) with no closed stage
	case ask.PendingReply != nil:
		t.State, t.Open = "reply_stashed", true
	default:
		t.Open = true
		t.State = "sent"
		if n := len(assigns); n > 0 && assigns[n-1].delivery != "" {
			t.State = assigns[n-1].delivery
		} else if len(assigns) > 1 {
			t.State = "delegated"
		}
	}
}

// reduceNotify reads a notify's stages as the messaging routes record them:
// a delivery receipt, a failure, pending (queued for an offline recipient),
// remote_<outcome> across machines, or websocket_sent without a receipt.
// The delivery queue records how a queued notify left it: dequeued (handed
// to the reconnected peer, not a receipt), or expired or dropped (failures).
// A pending notify with no such stage is asked about: still queued, or,
// for one queued before the queue recorded outcomes, unverified.
func (w *Wire) reduceNotify(ctx context.Context, t *WireThread, id string, rows []state.TraceRow) error {
	var sender, recipient proto.PeerID
	var pendingAt time.Time
	delivered, failed, unverified, dequeued, remote, outcome := false, false, false, false, "", ""
	for _, r := range rows {
		at := parseWireTS(r.TS)
		if t.CreatedAt.IsZero() {
			t.CreatedAt = at
		}
		t.LastAt = at
		if sender == "" && r.FromPeerID != "" {
			sender = proto.PeerID(r.FromPeerID)
		}
		if recipient == "" && r.PeerID != "" {
			recipient = proto.PeerID(r.PeerID)
		}
		switch {
		case wireDelivered[r.Stage]:
			delivered = true
		case r.Stage == state.QueueExpired || r.Stage == state.QueueDropped:
			outcome = r.Stage // the queue's own failure, shown as itself
		case r.Status == "fail":
			failed = true
		case r.Stage == "pending":
			pendingAt = at
		case r.Stage == state.QueueDequeued:
			dequeued = true
		case r.Stage == "websocket_sent":
			unverified = true
		case strings.HasPrefix(r.Stage, "remote_"):
			remote = r.Stage
		}
	}
	t.From, t.Holder, t.AssignedAt = w.party(sender), w.party(recipient), t.CreatedAt
	switch {
	case delivered:
		t.State = "delivered"
	case outcome != "":
		t.State = outcome
	case failed:
		t.State = "failed"
	case remote != "":
		t.State = remote // the other machine owns the rest
	case dequeued:
		t.State = state.QueueDequeued
	case !pendingAt.IsZero():
		queued, err := w.ledger.DeliveryQueued(ctx, id, w.now())
		if err != nil {
			return err
		}
		if !queued {
			t.State = "unverified"
			break
		}
		t.State, t.Open = "queued", true
		if w.now().Sub(pendingAt) >= minutes(w.stuck.UndeliveredMinutes) {
			t.Stuck = append(t.Stuck, "undelivered")
		}
	case unverified:
		t.State = "unverified" // sent on a transport that returns no receipt
	default:
		t.State, t.Open = "sent", true
	}
	return nil
}

// askStuck computes an open ask's stuck signals now (never on a timer).
func (w *Wire) askStuck(ctx context.Context, t *WireThread, ask *Ask) {
	now := w.now()
	if t.State == "reply_stashed" {
		if ask != nil && ask.PendingReplyAt != nil && now.Sub(*ask.PendingReplyAt) >= minutes(w.stuck.AskerGoneMinutes) {
			t.Stuck = append(t.Stuck, "asker_gone")
		}
		return
	}
	holder := t.Holder
	if !holder.Known || holder.Status == proto.StatusOffline {
		t.Stuck = append(t.Stuck, "holder_offline")
	}
	if holder.PeerID == "" || t.AssignedAt.IsZero() {
		return
	}
	turns, err := w.ledger.AssistantTurnsBetween(ctx, string(holder.PeerID), t.AssignedAt, now)
	if err != nil {
		return
	}
	t.HolderTurns = turns
	if holder.Status == proto.StatusOnline && turns >= w.stuck.IgnoredTurns {
		t.Stuck = append(t.Stuck, "ignored")
	}
	if turns == 0 && now.Sub(t.AssignedAt) >= minutes(w.stuck.SlowMinutes) {
		t.Stuck = append(t.Stuck, "slow")
	}
}

func (w *Wire) party(id proto.PeerID) WireParty {
	if id == "" {
		return WireParty{}
	}
	if p, ok := w.peers.GetPeer(id); ok && p != nil {
		return WireParty{PeerID: p.PeerID, Name: p.DisplayName, Circle: p.Circle, Role: p.Role, Status: p.Status, Known: true}
	}
	return WireParty{PeerID: id, Name: proto.DisplayName(id)}
}

func minutes(m float64) time.Duration { return time.Duration(m * float64(time.Minute)) }

func parseWireTS(ts string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
}

// wireIDPattern is what a thread id must look like to be shown as is.
// Anything else (a caller-supplied correlation id with spaces, tabs or
// markup) is shown as its h- alias.
var wireIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func wireDisplayID(id string) string {
	if wireIDPattern.MatchString(id) && !isWireAlias(id) {
		return id
	}
	return wireAlias(id)
}

func wireAlias(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "h-" + hex.EncodeToString(sum[:])[:12]
}

func isWireAlias(id string) bool {
	return len(id) == 14 && strings.HasPrefix(id, "h-")
}

func encodeWireCursor(c wireCandidate) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.last.UTC().Format(time.RFC3339Nano) + "|" + c.id))
}

func decodeWireCursor(cursor string) (*wireCandidate, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, ErrWireCursor
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	at, perr := time.Parse(time.RFC3339Nano, ts)
	if !ok || perr != nil {
		return nil, ErrWireCursor
	}
	return &wireCandidate{id: id, last: at}, nil
}
