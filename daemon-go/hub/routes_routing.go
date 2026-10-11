package hub

// Ask routing and its research ledger. Every ask attempt can record a
// route_decision row (a frozen snapshot of the sender, the declared target,
// and every peer's attested and claimed state) into its own delivery trace;
// outcomes land as the trace's later stages. In live mode, asks addressed to
// @route are routed by the configured arm and fail closed to the sender's
// fallback_to target. The routed peer still passes CheckAccess.

import (
	"context"
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

// beginDecision snapshots the mesh for an ask. nil when nothing will read it:
// not recording, and not a live @route ask.
func (h *Hub) beginDecision(ctx context.Context, a askAttempt, liveRoute bool) *routing.Decision {
	r := h.askRouter()
	if !r.Recording() && !liveRoute {
		return nil
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
	if h.ask == nil || h.ask.reg == nil {
		return d
	}
	reg := h.ask.reg
	clock, _ := reg.(descriptionClock)
	sender, _ := reg.GetPeerByName(a.fromPeer, nil)
	var stats map[proto.PeerID]service.AskPeerStats
	if h.ask != nil && h.ask.asks != nil && sender != nil {
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
	peers := reg.GetAllPeers()
	d.RegistrySize = len(peers)
	for _, p := range peers {
		if sender != nil && p.PeerID == sender.PeerID {
			continue
		}
		c := snap(p)
		switch {
		case peer.CircleAccess(sender, p, a.bypass) != nil:
			c.Access = routing.AccessCircleBoundary
		case proto.RequireAddressable(sender, p) != nil:
			c.Access = routing.AccessNotAddressable
		default:
			c.Access = routing.AccessOK
		}
		d.Candidates = append(d.Candidates, c)
	}
	sort.Slice(d.Candidates, func(i, j int) bool { return d.Candidates[i].PeerID < d.Candidates[j].PeerID })
	return d
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

// recordDecision writes the route_decision row (seq 0 of the ask's trace).
func (h *Hub) recordDecision(ctx context.Context, cid string, d *routing.Decision, target *proto.Peer, fromID proto.PeerID) {
	if d == nil || !h.askRouter().Recording() || h.deliveryTraces == nil {
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
	_ = h.deliveryTraces.RecordTrace(ctx, cid, "ask", routing.Stage, "", cid, toID, string(fromID), row.Detail())
}

// recordRouteFailure closes a decision whose ask never registered.
func (h *Hub) recordRouteFailure(ctx context.Context, cid string, d *routing.Decision, fromID proto.PeerID, reason string, err error) {
	if d == nil || !h.askRouter().Recording() || h.deliveryTraces == nil {
		return
	}
	h.recordDecision(ctx, cid, d, nil, fromID)
	detail := map[string]any{"reason": reason}
	if err != nil {
		detail["error"] = clipTraceText(err.Error())
	}
	_ = h.deliveryTraces.RecordTrace(ctx, cid, "ask", "route_failed", "fail", cid, "", string(fromID), detail)
}

// planShadow samples the shadow arms for a recorded explicit ask.
func (h *Hub) planShadow(d *routing.Decision) {
	r := h.askRouter()
	if d == nil || !r.Recording() || d.Declared.Resolution != routing.ResolutionResolved {
		return
	}
	d.ShadowPlanned = append(d.ShadowPlanned, r.ShadowPlan()...)
	if len(d.ShadowPlanned) > 0 {
		d.ShadowSampleEvery = map[string]int{}
		for _, arm := range d.ShadowPlanned {
			d.ShadowSampleEvery[arm] = r.SampleEvery(arm)
		}
	}
}

// runShadow runs the planned shadow arms off the request path and records
// each result, success or fallback, as a route_shadow row.
func (h *Hub) runShadow(cid string, d *routing.Decision, toID, fromID proto.PeerID) {
	if d == nil || len(d.ShadowPlanned) == 0 || h.deliveryTraces == nil {
		return
	}
	in := routing.InputFrom(cid, *d)
	h.routing.shadow.Add(1)
	go func() {
		defer h.routing.shadow.Done()
		ctx := context.Background()
		for _, arm := range d.ShadowPlanned {
			res := h.askRouter().Run(ctx, arm, in)
			_ = h.deliveryTraces.RecordTrace(ctx, cid, "ask", routing.ShadowStage, "", cid, string(toID), string(fromID), res.Detail())
		}
	}()
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
