package service

// AskTracker is the in-memory source of truth for open ask threads, ported from
// repowire/daemon/ask_tracker.py. It is in-memory only (vanishes on restart like
// the Python one): asks register when a peer fires ask(), the recipient
// transport injects the wire frame on receipt, and the ask closes when the
// recipient acks (bare / with message), opens a reply_to thread, or answers a
// structured question.
//
// Open asks targeting a peer are surfaced on every Stop-hook poll via
// PendingForPeer (the /asks/pending source), so an agent that hasn't acked is
// reminded on every subsequent turn until they do. Reply delivery is handled by
// the notification pipeline; the tracker only owns lifecycle + the pending-reply
// stash for ACP-routed replies whose first delivery failed.
//
// All mutating methods take the internal mutex; reads return snapshot copies so
// callers never observe a half-mutated Ask.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// evictionInterval is the minimum wall time between opportunistic TTL sweeps
// triggered by PendingForPeer (Stop hooks call it on each response, so the map
// is swept regularly without a background timer). Mirrors the Python
// _EVICTION_INTERVAL_SECONDS.
const evictionInterval = 300 * time.Second

// defaultAskTTL mirrors the Python config prune_max_age_hours default.
const defaultAskTTL = 24 * time.Hour

var (
	ErrAskNotFound     = errors.New("ask: unknown correlation_id")
	ErrAlreadyAnswered = errors.New("ask: question already answered/closed")
	ErrQuiesced        = errors.New("ask: peer mid-switch; ask refused")
	ErrQuiesceHasOpen  = errors.New("ask: peer has open asks; cannot quiesce")
)

// AskerIdentity is the asker's stable identity tuple at stash time; captured only
// when the asker peer has every field present and non-default (machine !=
// "unknown", normalized non-empty path). Drives the registry's pass-2 rebind.
type AskerIdentity struct {
	DisplayName proto.DisplayName
	Circle      string
	Backend     proto.AgentType
	Path        string // os.path.normpath(realpath(...)) equivalent
	Machine     string
}

// Answer is the typed resolution of a structured-question ask (rides on the ack).
type Answer struct {
	Outcome  string // "answered" | "denied" | "timed_out" | "cancelled" | "acknowledged"
	OptionID *string
	Text     *string
	Message  *string
}

// Ask is one open ask thread.
//
// closeReason ∈ {ack, ack_with_msg, reply_to, answered, evicted, send_failed}.
// PendingReply is the durable home of an ACP-routed reply whose first delivery
// failed (asker offline); AskerIdentity is captured alongside it for
// identity-tuple rebind. ReplyDelivery ∈ {"push","pull"}.
type Ask struct {
	CorrelationID         string
	FromPeerID            proto.PeerID
	FromPeerName          proto.DisplayName
	ToPeerID              proto.PeerID
	ToPeerName            proto.DisplayName
	Text                  string
	FromRepowireSessionID *string
	ToRepowireSessionID   *string
	ReplyTo               *string
	ParentID              *string
	CreatedAt             time.Time
	Closed                bool
	CloseReason           string
	PendingReply          *string
	PendingReplyAt        *time.Time
	AskerIdentity         *AskerIdentity
	ReplyText             *string
	RepliedAt             *time.Time
	ReplyDelivery         string // "push" | "pull"
	ReplyAttachments      []map[string]any
	Question              map[string]any // structured-question envelope; nil for a plain ask
	Answer                *Answer
}

// clone returns a snapshot copy safe to hand to a reader. Slices/maps/pointers
// are reference-shared but treated read-only by callers; the *Ask itself is a
// fresh value so a reader never sees an in-flight field mutation.
func (a *Ask) clone() *Ask {
	cp := *a
	return &cp
}

// AskTracker is the in-memory store of open ask threads.
type AskTracker struct {
	mu            sync.Mutex
	asks          map[string]*Ask
	ttl           time.Duration
	lastEviction  time.Time
	quiescing     map[proto.PeerID]struct{}
	answerWaiters map[string]chan Answer
}

// NewAskTracker returns an empty tracker. A non-positive ttl falls back to 24h.
func NewAskTracker(ttl time.Duration) *AskTracker {
	if ttl <= 0 {
		ttl = defaultAskTTL
	}
	return &AskTracker{
		asks:          make(map[string]*Ask),
		ttl:           ttl,
		quiescing:     make(map[proto.PeerID]struct{}),
		answerWaiters: make(map[string]chan Answer),
	}
}

// hex8 mints an 8-char hex token (uuid4().hex[:8] analogue).
func hex8() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// RegisterAskParams carries the fields of a new open ask.
type RegisterAskParams struct {
	FromPeerID            proto.PeerID
	FromPeerName          proto.DisplayName
	ToPeerID              proto.PeerID
	ToPeerName            proto.DisplayName
	Text                  string
	ReplyTo               *string
	CorrelationID         string // "" → mint "ask-<hex8>"
	FromRepowireSessionID *string
	ToRepowireSessionID   *string
	ParentID              *string
	Question              map[string]any
	ReplyDelivery         string // "" defaults to "push"
}

// Register records a new open ask and returns its CID. A supplied CID that
// already exists is a retry (existing entry preserved, same id returned).
// Refuses with ErrQuiesced if either endpoint is mid-switch.
func (t *AskTracker) Register(ctx context.Context, p RegisterAskParams) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.quiescing[p.ToPeerID]; ok {
		return "", ErrQuiesced
	}
	if _, ok := t.quiescing[p.FromPeerID]; ok {
		return "", ErrQuiesced
	}
	cid := p.CorrelationID
	if cid == "" {
		cid = "ask-" + hex8()
	}
	if _, ok := t.asks[cid]; ok {
		// Retry: existing entry preserved (lifecycle state intact).
		return cid, nil
	}
	delivery := p.ReplyDelivery
	if delivery == "" {
		delivery = "push"
	}
	t.asks[cid] = &Ask{
		CorrelationID:         cid,
		FromPeerID:            p.FromPeerID,
		FromPeerName:          p.FromPeerName,
		ToPeerID:              p.ToPeerID,
		ToPeerName:            p.ToPeerName,
		Text:                  p.Text,
		FromRepowireSessionID: p.FromRepowireSessionID,
		ToRepowireSessionID:   p.ToRepowireSessionID,
		ReplyTo:               p.ReplyTo,
		ParentID:              p.ParentID,
		Question:              p.Question,
		ReplyDelivery:         delivery,
		CreatedAt:             time.Now().UTC(),
	}
	return cid, nil
}

// Get returns a snapshot copy of the ask, or (nil,false) if unknown.
func (t *AskTracker) Get(cid string) (*Ask, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ask, ok := t.asks[cid]
	if !ok {
		return nil, false
	}
	return ask.clone(), true
}

// ListByParent returns snapshot copies of all child asks of an ask-many parent.
func (t *AskTracker) ListByParent(parentID string) []*Ask {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*Ask
	for _, ask := range t.asks {
		if ask.ParentID != nil && *ask.ParentID == parentID {
			out = append(out, ask.clone())
		}
	}
	return out
}

// cancelWaiter resolves a dangling answer waiter with a cancelled Answer.
// Caller MUST hold t.mu. Called when an ask reaches a terminal state while a
// blocking transport is still awaiting it, so the waiter doesn't hang.
func (t *AskTracker) cancelWaiter(cid string, message *string) {
	w, ok := t.answerWaiters[cid]
	if !ok {
		return
	}
	delete(t.answerWaiters, cid)
	// Buffered cap-1 channel; a single send never blocks and the value is
	// readable even if the waiter already moved on.
	w <- Answer{Outcome: "cancelled", Message: message}
}

// Close terminally closes an ask with reason. Returns (ask,true) if it existed
// and was open; (nil,false) if missing or already closed (idempotent). Cancels
// any blocking answer waiter.
func (t *AskTracker) Close(ctx context.Context, cid, reason string) (*Ask, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ask, ok := t.asks[cid]
	if !ok || ask.Closed {
		return nil, false
	}
	ask.Closed = true
	ask.CloseReason = reason
	r := reason
	t.cancelWaiter(cid, &r)
	return ask.clone(), true
}

// validateAnswer ports Question.validate_answer against the raw question map.
// Returns an error if the answer is invalid for this question, else nil. A nil
// question (plain ask) is always valid.
func validateAnswer(question map[string]any, ans Answer) error {
	switch ans.Outcome {
	case "timed_out", "cancelled", "acknowledged", "denied":
		return nil
	}
	if question == nil {
		return nil
	}
	kind, _ := question["kind"].(string)
	if kind != "choice" {
		// text / acknowledge kinds are permissive.
		return nil
	}
	if ans.OptionID == nil {
		return errors.New("choice question requires an option_id")
	}
	if !questionHasOption(question, *ans.OptionID) {
		return fmt.Errorf("unknown option_id: %s", *ans.OptionID)
	}
	return nil
}

// questionHasOption reports whether the question's options carry id.
func questionHasOption(question map[string]any, id string) bool {
	opts, ok := question["options"].([]any)
	if !ok {
		return false
	}
	for _, o := range opts {
		m, ok := o.(map[string]any)
		if !ok {
			continue
		}
		if oid, ok := m["id"].(string); ok && oid == id {
			return true
		}
	}
	return false
}

// Answer records the typed answer to a question ask (first valid wins), closes
// it as "answered", and resolves any waiter. Returns ErrAskNotFound,
// ErrAlreadyAnswered, or a validation error.
func (t *AskTracker) Answer(ctx context.Context, cid string, ans Answer) (*Ask, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ask, ok := t.asks[cid]
	if !ok {
		return nil, ErrAskNotFound
	}
	// Guard on closed, not just answer-present: an ask can be closed by another
	// terminal path with no answer recorded. First terminal state wins.
	if ask.Closed || ask.Answer != nil {
		return nil, ErrAlreadyAnswered
	}
	if err := validateAnswer(ask.Question, ans); err != nil {
		return nil, err
	}
	a := ans
	ask.Answer = &a
	if ans.Text != nil && ask.ReplyText == nil {
		txt := *ans.Text
		ask.ReplyText = &txt
		now := time.Now().UTC()
		ask.RepliedAt = &now
	}
	ask.Closed = true
	ask.CloseReason = "answered"
	if w, ok := t.answerWaiters[cid]; ok {
		delete(t.answerWaiters, cid)
		w <- a
	}
	return ask.clone(), nil
}

// CaptureReply records an ack message body on an ask (for ask-many / pull). No-op
// if already captured.
func (t *AskTracker) CaptureReply(ctx context.Context, cid, replyText string, attachments []map[string]any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ask, ok := t.asks[cid]
	if !ok || ask.ReplyText != nil {
		return
	}
	txt := replyText
	ask.ReplyText = &txt
	if len(attachments) > 0 {
		ask.ReplyAttachments = attachments
	}
	now := time.Now().UTC()
	ask.RepliedAt = &now
}

// waiterFor returns the existing waiter channel for cid or creates one. Caller
// MUST hold t.mu.
func (t *AskTracker) waiterFor(cid string) chan Answer {
	if w, ok := t.answerWaiters[cid]; ok {
		return w
	}
	w := make(chan Answer, 1)
	t.answerWaiters[cid] = w
	return w
}

// WaitForAnswer blocks until cid is answered or applies defaultAnswer on timeout
// (the timeout path goes through Answer so the ledger stays truthful). A
// non-positive timeout waits indefinitely (bounded only by ctx). Used by
// blocking transports / the /query compat shim.
func (t *AskTracker) WaitForAnswer(ctx context.Context, cid string, timeout time.Duration, defaultAnswer *Answer) (Answer, error) {
	resolvedDefault := Answer{Outcome: "timed_out"}
	if defaultAnswer != nil {
		resolvedDefault = *defaultAnswer
	}
	t.mu.Lock()
	ask, ok := t.asks[cid]
	if !ok {
		t.mu.Unlock()
		return Answer{}, ErrAskNotFound
	}
	if ask.Answer != nil {
		a := *ask.Answer
		t.mu.Unlock()
		return a, nil
	}
	if ask.Closed {
		reason := ask.CloseReason
		t.mu.Unlock()
		return Answer{Outcome: "cancelled", Message: &reason}, nil
	}
	// Fail loud NOW if the default is invalid for this question, rather than
	// silently returning an unrecorded answer at timeout: the timeout path must
	// always go through Answer().
	if err := validateAnswer(ask.Question, resolvedDefault); err != nil {
		t.mu.Unlock()
		return Answer{}, fmt.Errorf("invalid default_answer: %w", err)
	}
	w := t.waiterFor(cid)
	t.mu.Unlock()

	var timer <-chan time.Time
	if timeout > 0 {
		tm := time.NewTimer(timeout)
		defer tm.Stop()
		timer = tm.C
	}
	select {
	case ans := <-w:
		return ans, nil
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	case <-timer:
		if _, err := t.Answer(ctx, cid, resolvedDefault); err != nil {
			// A real answer landed in the race window, or the ask was evicted
			// (which already cancelled this waiter). Return the recorded answer
			// if there is one.
			if existing, ok := t.Get(cid); ok && existing.Answer != nil {
				return *existing.Answer, nil
			}
		}
		return resolvedDefault, nil
	}
}

// WaitForResolution is a bounded wait that records NOTHING on timeout (ask stays
// open, returns (nil,nil)). Switches the ask to pull delivery when pull=true so a
// racing ack retains the reply instead of injecting into the blocked asker's pane.
func (t *AskTracker) WaitForResolution(ctx context.Context, cid string, timeout time.Duration, pull bool) (*Ask, error) {
	t.mu.Lock()
	ask, ok := t.asks[cid]
	if !ok {
		t.mu.Unlock()
		return nil, ErrAskNotFound
	}
	if ask.Closed {
		snap := ask.clone()
		t.mu.Unlock()
		return snap, nil
	}
	if pull {
		ask.ReplyDelivery = "pull"
	}
	w := t.waiterFor(cid)
	t.mu.Unlock()

	var timer <-chan time.Time
	if timeout > 0 {
		tm := time.NewTimer(timeout)
		defer tm.Stop()
		timer = tm.C
	}
	select {
	case <-w:
		// Resolved; return the latest snapshot.
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer:
		return nil, nil
	}
	if snap, ok := t.Get(cid); ok {
		return snap, nil
	}
	return nil, nil
}

// SetPendingReply stashes a completed reply for later redelivery (ACP asker
// offline). Returns true if the ask exists and was open (or an already-answered
// question when allowAnsweredQuestion). Captures the identity tuple for rebind.
func (t *AskTracker) SetPendingReply(ctx context.Context, cid, framedReply string, identity *AskerIdentity, allowAnsweredQuestion bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ask, ok := t.asks[cid]
	if !ok {
		return false
	}
	if ask.Closed {
		// Escape hatch: an already-answered structured question may carry a
		// pending redelivery without reopening. Arbitrary closed asks may not.
		answeredQuestion := allowAnsweredQuestion &&
			ask.Question != nil &&
			ask.Answer != nil &&
			ask.CloseReason == "answered"
		if !answeredQuestion {
			return false
		}
	}
	reply := framedReply
	ask.PendingReply = &reply
	ask.AskerIdentity = identity
	now := time.Now().UTC()
	ask.PendingReplyAt = &now
	return true
}

// MarkPendingReplyDelivered marks a stashed reply delivered and clears it. Open
// asks are closed with reason; an already-answered structured question stays
// closed as "answered" and only its transient stash is cleared. newFrom (the
// pass-1/generic rebind) rewrites FromPeerID when non-nil.
func (t *AskTracker) MarkPendingReplyDelivered(ctx context.Context, cid string, newFrom *proto.PeerID, reason string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ask, ok := t.asks[cid]
	if !ok || ask.PendingReply == nil {
		return false
	}
	if newFrom != nil {
		ask.FromPeerID = *newFrom
	}
	if !ask.Closed {
		ask.Closed = true
		ask.CloseReason = reason
		r := reason
		t.cancelWaiter(cid, &r)
	}
	ask.PendingReply = nil
	return true
}

// RebindAndClose atomically rewrites FromPeerID→newFrom, closes, and clears the
// stash (pass-2 success). Returns true if the ask was open and the mutation
// applied; false if missing or already closed (no mutation).
func (t *AskTracker) RebindAndClose(ctx context.Context, cid string, newFrom proto.PeerID, reason string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ask, ok := t.asks[cid]
	if !ok || ask.Closed {
		return false
	}
	ask.FromPeerID = newFrom
	ask.Closed = true
	ask.CloseReason = reason
	ask.PendingReply = nil
	r := reason
	t.cancelWaiter(cid, &r)
	return true
}

// ClearPendingReply drops the stashed reply from an ask (idempotent), so a closed
// Ask doesn't retain reply text it can never use again.
func (t *AskTracker) ClearPendingReply(ctx context.Context, cid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ask, ok := t.asks[cid]; ok {
		ask.PendingReply = nil
	}
}

// TakePendingRepliesForAsker snapshots (no mutation) asks whose FromPeerID==id
// and carry a stash — pass-1 same-id redelivery. We snapshot rather than drain so
// a failed redelivery leaves the reply in place for the next reconnect.
func (t *AskTracker) TakePendingRepliesForAsker(id proto.PeerID) []*Ask {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*Ask
	for _, ask := range t.asks {
		if ask.FromPeerID == id && ask.PendingReply != nil {
			out = append(out, ask.clone())
		}
	}
	return out
}

// TakeOrphanPendingRepliesMatching is the pure filter for pass-2 identity rebind:
// asks with a stash whose AskerIdentity matches the tuple exactly and whose
// FromPeerID is NOT in livePeerIDs. No liveness/uniqueness gating here.
func (t *AskTracker) TakeOrphanPendingRepliesMatching(id AskerIdentity, livePeerIDs map[proto.PeerID]struct{}) []*Ask {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*Ask
	for _, ask := range t.asks {
		if ask.PendingReply == nil {
			continue
		}
		ident := ask.AskerIdentity
		if ident == nil {
			continue
		}
		if ident.DisplayName != id.DisplayName ||
			ident.Circle != id.Circle ||
			ident.Backend != id.Backend ||
			ident.Path != id.Path ||
			ident.Machine != id.Machine {
			continue
		}
		if _, live := livePeerIDs[ask.FromPeerID]; live {
			continue
		}
		out = append(out, ask.clone())
	}
	return out
}

// SnapshotPendingRepliesForPeer is a pure read: asks involving this peer (either
// endpoint) that carry a stashed reply. The registry emits pending_reply_lost
// from this before forget/evict.
func (t *AskTracker) SnapshotPendingRepliesForPeer(id proto.PeerID) []*Ask {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*Ask
	for _, ask := range t.asks {
		if ask.PendingReply != nil && (ask.ToPeerID == id || ask.FromPeerID == id) {
			out = append(out, ask.clone())
		}
	}
	return out
}

// SnapshotExpiredPendingReplies is a pure read: TTL-expired asks that still carry
// a stashed reply. The registry emits pending_reply_lost from this before
// EvictExpired deletes the entries (single owner of TTL-loss emission).
func (t *AskTracker) SnapshotExpiredPendingReplies() []*Ask {
	cutoff := time.Now().UTC().Add(-t.ttl)
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*Ask
	for _, ask := range t.asks {
		if ask.CreatedAt.Before(cutoff) && ask.PendingReply != nil {
			out = append(out, ask.clone())
		}
	}
	return out
}

// PendingForPeer returns open asks for a peer, newest first, capped. direction ∈
// {"inbound","outbound","both"}. Opportunistically evicts TTL-expired asks at
// most once per evictionInterval (skipping stashed-reply asks — those are the
// registry's to reap). This is the /asks/pending Stop-hook reminder source.
func (t *AskTracker) PendingForPeer(ctx context.Context, id proto.PeerID, maxResults int, direction string) ([]*Ask, error) {
	switch direction {
	case "inbound", "outbound", "both":
	default:
		return nil, fmt.Errorf("invalid direction: %q", direction)
	}
	t.maybeEvictExpired(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	matches := func(ask *Ask) bool {
		switch direction {
		case "inbound":
			return ask.ToPeerID == id
		case "outbound":
			return ask.FromPeerID == id
		default:
			return ask.ToPeerID == id || ask.FromPeerID == id
		}
	}
	var candidates []*Ask
	for _, ask := range t.asks {
		if matches(ask) && !ask.Closed {
			candidates = append(candidates, ask.clone())
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
	})
	if maxResults >= 0 && len(candidates) > maxResults {
		candidates = candidates[:maxResults]
	}
	return candidates, nil
}

// maybeEvictExpired runs TTL eviction if enough wall time has passed since the
// last sweep. Skips stashed-reply asks: those are owned exclusively by the
// registry's lazy_repair (snapshot → emit pending_reply_lost → evict). If this
// Stop-hook-triggered path dropped stashed-expired asks first, the loss would be
// silent.
func (t *AskTracker) maybeEvictExpired(ctx context.Context) {
	now := time.Now()
	t.mu.Lock()
	if !t.lastEviction.IsZero() && now.Sub(t.lastEviction) < evictionInterval {
		t.mu.Unlock()
		return
	}
	t.lastEviction = now
	t.mu.Unlock()
	t.EvictExpired(ctx, false)
}

// ForgetPeer drops every ask involving id (registry prune path). Returns count.
func (t *AskTracker) ForgetPeer(ctx context.Context, id proto.PeerID) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	var doomed []string
	for cid, ask := range t.asks {
		if ask.ToPeerID == id || ask.FromPeerID == id {
			doomed = append(doomed, cid)
		}
	}
	for _, cid := range doomed {
		ask := t.asks[cid]
		if !ask.Closed {
			ask.Closed = true
			ask.CloseReason = "evicted"
		}
		t.cancelWaiter(cid, nil)
		delete(t.asks, cid)
	}
	return len(doomed)
}

// EvictExpired drops asks older than ttl; includeStashed=false leaves
// stashed-reply asks for the registry-owned loss path. Closes them as 'evicted'
// before removal so any holder of a reference sees why they vanished. Returns
// count.
func (t *AskTracker) EvictExpired(ctx context.Context, includeStashed bool) int {
	cutoff := time.Now().UTC().Add(-t.ttl)
	t.mu.Lock()
	defer t.mu.Unlock()
	var expired []string
	for cid, ask := range t.asks {
		if ask.CreatedAt.Before(cutoff) && (includeStashed || ask.PendingReply == nil) {
			expired = append(expired, cid)
		}
	}
	for _, cid := range expired {
		ask := t.asks[cid]
		if !ask.Closed {
			ask.Closed = true
			ask.CloseReason = "evicted"
		}
		t.cancelWaiter(cid, nil)
		delete(t.asks, cid)
	}
	return len(expired)
}

// BeginQuiesce atomically verifies no open asks for the peer, then marks it
// quiescing so register() refuses new asks in either direction. Exclusive: a
// concurrent BeginQuiesce for the same peer returns ErrQuiesced. Returns
// ErrQuiesceHasOpen if open asks exist. Pair with EndQuiesce in a defer.
func (t *AskTracker) BeginQuiesce(ctx context.Context, id proto.PeerID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.quiescing[id]; ok {
		return ErrQuiesced
	}
	for _, ask := range t.asks {
		if !ask.Closed && (ask.ToPeerID == id || ask.FromPeerID == id) {
			return ErrQuiesceHasOpen
		}
	}
	t.quiescing[id] = struct{}{}
	return nil
}

// EndQuiesce releases the switch barrier. Idempotent.
func (t *AskTracker) EndQuiesce(ctx context.Context, id proto.PeerID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.quiescing, id)
}

// OpenCount is the number of open (non-closed) asks. For diagnostics.
func (t *AskTracker) OpenCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, ask := range t.asks {
		if !ask.Closed {
			n++
		}
	}
	return n
}

// TotalCount is the total asks tracked (open + closed-but-retained). For diagnostics.
func (t *AskTracker) TotalCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.asks)
}
