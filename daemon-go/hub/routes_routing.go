package hub

// Ask routing and its research ledger. Every ask attempt can record a
// route_decision row (a frozen snapshot of the sender, the declared target,
// and every peer's attested and claimed state) into its own delivery trace;
// outcomes land as the trace's later stages. In live mode, asks addressed to
// @route are routed by the configured arm and fail closed to the sender's
// fallback_to target. The routed peer still passes CheckAccess.

import (
	"context"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/routing"
	"github.com/repowire/repowire/daemon-go/service"
)

// AskRouting tells the sender what the router did with a routed ask.
type AskRouting struct {
	Arm            string   `json:"arm"`
	RoutedTo       string   `json:"routed_to"`
	RoutedToPeerID string   `json:"routed_to_peer_id"`
	Confidence     *float64 `json:"confidence,omitempty"`
	Fallback       bool     `json:"fallback"`
	FallbackReason string   `json:"fallback_reason,omitempty"`
}

// routingDeps holds the router and the in-flight shadow runs.
type routingDeps struct {
	router *routing.Router
	shadow sync.WaitGroup
}

// WithRouting wires the ask router. Unwired is the same as mode off with
// recording off.
func (h *Hub) WithRouting(r *routing.Router) *Hub {
	h.routing = &routingDeps{router: r}
	return h
}

func (h *Hub) askRouter() *routing.Router {
	if h.routing == nil {
		return nil
	}
	return h.routing.router
}

// waitShadow blocks until in-flight shadow arms have recorded. Tests only.
func (h *Hub) waitShadow() {
	if h.routing != nil {
		h.routing.shadow.Wait()
	}
}

// askAttempt is one ask's routing record while it is being resolved.
type askAttempt struct {
	surface  string
	fromPeer string
	circle   *string
	bypass   bool
	text     string
	replyTo  *string
	question bool
	attach   int
	parentID *string
}

// beginDecision mints the ask's cid and snapshots the mesh, but only when
// something will read it: recording on, or a live @route ask. Otherwise it
// returns ("", nil) and touches nothing, so the uninstrumented path is the
// pre-router path (the tracker mints the cid at Register, as before).
func (h *Hub) beginDecision(_ context.Context, a askAttempt, liveRoute bool) (string, *routing.Decision) {
	r := h.askRouter()
	if !r.Recording() && !liveRoute {
		return "", nil
	}
	now := time.Now().UTC()
	d := &routing.Decision{
		Schema: routing.SchemaV1, Surface: a.surface, ParentID: a.parentID,
		DecidedAt: now.Format(time.RFC3339Nano), Mode: r.Mode(), Arm: routing.ArmExplicit,
		SenderRaw: a.fromPeer, ShadowPlanned: []string{},
		Request:    routing.NewRequest(a.text, a.circle, a.replyTo, a.bypass, a.question, a.attach),
		Declared:   routing.Declared{Resolution: routing.ResolutionAbsent},
		Candidates: []routing.Candidate{},
	}
	h.snapshotCandidates(d, h.resolveSender(a.fromPeer, a.circle), a.bypass, now)
	return service.NewAskID(), d
}

// resolveSender resolves the sender as CheckAccess does before a target is
// known: circle-scoped first, then unscoped, with an ambiguous name left
// unresolved (nil) rather than guessed.
func (h *Hub) resolveSender(raw string, circle *string) *proto.Peer {
	if h.ask == nil || h.ask.reg == nil {
		return nil
	}
	if circle != nil {
		if p, err := h.ask.reg.GetPeerByName(raw, circle); err == nil && p != nil {
			return p
		}
	}
	p, err := h.ask.reg.GetPeerByName(raw, nil)
	if err != nil {
		return nil
	}
	return p
}

// rebaseSender re-snapshots against the sender CheckAccess actually resolved,
// when it differs from the pre-resolution guess. Explicit asks only: a live
// decision keeps the candidate set its arm saw.
func (h *Hub) rebaseSender(d *routing.Decision, from *proto.Peer, bypass bool) {
	if d == nil || from == nil || d.Router != nil || (d.Sender != nil && d.Sender.PeerID == string(from.PeerID)) {
		return
	}
	at, err := time.Parse(time.RFC3339Nano, d.DecidedAt)
	if err != nil {
		at = time.Now().UTC()
	}
	h.snapshotCandidates(d, from, bypass, at)
}

// snapshotCandidates fills the sender and candidate snapshot. With no
// resolved sender, every peer the raw sender string names is excluded so the
// sender can never appear as its own candidate.
func (h *Hub) snapshotCandidates(d *routing.Decision, sender *proto.Peer, bypass bool, now time.Time) {
	d.Sender, d.Candidates, d.RegistrySize = nil, []routing.Candidate{}, 0
	if h.ask == nil || h.ask.reg == nil {
		return
	}
	reg := h.ask.reg
	clock, _ := reg.(descriptionClock)
	var stats map[proto.PeerID]service.AskPeerStats
	if h.ask.asks != nil && sender != nil {
		stats = h.ask.asks.RoutingStats(sender.PeerID)
	}
	snap := func(p *proto.Peer) routing.Candidate {
		st := stats[p.PeerID]
		var setAt *time.Time
		if clock != nil {
			setAt = clock.DescriptionSetAt(p.PeerID)
		}
		return routing.Snapshot(p, now, setAt, routing.PeerStats{OpenInbound: st.OpenInbound, LastExchange: st.LastExchange})
	}
	if sender != nil {
		s := snap(sender)
		d.Sender = &s
	}
	isSender := func(p *proto.Peer) bool {
		if sender != nil {
			return p.PeerID == sender.PeerID
		}
		return string(p.PeerID) == d.SenderRaw || string(p.DisplayName) == d.SenderRaw
	}
	peers := reg.GetAllPeers()
	d.RegistrySize = len(peers)
	for _, p := range peers {
		if isSender(p) {
			continue
		}
		c := snap(p)
		switch {
		case peer.CircleAccess(sender, p, bypass) != nil:
			c.Access = routing.AccessCircleBoundary
		case proto.RequireAddressable(sender, p) != nil:
			c.Access = routing.AccessNotAddressable
		default:
			c.Access = routing.AccessOK
		}
		d.Candidates = append(d.Candidates, c)
	}
	sort.Slice(d.Candidates, func(i, j int) bool { return d.Candidates[i].PeerID < d.Candidates[j].PeerID })
}

// declare records the sender's named target and how it resolved.
func declare(d *routing.Decision, raw string, target *proto.Peer, resolution string) {
	if d == nil || raw == "" {
		return
	}
	d.Declared = routing.Declared{Raw: &raw, Resolution: resolution}
	if target != nil {
		id := string(target.PeerID)
		d.Declared.PeerID = &id
	}
}

// auditWriteTimeout bounds one research-ledger write.
const auditWriteTimeout = 5 * time.Second

// audit writes one routing ledger row under a context detached from the
// request: a caller hanging up must not cost the ledger its row. Write
// failures are logged, never swallowed.
func (h *Hub) audit(ctx context.Context, cid, stage, status, toID, fromID string, detail map[string]any) {
	if h.deliveryTraces == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	if err := h.deliveryTraces.RecordTrace(ctx, cid, "ask", stage, status, cid, toID, fromID, detail); err != nil {
		log.Printf("routing ledger: %s %s not recorded: %v", cid, stage, err)
	}
}

// recordDecision writes the route_decision row (seq 0 of the ask's trace).
func (h *Hub) recordDecision(ctx context.Context, cid string, d *routing.Decision, target *proto.Peer, fromID proto.PeerID) {
	if d == nil || !h.askRouter().Recording() {
		return
	}
	if target != nil {
		d.Chosen = &routing.Chosen{PeerID: string(target.PeerID), DisplayName: string(target.DisplayName)}
	}
	row := *d
	if !h.askRouter().RecordText() {
		row.Request.Text, row.Request.TextRecorded = "", false
	}
	toID := ""
	if target != nil {
		toID = string(target.PeerID)
	}
	h.audit(ctx, cid, routing.Stage, "", toID, string(fromID), row.Detail())
}

// recordRouteFailure closes a decision whose ask never registered.
func (h *Hub) recordRouteFailure(ctx context.Context, cid string, d *routing.Decision, fromID proto.PeerID, reason string, err error) {
	if d == nil || !h.askRouter().Recording() {
		return
	}
	h.recordDecision(ctx, cid, d, nil, fromID)
	detail := map[string]any{"reason": reason}
	if err != nil {
		detail["error"] = clipTraceText(err.Error())
	}
	h.audit(ctx, cid, "route_failed", "fail", "", string(fromID), detail)
}

// planShadow samples the shadow arms for a recorded explicit ask and takes a
// shadow pool slot for them. When the pool is full the sampled arms are
// recorded on the decision itself as saturated, so overload costs the
// request path no extra ledger write. It reports whether a slot is held;
// the caller must hand it to runShadow.
func (h *Hub) planShadow(d *routing.Decision) bool {
	r := h.askRouter()
	if d == nil || !r.Recording() || d.Declared.Resolution != routing.ResolutionResolved {
		return false
	}
	d.ShadowPlanned = append(d.ShadowPlanned, r.ShadowPlan()...)
	if len(d.ShadowPlanned) == 0 {
		return false
	}
	d.ShadowSampleEvery = map[string]int{}
	for _, arm := range d.ShadowPlanned {
		d.ShadowSampleEvery[arm] = r.SampleEvery(arm)
	}
	if r.TryAcquireShadow() {
		return true
	}
	for _, arm := range d.ShadowPlanned {
		d.ShadowSkipped = append(d.ShadowSkipped, r.Skipped(arm, routing.FallbackSaturated))
	}
	return false
}

// runShadow runs the planned shadow arms off the request path, holding the
// pool slot planShadow took, and records each result, success or fallback,
// as a route_shadow row. A panic anywhere in the run is contained and the
// remaining arms are recorded.
func (h *Hub) runShadow(cid string, d *routing.Decision, slot bool, toID, fromID proto.PeerID) {
	if !slot {
		return
	}
	r := h.askRouter()
	in := routing.InputFrom(cid, *d)
	h.routing.shadow.Add(1)
	go func() {
		defer h.routing.shadow.Done()
		defer r.ReleaseShadow()
		for _, arm := range d.ShadowPlanned {
			res := h.runShadowArm(r, arm, in)
			h.audit(context.Background(), cid, routing.ShadowStage, "", string(toID), string(fromID), res.Detail())
		}
	}()
}

// runShadowArm is the per-arm panic boundary on the shadow goroutine.
func (h *Hub) runShadowArm(r *routing.Router, arm string, in routing.Input) (res routing.RouterResult) {
	defer func() {
		if p := recover(); p != nil {
			res = r.Skipped(arm, routing.FallbackPanic) // the panic value is not recorded
		}
	}()
	return r.Run(context.Background(), arm, in)
}

// askResolution is the coordinator's verdict on one ask: its cid (empty
// until Register when nothing is recorded), the sender and the authorized
// target, and how the target was chosen. selection is nil for a named target.
type askResolution struct {
	cid       string
	decision  *routing.Decision
	fromID    proto.PeerID
	fromName  proto.DisplayName
	target    *proto.Peer
	selection *routing.RouterResult
	slot      bool // a shadow pool slot is held for runShadow
}

// resolveAsk is the routing coordinator for /ask. It owns explicit-first
// dispatch, authorization and the ledger:
//
//   - a named target is resolved and authorized by resolveAskTarget, and the
//     strategy is never consulted, so no strategy can override a named peer;
//   - a targetless ask goes to the configured strategy, whose pick is only a
//     proposal: it is authorized through the same resolveAskTarget, and a
//     pick that fails authorization falls back to the sender's fallback_to,
//     which is authorized in turn.
//
// Every failure is recorded here; on success the decision row is written
// before the caller registers the ask.
func (h *Hub) resolveAsk(ctx context.Context, req AskRequest) (*askResolution, error) {
	r := h.askRouter()
	targetless := r.Strategy() != routing.StrategyStatic && routing.IsRouteTarget(req.ToPeer)
	// When routing is instrumented the cid is minted before resolution so a
	// failed attempt still has a trace; otherwise Register mints it as it
	// always has. reply_to closes a PRIOR ask; it is NOT this ask's cid.
	cid, decision := h.beginDecision(ctx, askAttempt{
		surface: routing.SurfaceAsk, fromPeer: req.FromPeer, circle: req.Circle, bypass: req.BypassCircle,
		text: req.Text, replyTo: req.ReplyTo, question: req.Question != nil, attach: len(req.Attachments),
	}, targetless)
	res := &askResolution{cid: cid, decision: decision}

	toPeer, declared := req.ToPeer, req.ToPeer
	if targetless {
		declared = req.FallbackTo
		toPeer, res.selection = h.routeLive(ctx, cid, decision, req.FallbackTo)
		if toPeer == "" {
			h.recordRouteFailure(ctx, cid, decision, proto.PeerID(req.FromPeer), "router_unavailable", nil)
			return nil, routeErr(http.StatusServiceUnavailable, map[string]any{
				"error": "router_unavailable", "arm": res.selection.Arm, "reason": res.selection.FallbackReason,
				"hint": "The router could not choose a peer and no fallback_to was given; nothing was delivered. Name a peer, or retry with fallback_to.",
			})
		}
	}
	from, target, reason, err := h.resolveAskTarget(ctx, req, toPeer, declared, decision)
	if err != nil && res.selection != nil && !res.selection.Fallback {
		// The arm's pick failed authorization (its snapshot went stale). That
		// is a router fallback whether or not there is somewhere to fall back
		// to; with fallback_to, deliver there rather than guess again.
		res.selection.Fallback, res.selection.FallbackReason = true, "stale_pick:"+reason
		if req.FallbackTo != "" {
			from, target, reason, err = h.resolveAskTarget(ctx, req, req.FallbackTo, declared, decision)
		}
	}
	res.fromID, res.fromName = proto.PeerID(req.FromPeer), proto.DisplayName(req.FromPeer)
	if from != nil {
		res.fromID, res.fromName = from.PeerID, from.DisplayName
	}
	if err != nil {
		h.recordRouteFailure(ctx, cid, decision, res.fromID, reason, err)
		return nil, err
	}
	res.target = target
	h.rebaseSender(decision, from, req.BypassCircle)
	if res.selection == nil {
		res.slot = h.planShadow(decision)
	}
	h.recordDecision(ctx, cid, decision, target, res.fromID)
	return res, nil
}

// recordQuiesced closes a recorded decision whose ask the tracker refused
// because an endpoint is mid-switch.
func (h *Hub) recordQuiesced(ctx context.Context, res *askResolution) {
	if res.decision != nil && h.askRouter().Recording() {
		h.audit(ctx, res.cid, "route_failed", "fail", string(res.target.PeerID), string(res.fromID), map[string]any{"reason": "quiesced"})
	}
}

// routeLive runs the live arm for an @route ask and returns the address to
// resolve next: the arm's pick, or the sender's fallback_to when the arm fell
// back (empty when the sender gave none).
func (h *Hub) routeLive(ctx context.Context, cid string, d *routing.Decision, fallbackTo string) (string, *routing.RouterResult) {
	r := h.askRouter()
	res := r.Run(ctx, r.LiveArm(), routing.InputFrom(cid, *d))
	d.Arm = res.Arm
	d.Router = &res
	if res.Fallback {
		return fallbackTo, &res
	}
	return res.Scores[0].PeerID, &res
}

// routedSummary is the sender-facing account of a routed ask.
func routedSummary(res *routing.RouterResult, target *proto.Peer) *AskRouting {
	if res == nil {
		return nil
	}
	out := &AskRouting{Arm: res.Arm, Confidence: res.Confidence, Fallback: res.Fallback, FallbackReason: res.FallbackReason}
	if target != nil {
		out.RoutedTo, out.RoutedToPeerID = string(target.DisplayName), string(target.PeerID)
	}
	return out
}

// eligibleCount is the number of candidates an arm chose among.
func eligibleCount(d *routing.Decision) int {
	n := 0
	for _, c := range d.Candidates {
		if c.Access == routing.AccessOK {
			n++
		}
	}
	return n
}

// closeOutcome adds outcome measures to a recorded ask's closed stage.
func (h *Hub) closeOutcome(ctx context.Context, ask *service.Ask, detail map[string]any) {
	if !h.askRouter().Recording() {
		return
	}
	now := time.Now().UTC()
	detail["time_to_close_ms"] = now.Sub(ask.CreatedAt).Milliseconds()
	if h.store != nil {
		if turns, err := h.store.AssistantTurnsBetween(ctx, string(ask.ToPeerID), ask.CreatedAt, now); err == nil {
			detail["recipient_turns"] = turns
		}
	}
}
