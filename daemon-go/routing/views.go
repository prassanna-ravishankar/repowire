package routing

import (
	"fmt"
	"sort"
	"time"
)

// View is the slice of candidate state an arm may read. It is the experiment's
// independent variable: claimed is what peers say about themselves, attested
// is what the daemon observed, both is the union.
type View string

const (
	ViewClaimed  View = "claimed"
	ViewAttested View = "attested"
	ViewBoth     View = "both"
)

// Input is everything an arm is given for one decision. Candidates holds only
// eligible peers (access ok), already in a deterministic order.
type Input struct {
	TraceID    string
	DecidedAt  time.Time
	Request    Request
	Sender     *Candidate
	Candidates []Candidate
}

// InputFrom rebuilds an arm input from a recorded Decision, so live routing and
// static replay feed arms identically.
func InputFrom(traceID string, d Decision) Input {
	at, _ := time.Parse(time.RFC3339Nano, d.DecidedAt)
	in := Input{TraceID: traceID, DecidedAt: at, Request: d.Request, Sender: d.Sender}
	for _, c := range d.Candidates {
		if c.Access == AccessOK {
			in.Candidates = append(in.Candidates, c)
		}
	}
	sort.Slice(in.Candidates, func(i, j int) bool { return in.Candidates[i].PeerID < in.Candidates[j].PeerID })
	return in
}

// key is the opaque label a model sees for candidate i. Display names and peer
// ids are withheld: they would let an arm match the sender's wording to a name
// instead of judging fit, and they are not part of either view.
func key(i int) string { return fmt.Sprintf("c%d", i+1) }

// Project renders the input as the data an arm under view may read, and the
// key → peer_id map to read its answer back.
func Project(in Input, view View) (map[string]any, map[string]string) {
	keys := map[string]string{}
	candidates := map[string]any{}
	for i, c := range in.Candidates {
		keys[key(i)] = c.PeerID
		candidates[key(i)] = fields(c, view, in.DecidedAt)
	}
	state := map[string]any{"ask": in.Request.Text, "candidates": candidates}
	if in.Sender != nil {
		state["sender"] = fields(*in.Sender, view, in.DecidedAt)
	}
	return state, keys
}

func fields(c Candidate, view View, now time.Time) map[string]any {
	out := map[string]any{}
	if view == ViewClaimed || view == ViewBoth {
		out["description"] = c.Description
	}
	if view == ViewAttested || view == ViewBoth {
		out["backend"] = c.Backend
		out["project"] = c.Project
		out["path"] = c.Path
		out["role"] = c.Role
		out["status"] = c.Status
		out["turn_state"] = c.TurnState
		out["last_seen_age_s"] = roundAge(c.LastSeenAgeS)
		out["open_asks_inbound"] = c.OpenAsksInbound
		out["last_exchange_with_sender_age_s"] = ageSince(c.LastExchangeAt, now)
	}
	if view == ViewBoth {
		out["description_age_s"] = roundAge(c.DescriptionAgeS)
	}
	return out
}

func roundAge(v *float64) any {
	if v == nil {
		return nil
	}
	return int64(*v)
}

func ageSince(ts *string, now time.Time) any {
	if ts == nil || now.IsZero() {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *ts)
	if err != nil {
		return nil
	}
	return int64(now.Sub(t).Seconds())
}
