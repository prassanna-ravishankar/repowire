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
	"log"
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
	ErrAskResolving    = errors.New("ask: another resolution is in progress")
	ErrNotClaimed      = errors.New("ask: operation requires the ask's claim")
	ErrResultWaiting   = errors.New("ask: a result is already waiting for the asker")
	ErrNotHolder       = errors.New("ask: only its current recipient may hand it on")
	ErrDelegateTarget  = errors.New("ask: cannot delegate to its asker or current recipient")
	ErrDelegationLimit = errors.New("ask: delegation chain is full")
	// ErrFormerHolder refuses moving an ask back to a peer that held it: an
	// agent's resolution names only the correlation id and is matched to the
	// holder by peer id, so a returning holder's stale reply would look
	// current (rfcs/wire-actions.md).
	ErrFormerHolder = errors.New("ask: cannot move an ask back to a peer that held it")
)

// MaxDelegations bounds an ask's delegation chain.
const MaxDelegations = 8

// Delegation is one hop of an ask from one recipient to the next.
type Delegation struct {
	FromPeerID proto.PeerID
	FromName   proto.DisplayName
	ToPeerID   proto.PeerID
	ToName     proto.DisplayName
	Note       string
	At         time.Time
	Generation int // the assignment this hop began

	fromSessionID *string // restored if the hop is undone
}

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
// closeReason ∈ {ack, ack_with_msg, declined, reply_to, answered, evicted, send_failed}.
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
	Attachments           []map[string]any // the ask's own, re-sent on delegation
	// Delegations is the chain of hand-offs, oldest first; ToPeerID is the
	// last hop's recipient. Replaced, never mutated, so snapshots stay valid.
	Delegations []Delegation
	// Generation identifies the current assignment: 0 for the first
	// recipient, a fresh number for each hand-off. A runtime completion
	// carries the generation it was issued under, and any other one is
	// dropped. Numbers are never reused (issued only goes up, and undoing a
	// hop restores the previous holder's number), so a completion from a
	// hand-off that failed and was retried to the same peer cannot match.
	Generation int
	issued     int // the highest generation this ask has issued

	// done is closed once, when this ask instance reaches its terminal state,
	// waking every waiter; they then read what was recorded.
	done chan struct{}
	// evicted marks an instance evicted while claimed: it stays in the map,
	// closed, until Release, so its id cannot be reused under the claimant.
	evicted bool
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
	mu           sync.Mutex
	asks         map[string]*Ask
	resolving    map[string]struct{}
	deferred     map[string][]deferredWork // RunClaimed work waiting on a claim
	ttl          time.Duration
	lastEviction time.Time
	quiescing    map[proto.PeerID]struct{}
	onRegister   func(cid string, from, to proto.PeerID)
	closeGuard   func(ask *Ask, reason string) error
}

// SetCloseGuard installs a check every terminal transition runs before the
// ask is marked closed: Close, CloseInboundForPeer, ForgetPeer and
// EvictExpired. A guard error keeps the ask open (Close reports false; the
// bulk paths skip it), so a result that must be made durable first is never
// lost by closing ahead of the write. It runs under the tracker's lock and
// must not call back into the tracker.
func (t *AskTracker) SetCloseGuard(fn func(ask *Ask, reason string) error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeGuard = fn
}

// guardLocked reports whether ask may close with reason. Caller holds t.mu.
func (t *AskTracker) guardLocked(ask *Ask, reason string) bool {
	if ask.Closed || t.closeGuard == nil {
		return true
	}
	snapshot := ask.clone()
	snapshot.CloseReason = reason
	if err := t.closeGuard(snapshot, reason); err != nil {
		log.Printf("ask %s: close %s held open: %v", ask.CorrelationID, reason, err)
		return false
	}
	return true
}

// SetOnRegister installs a callback run once for each newly registered ask
// (not for a retry of an existing cid). It runs under the tracker's lock, so
// it must be quick and must not call back into the tracker.
func (t *AskTracker) SetOnRegister(fn func(cid string, from, to proto.PeerID)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onRegister = fn
}

// TTL is how long an ask may stay open.
func (t *AskTracker) TTL() time.Duration { return t.ttl }

// NewAskTracker returns an empty tracker. A non-positive ttl falls back to 24h.
func NewAskTracker(ttl time.Duration) *AskTracker {
	if ttl <= 0 {
		ttl = defaultAskTTL
	}
	return &AskTracker{
		asks:      make(map[string]*Ask),
		resolving: make(map[string]struct{}),
		deferred:  make(map[string][]deferredWork),
		ttl:       ttl,
		quiescing: make(map[proto.PeerID]struct{}),
	}
}

// NewAskID mints a fresh ask correlation id ("ask-<hex8>"). Callers that need
// the id before registering (to trace a failed resolution) mint it here.
func NewAskID() string { return "ask-" + hex8() }

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
	Attachments           []map[string]any
	ReplyDelivery         string // "" defaults to "push"
	// CreatedAt keeps a rehydrated ask's original age (zero means now), so a
	// restart does not reset its TTL.
	CreatedAt time.Time
	// SkipHistory keeps the ask out of the OnRegister callback (the durable
	// prior-ask counts): ask_many children, /query and blocking questions write
	// no ledger "created" stage, so counting them live would make the counts
	// disagree with themselves across a restart.
	SkipHistory bool
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
		cid = NewAskID()
	}
	if _, ok := t.asks[cid]; ok {
		// Retry: existing entry preserved (lifecycle state intact).
		return cid, nil
	}
	delivery := p.ReplyDelivery
	if delivery == "" {
		delivery = "push"
	}
	created := p.CreatedAt.UTC()
	if p.CreatedAt.IsZero() {
		created = time.Now().UTC()
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
		Attachments:           p.Attachments,
		ReplyDelivery:         delivery,
		CreatedAt:             created,
		done:                  make(chan struct{}),
	}
	if t.onRegister != nil && !p.SkipHistory {
		t.onRegister(cid, p.FromPeerID, p.ToPeerID)
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

// OpenAsks returns snapshot copies of every open ask.
func (t *AskTracker) OpenAsks() []*Ask {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*Ask, 0, len(t.asks))
	for _, ask := range t.asks {
		if !ask.Closed {
			out = append(out, ask.clone())
		}
	}
	return out
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

// closeLocked is every terminal transition: it marks ask closed with reason
// and wakes all its waiters. Caller holds t.mu. Idempotent.
func (t *AskTracker) closeLocked(ask *Ask, reason string) {
	if ask.Closed {
		return
	}
	ask.Closed = true
	ask.CloseReason = reason
	if ask.done != nil {
		close(ask.done)
	}
}

// recordedAnswer is what a waiter on a closed ask learns: its answer, or a
// cancellation naming why it closed without one.
func (t *AskTracker) recordedAnswer(ask *Ask) Answer {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ask.Answer != nil {
		return *ask.Answer
	}
	reason := ask.CloseReason
	return Answer{Outcome: "cancelled", Message: &reason}
}

// Close terminally closes an ask with reason. Returns (ask,true) if it existed
// and was open; (nil,false) if missing or already closed (idempotent). Cancels
// any blocking answer waiter.
func (t *AskTracker) Close(ctx context.Context, cid, reason string) (*Ask, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ask, ok := t.asks[cid]
	if !ok || ask.Closed || !t.guardLocked(ask, reason) {
		return nil, false
	}
	t.closeLocked(ask, reason)
	return ask.clone(), true
}

// Resolve closes an ask with its reply in one step: the guard sees the
// reason and reply together, and nothing changes unless it passes. Of two
// racing resolutions exactly one wins, whole; the loser reports false and
// leaves no trace on the ask.
func (t *AskTracker) Resolve(ctx context.Context, cid, reason string, reply *string, attachments []map[string]any) (*Ask, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ask, ok := t.asks[cid]
	if !ok || ask.Closed {
		return nil, false
	}
	if t.closeGuard != nil {
		snapshot := ask.clone()
		snapshot.CloseReason = reason
		if reply != nil {
			snapshot.ReplyText = reply
		}
		if err := t.closeGuard(snapshot, reason); err != nil {
			log.Printf("ask %s: resolve %s held open: %v", cid, reason, err)
			return nil, false
		}
	}
	if reply != nil && ask.ReplyText == nil {
		text := *reply
		now := time.Now().UTC()
		ask.ReplyText, ask.RepliedAt = &text, &now
		if len(attachments) > 0 {
			ask.ReplyAttachments = attachments
		}
	}
	t.closeLocked(ask, reason)
	return ask.clone(), true
}

// Claim takes an open ask for one holder operation (a reply, a decline, an
// answer, a delegation, a runtime's completion) and returns its snapshot. A
// second claim is refused with ErrAskResolving until Release;
// ErrAlreadyAnswered when the ask is closed, ErrAskNotFound when unknown.
//
// The invariant: an ask's recipient changes, and its holder resolves it, only
// under the claim. So checks made on the claimed snapshot (who holds it, is it
// a question) stay true until Release, and a reply reaches the asker at most
// once.
func (t *AskTracker) Claim(cid string) (*Ask, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ask, ok := t.asks[cid]
	switch {
	case !ok:
		return nil, ErrAskNotFound
	case ask.Closed:
		return nil, ErrAlreadyAnswered
	}
	if _, busy := t.resolving[cid]; busy {
		return nil, ErrAskResolving
	}
	t.resolving[cid] = struct{}{}
	return ask.clone(), nil
}

// Release ends a Claim and starts any RunClaimed work that waited on it.
// Idempotent.
func (t *AskTracker) Release(cid string) {
	t.mu.Lock()
	delete(t.resolving, cid)
	next := t.deferred[cid]
	delete(t.deferred, cid)
	if ask := t.asks[cid]; ask != nil && ask.evicted {
		delete(t.asks, cid)
		next = nil
	}
	t.mu.Unlock()
	for _, work := range next {
		go t.runClaimedOn(cid, work.ask, work.fn)
	}
}

// deferredWork is RunClaimed work queued behind a claim, bound to the ask
// instance it was queued for.
type deferredWork struct {
	ask *Ask
	fn  func(*Ask)
}

// RunClaimed runs fn with the claimed snapshot under cid's Claim: now when the
// ask is free, otherwise once the claim holding it is released. It never runs
// on an ask that is closed or gone by then. Holder work that must not be lost
// while a reply or a hand-off holds the ask (a runtime's completion, a
// question's timeout) waits here rather than polling or being dropped.
func (t *AskTracker) RunClaimed(cid string, fn func(*Ask)) {
	t.mu.Lock()
	ask := t.asks[cid]
	t.mu.Unlock()
	if ask != nil {
		t.runClaimedOn(cid, ask, fn)
	}
}

// runClaimedOn is RunClaimed bound to one ask instance: work queued for an ask
// that is later evicted never runs against a new ask registered under the
// same id.
func (t *AskTracker) runClaimedOn(cid string, ask *Ask, fn func(*Ask)) {
	t.mu.Lock()
	if t.asks[cid] != ask || ask.Closed {
		t.mu.Unlock()
		return
	}
	if t.claimedLocked(cid) {
		t.deferred[cid] = append(t.deferred[cid], deferredWork{ask: ask, fn: fn})
		t.mu.Unlock()
		return
	}
	t.resolving[cid] = struct{}{}
	snapshot := ask.clone()
	t.mu.Unlock()
	defer t.Release(cid)
	fn(snapshot)
}

// claimedLocked reports whether cid is claimed. Caller holds t.mu.
func (t *AskTracker) claimedLocked(cid string) bool {
	_, held := t.resolving[cid]
	return held
}

// DelegateParams hands an open ask to a new recipient.
type DelegateParams struct {
	// By is who asks for the hand-off; "" is an unidentified admin caller.
	// When set it must be the current recipient.
	By          proto.PeerID
	To          proto.PeerID
	ToName      proto.DisplayName
	ToSessionID *string
	Note        string
}

// Delegate retargets an open ask to p.To, keeping its correlation id, and
// appends the hop. The asker, the reply path and the reply mode are
// unchanged. The caller holds the ask's Claim through delivery, so it can
// UndoDelegation the returned hop if delivery fails.
func (t *AskTracker) Delegate(ctx context.Context, cid string, p DelegateParams) (*Ask, Delegation, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ask, ok := t.asks[cid]
	switch {
	case !ok:
		return nil, Delegation{}, ErrAskNotFound
	case ask.Closed:
		return nil, Delegation{}, ErrAlreadyAnswered
	case !t.claimedLocked(cid):
		return nil, Delegation{}, ErrNotClaimed
	case p.By != "" && p.By != ask.ToPeerID:
		return nil, Delegation{}, ErrNotHolder
	case p.To == ask.ToPeerID || p.To == ask.FromPeerID:
		return nil, Delegation{}, ErrDelegateTarget
	case ask.HeldBefore(p.To):
		return nil, Delegation{}, ErrFormerHolder
	case len(ask.Delegations) >= MaxDelegations:
		return nil, Delegation{}, ErrDelegationLimit
	case ask.PendingReply != nil:
		// The holder's result exists and waits for the asker; moving the ask
		// would send it under the new holder's name.
		return nil, Delegation{}, ErrResultWaiting
	}
	if _, q := t.quiescing[p.To]; q {
		return nil, Delegation{}, ErrQuiesced
	}
	hop := Delegation{
		FromPeerID: ask.ToPeerID, FromName: ask.ToPeerName,
		ToPeerID: p.To, ToName: p.ToName, Note: p.Note, At: time.Now().UTC(),
		Generation:    ask.issued + 1,
		fromSessionID: ask.ToRepowireSessionID,
	}
	ask.issued = hop.Generation
	ask.Generation = hop.Generation
	ask.Delegations = append(append([]Delegation(nil), ask.Delegations...), hop)
	ask.ToPeerID, ask.ToPeerName, ask.ToRepowireSessionID = p.To, p.ToName, p.ToSessionID
	return ask.clone(), hop, nil
}

// UndoDelegation reverts hop, under the Claim that made it, returning the ask
// to the delegator. It reports false, changing nothing, when the ask was
// closed or evicted meanwhile (by a path that does not claim) or hop is not
// its last one.
func (t *AskTracker) UndoDelegation(ctx context.Context, cid string, hop Delegation) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ask, ok := t.asks[cid]
	if !ok || ask.Closed || len(ask.Delegations) == 0 || !t.claimedLocked(cid) {
		return false
	}
	last := ask.Delegations[len(ask.Delegations)-1]
	if last.ToPeerID != hop.ToPeerID || !last.At.Equal(hop.At) || ask.ToPeerID != hop.ToPeerID {
		return false
	}
	ask.Delegations = append([]Delegation(nil), ask.Delegations[:len(ask.Delegations)-1]...)
	ask.ToPeerID, ask.ToPeerName, ask.ToRepowireSessionID = hop.FromPeerID, hop.FromName, hop.fromSessionID
	ask.Generation = 0 // the previous holder's assignment; issued stays, so hop's number is never reused
	if n := len(ask.Delegations); n > 0 {
		ask.Generation = ask.Delegations[n-1].Generation
	}
	return true
}

// ReplyFrame is the head of a reply on this ask's thread, as the asker sees
// it: "[ack #cid from @C]", or "[ack #cid from @C via @A, @B]" after
// delegation.
func (a *Ask) ReplyFrame(kind string) string {
	frame := fmt.Sprintf("[%s #%s from @%s", kind, a.CorrelationID, a.ToPeerName)
	for i, hop := range a.Delegations {
		sep := ", @"
		if i == 0 {
			sep = " via @"
		}
		frame += sep + string(hop.FromName)
	}
	return frame + "]"
}

// HeldBefore reports whether id held the ask before a delegation moved it on.
func (a *Ask) HeldBefore(id proto.PeerID) bool {
	for _, hop := range a.Delegations {
		if hop.FromPeerID == id {
			return true
		}
	}
	return false
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
// ErrAlreadyAnswered, ErrAskResolving while the ask is claimed, or a
// validation error.
func (t *AskTracker) Answer(ctx context.Context, cid string, ans Answer) (*Ask, error) {
	return t.answer(cid, ans, false)
}

// AnswerClaimed is Answer for a caller holding the ask's Claim.
func (t *AskTracker) AnswerClaimed(ctx context.Context, cid string, ans Answer) (*Ask, error) {
	return t.answer(cid, ans, true)
}

func (t *AskTracker) answer(cid string, ans Answer, claimed bool) (*Ask, error) {
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
	if t.claimedLocked(cid) != claimed {
		if claimed {
			return nil, ErrNotClaimed
		}
		return nil, ErrAskResolving
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
	t.closeLocked(ask, "answered")
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
	t.mu.Unlock()

	var timer <-chan time.Time
	if timeout > 0 {
		tm := time.NewTimer(timeout)
		defer tm.Stop()
		timer = tm.C
	}
	select {
	case <-ask.done:
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	case <-timer:
		// Commit the default under this ask's claim, after any reply or
		// hand-off holding it. Whatever closes the ask first (this default,
		// a real answer, another waiter's default, a close or an eviction)
		// closes done, and every waiter returns what was recorded.
		t.runClaimedOn(cid, ask, func(*Ask) { _, _ = t.AnswerClaimed(ctx, cid, resolvedDefault) })
		select {
		case <-ask.done:
		case <-ctx.Done():
			return Answer{}, ctx.Err()
		}
	}
	return t.recordedAnswer(ask), nil
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
	t.mu.Unlock()

	var timer <-chan time.Time
	if timeout > 0 {
		tm := time.NewTimer(timeout)
		defer tm.Stop()
		timer = tm.C
	}
	select {
	case <-ask.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer:
		return nil, nil
	}
	// Resolved: the snapshot of this instance, even if it has since been
	// evicted from the map.
	t.mu.Lock()
	defer t.mu.Unlock()
	return ask.clone(), nil
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
	t.closeLocked(ask, reason)
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
	t.closeLocked(ask, reason)
	ask.PendingReply = nil
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
// CloseInboundForPeer closes every open ask addressed to id with reason and
// cancels its waiters with that reason. The records stay (they are closed, not
// forgotten) so history and traces keep the cid.
func (t *AskTracker) CloseInboundForPeer(id proto.PeerID, reason string) []ClosedInboundAsk {
	t.mu.Lock()
	defer t.mu.Unlock()
	var closed []ClosedInboundAsk
	for cid, ask := range t.asks {
		if ask.ToPeerID != id || ask.Closed || !t.guardLocked(ask, reason) {
			continue
		}
		t.closeLocked(ask, reason)
		closed = append(closed, ClosedInboundAsk{CorrelationID: cid, FromPeerID: ask.FromPeerID})
	}
	return closed
}

// evictLocked closes cid as evicted and drops it, with any work queued for it.
// A claimed ask is closed now but dropped at Release: until then its id stays
// taken, so a claimant's last write (a close after a send, a timeout's
// default) can never land on a new ask registered under the same id. Caller
// holds t.mu.
func (t *AskTracker) evictLocked(cid string) {
	ask := t.asks[cid]
	t.closeLocked(ask, "evicted")
	delete(t.deferred, cid)
	if t.claimedLocked(cid) {
		ask.evicted = true
		return
	}
	delete(t.asks, cid)
}

// ClosedInboundAsk is what CloseInboundForPeer reports per closed ask.
type ClosedInboundAsk struct {
	CorrelationID string
	FromPeerID    proto.PeerID
}

func (t *AskTracker) ForgetPeer(ctx context.Context, id proto.PeerID) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	var doomed []string
	for cid, ask := range t.asks {
		if (ask.ToPeerID == id || ask.FromPeerID == id) && t.guardLocked(ask, "evicted") {
			doomed = append(doomed, cid)
		}
	}
	for _, cid := range doomed {
		t.evictLocked(cid)
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
		if ask.CreatedAt.Before(cutoff) && (includeStashed || ask.PendingReply == nil) && t.guardLocked(ask, "evicted") {
			expired = append(expired, cid)
		}
	}
	for _, cid := range expired {
		t.evictLocked(cid)
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

// AskPeerStats is the tracker's view of one peer relative to a given sender.
type AskPeerStats struct {
	OpenInbound  int        // open asks addressed to the peer, from anyone
	LastExchange *time.Time // newest ask between the sender and the peer, either direction
}

// RoutingStats summarises load and recent contact for every peer the tracker
// knows about, relative to sender. Bounded by the tracker TTL.
func (t *AskTracker) RoutingStats(sender proto.PeerID) map[proto.PeerID]AskPeerStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[proto.PeerID]AskPeerStats{}
	touch := func(peer proto.PeerID, at time.Time) {
		st := out[peer]
		if st.LastExchange == nil || at.After(*st.LastExchange) {
			at := at
			st.LastExchange = &at
		}
		out[peer] = st
	}
	for _, a := range t.asks {
		if !a.Closed {
			st := out[a.ToPeerID]
			st.OpenInbound++
			out[a.ToPeerID] = st
		}
		at := a.CreatedAt
		if a.RepliedAt != nil {
			at = *a.RepliedAt
		}
		switch sender {
		case a.FromPeerID:
			touch(a.ToPeerID, at)
		case a.ToPeerID:
			touch(a.FromPeerID, at)
		}
	}
	return out
}
