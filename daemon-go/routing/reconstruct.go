package routing

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

// Reconstruction rebuilds the routing Input of asks that predate route_decision
// rows, from the events journal alone. The journal has no registry snapshots,
// no descriptions and (for the ask window of the first dataset) no peer_online
// or peer_status rows, so every field below is an inference from observed
// activity. Each rule is named in the option that parameterises it and in
// docs/reference/cli.md; the result is attested-minus-description: the claimed
// view is empty by construction.

// ReconstructOptions parameterise the inferences. Zero values take defaults.
type ReconstructOptions struct {
	// LiveWindow: a peer counts as online at t only if its last observed
	// activity was within this window and no offline/reaped event followed it.
	LiveWindow time.Duration
	// ExchangeTTL bounds last_exchange_with_sender_at and open_asks_inbound,
	// matching the live AskTracker's 24h memory.
	ExchangeTTL time.Duration
	// WorkingWindow: a peer whose last assistant chat turn ended within this
	// window is "working", else "idle".
	WorkingWindow time.Duration
}

// DefaultReconstructOptions are the values the paper's numbers use.
func DefaultReconstructOptions() ReconstructOptions {
	return ReconstructOptions{LiveWindow: 24 * time.Hour, ExchangeTTL: 24 * time.Hour, WorkingWindow: 5 * time.Minute}
}

// ReconstructTypes are the journal event types reconstruction reads.
var ReconstructTypes = []string{"ask", "ack", "notification", "chat_turn", "peer_offline", "peer_reaped"}

// Exclusion reasons, applied in this order; an ask is counted under the first
// that matches.
const (
	ExcludeMachineSender = "sender_machine_dispatch"
	ExcludeSelfTarget    = "self_target"
	ExcludeServiceTarget = "service_target"
	ExcludeNoTargetID    = "no_target_peer_id"
)

// Evidence labels for whether the declared target was online at t.
const (
	EvidenceOnline  = "online"
	EvidenceNoPrior = "no_prior_evidence"
	EvidenceOffline = "offline_after_last_activity"
	EvidenceStale   = "stale_beyond_window"
)

const (
	hubPrefix           = "orchestrator"
	reconstructedMode   = "reconstructed"
	reconstructedSource = "events"
	roleAgent           = "agent"
	roleService         = "service"
)

// Strata are the fields an analysis splits reconstructed rows on.
type Strata struct {
	// HubBound: the declared target's name starts with "orchestrator".
	HubBound bool `json:"hub_bound"`
	// Recency is the #418 stratum, computed over the reconstructed candidates.
	Recency string `json:"recency"`
	// TargetOnline: the declared target was in the reconstructed candidate
	// set. Rows with false are never scored.
	TargetOnline   bool   `json:"target_online_at_t"`
	TargetEvidence string `json:"target_evidence"`
}

// ReconstructedRow is one JSONL line of a reconstructed dataset.
type ReconstructedRow struct {
	TraceID  string   `json:"trace_id"`
	Decision Decision `json:"decision"`
	Strata   Strata   `json:"strata"`
}

// Example converts the row for replay.
func (r ReconstructedRow) Example() Example {
	s := r.Strata
	return Example{TraceID: r.TraceID, Decision: r.Decision, Strata: &s}
}

// SizeDist summarises candidate-set sizes.
type SizeDist struct {
	Min, P25, Median, P75, P90, Max int
	Mean                            float64
}

// Summary is the accounting of one reconstruction: every ask lands in exactly
// one of Excluded, TargetNotOnline or Scored.
type Summary struct {
	Asks            int            `json:"asks"`
	From            string         `json:"from,omitempty"`
	To              string         `json:"to,omitempty"`
	Excluded        map[string]int `json:"excluded"`
	TargetNotOnline map[string]int `json:"target_not_online"` // "<evidence>/<hub_bound|worker_bound>"
	Scorable        int            `json:"scorable"`
	// Strata counts scorable rows by "<hub_bound|worker_bound>/<recency>".
	Strata              map[string]int `json:"strata"`
	CandidateSize       SizeDist       `json:"candidate_size"`
	DistinctTargets     int            `json:"distinct_targets_peer_id"`
	DistinctTargetNames int            `json:"distinct_targets_display_name"`
	DistinctSenders     int            `json:"distinct_senders_peer_id"`
}

func side(hub bool) string {
	if hub {
		return "hub_bound"
	}
	return "worker_bound"
}

// Reconstruct rebuilds a Decision for every ask in events that is not
// excluded. Rows whose declared target was not online at t are returned too,
// flagged by Strata.TargetOnline, so the dead-target bucket stays visible.
// events must be sorted by time (state.EventsOfTypes does).
func Reconstruct(events []state.EventRow, opts ReconstructOptions) ([]ReconstructedRow, Summary) {
	def := DefaultReconstructOptions()
	if opts.LiveWindow <= 0 {
		opts.LiveWindow = def.LiveWindow
	}
	if opts.ExchangeTTL <= 0 {
		opts.ExchangeTTL = def.ExchangeTTL
	}
	if opts.WorkingWindow <= 0 {
		opts.WorkingWindow = def.WorkingWindow
	}
	h := indexEvents(events)
	sum := Summary{Excluded: map[string]int{}, TargetNotOnline: map[string]int{}, Strata: map[string]int{}}
	var rows []ReconstructedRow
	var sizes []int
	targets, targetNames, senders := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range events {
		if e.Type != "ask" {
			continue
		}
		sum.Asks++
		from, fromID := str(e.Payload, "from"), str(e.Payload, "from_peer_id")
		to, toID := str(e.Payload, "to"), str(e.Payload, "to_peer_id")
		switch {
		case isMachine(from):
			sum.Excluded[ExcludeMachineSender]++
			continue
		case boolean(e.Payload, "self_target") || (toID != "" && toID == fromID):
			sum.Excluded[ExcludeSelfTarget]++
			continue
		case toID == "":
			sum.Excluded[ExcludeNoTargetID]++
			continue
		case isServiceName(to):
			sum.Excluded[ExcludeServiceTarget]++
			continue
		}
		t := e.At
		d := Decision{
			Schema: SchemaV1, Surface: SurfaceAsk, DecidedAt: t.UTC().Format(time.RFC3339Nano),
			Mode: reconstructedMode, Arm: ArmExplicit, SenderRaw: from,
			Declared: Declared{Raw: &to, PeerID: &toID, Resolution: ResolutionResolved},
			Request:  NewRequest(str(e.Payload, "text"), nil, optStr(e.Payload, "reply_to"), false, false, listLen(e.Payload, "attachments")),
			Chosen:   &Chosen{PeerID: toID, DisplayName: to},
		}
		sender := h.candidate(fromID, fromID, t, opts)
		sender.Access = ""
		d.Sender = &sender
		targetState := h.liveness(toID, t, opts.LiveWindow)
		for id := range h.peers {
			if id == fromID {
				continue
			}
			if st := h.liveness(id, t, opts.LiveWindow); st.evidence != EvidenceOnline || isMachine(h.nameAt(id, t)) {
				continue
			}
			d.Candidates = append(d.Candidates, h.candidate(id, fromID, t, opts))
		}
		sort.Slice(d.Candidates, func(i, j int) bool { return d.Candidates[i].PeerID < d.Candidates[j].PeerID })
		d.RegistrySize = len(d.Candidates) + 1
		hub := strings.HasPrefix(to, hubPrefix)
		in := InputFrom(str(e.Payload, "correlation_id"), d)
		st := Strata{
			HubBound: hub, TargetEvidence: targetState.evidence,
			TargetOnline: targetState.evidence == EvidenceOnline,
			Recency:      recencyStratum(in, DeclaredIsRecentContact(in, toID)),
		}
		rows = append(rows, ReconstructedRow{TraceID: in.TraceID, Decision: d, Strata: st})
		if !st.TargetOnline {
			sum.TargetNotOnline[st.TargetEvidence+"/"+side(hub)]++
			continue
		}
		sum.Scorable++
		sum.Strata[side(hub)+"/"+st.Recency]++
		sizes = append(sizes, len(in.Candidates))
		targets[toID], targetNames[to], senders[fromID] = true, true, true
		if sum.From == "" || d.DecidedAt < sum.From {
			sum.From = d.DecidedAt
		}
		if d.DecidedAt > sum.To {
			sum.To = d.DecidedAt
		}
	}
	sum.CandidateSize = sizeDist(sizes)
	sum.DistinctTargets, sum.DistinctTargetNames, sum.DistinctSenders = len(targets), len(targetNames), len(senders)
	return rows, sum
}

func sizeDist(sizes []int) SizeDist {
	if len(sizes) == 0 {
		return SizeDist{}
	}
	sorted := append([]int(nil), sizes...)
	sort.Ints(sorted)
	var total int
	for _, n := range sorted {
		total += n
	}
	q := func(p float64) int { return sorted[int(p*float64(len(sorted)-1))] }
	return SizeDist{Min: sorted[0], P25: q(.25), Median: q(.5), P75: q(.75), P90: q(.9), Max: sorted[len(sorted)-1], Mean: float64(total) / float64(len(sorted))}
}

// WriteSummary prints the accounting as a table.
func WriteSummary(w io.Writer, s Summary) {
	fmt.Fprintf(w, "asks in journal            %d\n", s.Asks)
	for _, k := range sortedKeys(s.Excluded) {
		fmt.Fprintf(w, "  excluded %-26s %d\n", k, s.Excluded[k])
	}
	for _, k := range sortedKeys(s.TargetNotOnline) {
		fmt.Fprintf(w, "  target not online %-26s %d\n", k, s.TargetNotOnline[k])
	}
	fmt.Fprintf(w, "scorable                   %d (%s to %s)\n", s.Scorable, s.From, s.To)
	for _, k := range sortedKeys(s.Strata) {
		fmt.Fprintf(w, "  %-44s %d\n", k, s.Strata[k])
	}
	c := s.CandidateSize
	fmt.Fprintf(w, "candidates per row: min %d p25 %d median %d p75 %d p90 %d max %d mean %.1f\n", c.Min, c.P25, c.Median, c.P75, c.P90, c.Max, c.Mean)
	fmt.Fprintf(w, "distinct targets: %d peer_ids, %d display names; %d distinct senders\n", s.DistinctTargets, s.DistinctTargetNames, s.DistinctSenders)
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// peerLog is everything the journal says about one peer_id.
type peerLog struct {
	names   []nameObs
	active  []time.Time // events the peer itself produced
	offline []time.Time // peer_offline / peer_reaped
	turns   []turn
}

type nameObs struct {
	at   time.Time
	name string
}

type turn struct {
	at        time.Time
	assistant bool
}

type inbound struct {
	at  time.Time
	cid string
}

// history indexes the journal for as-of-t queries. All queries are strict:
// an event at exactly t is not yet known at t.
type history struct {
	peers    map[string]*peerLog
	pairs    map[[2]string][]time.Time
	inbound  map[string][]inbound
	closedAt map[string]time.Time // ask correlation id -> earliest ack or reply_to
}

func indexEvents(events []state.EventRow) *history {
	h := &history{peers: map[string]*peerLog{}, pairs: map[[2]string][]time.Time{}, inbound: map[string][]inbound{}, closedAt: map[string]time.Time{}}
	log := func(id string) *peerLog {
		p := h.peers[id]
		if p == nil {
			p = &peerLog{}
			h.peers[id] = p
		}
		return p
	}
	name := func(id, n string, at time.Time) {
		if id == "" || n == "" {
			return
		}
		p := log(id)
		if len(p.names) == 0 || p.names[len(p.names)-1].name != n {
			p.names = append(p.names, nameObs{at, n})
		}
	}
	pair := func(a, b string, at time.Time) {
		if a != "" && b != "" && a != b {
			k := [2]string{a, b}
			if b < a {
				k = [2]string{b, a}
			}
			h.pairs[k] = append(h.pairs[k], at)
		}
	}
	closeAsk := func(cid string, at time.Time) {
		if cid == "" {
			return
		}
		if prev, ok := h.closedAt[cid]; !ok || at.Before(prev) {
			h.closedAt[cid] = at
		}
	}
	for _, e := range events {
		p := e.Payload
		fromID, toID := str(p, "from_peer_id"), str(p, "to_peer_id")
		switch e.Type {
		case "ask", "ack", "notification":
			if fromID != "" {
				pl := log(fromID)
				pl.active = append(pl.active, e.At)
			}
			name(fromID, str(p, "from"), e.At)
			name(toID, str(p, "to"), e.At)
			pair(fromID, toID, e.At)
			switch e.Type {
			case "ask":
				if !boolean(p, "self_target") && toID != "" {
					h.inbound[toID] = append(h.inbound[toID], inbound{e.At, str(p, "correlation_id")})
				}
				closeAsk(str(p, "reply_to"), e.At)
			case "ack":
				closeAsk(str(p, "correlation_id"), e.At)
			}
		case "chat_turn":
			id := e.PeerID
			if id == "" {
				continue
			}
			pl := log(id)
			pl.active = append(pl.active, e.At)
			pl.turns = append(pl.turns, turn{e.At, str(p, "role") == "assistant"})
			name(id, firstNonEmpty(e.PeerName, str(p, "peer")), e.At)
		case "peer_offline", "peer_reaped":
			id := firstNonEmpty(e.PeerID, str(p, "peer_id"))
			if id == "" {
				continue
			}
			log(id).offline = append(log(id).offline, e.At)
			name(id, firstNonEmpty(e.PeerName, str(p, "display_name")), e.At)
		}
	}
	return h
}

type liveState struct {
	last     time.Time
	evidence string
}

// liveness classifies peer id at t from its own activity and offline events.
func (h *history) liveness(id string, t time.Time, window time.Duration) liveState {
	p := h.peers[id]
	if p == nil {
		return liveState{evidence: EvidenceNoPrior}
	}
	last, ok := lastBefore(p.active, t)
	if !ok {
		return liveState{evidence: EvidenceNoPrior}
	}
	j := sort.Search(len(p.offline), func(i int) bool { return p.offline[i].After(last) })
	switch {
	case j < len(p.offline) && p.offline[j].Before(t):
		return liveState{last, EvidenceOffline}
	case t.Sub(last) > window:
		return liveState{last, EvidenceStale}
	}
	return liveState{last, EvidenceOnline}
}

// lastBefore is the latest time strictly before t in the sorted slice.
func lastBefore(times []time.Time, t time.Time) (time.Time, bool) {
	i := sort.Search(len(times), func(i int) bool { return !times[i].Before(t) })
	if i == 0 {
		return time.Time{}, false
	}
	return times[i-1], true
}

// nameAt is the display name the peer carried at t: the latest observation at
// or before t, else the earliest known.
func (h *history) nameAt(id string, t time.Time) string {
	p := h.peers[id]
	if p == nil || len(p.names) == 0 {
		return id
	}
	name := p.names[0].name
	for _, o := range p.names {
		if o.at.After(t) {
			break
		}
		name = o.name
	}
	return name
}

// candidate snapshots peer id as seen by sender at t.
func (h *history) candidate(id, sender string, t time.Time, opts ReconstructOptions) Candidate {
	name := h.nameAt(id, t)
	project, backend := SplitPeerName(name)
	role := roleAgent
	if isServiceName(name) {
		role = roleService
	}
	c := Candidate{
		PeerID: id, DisplayName: name, Role: role, Backend: backend, Project: project,
		Status: "online", Addressable: true, Source: reconstructedSource, Access: AccessOK,
	}
	if live := h.liveness(id, t, opts.LiveWindow); !live.last.IsZero() {
		c.LastSeen, c.LastSeenAgeS = stamp(&live.last, t)
	}
	if p := h.peers[id]; p != nil {
		if i := sort.Search(len(p.turns), func(i int) bool { return !p.turns[i].at.Before(t) }); i > 0 {
			last := p.turns[i-1]
			c.TurnState = "idle"
			if last.assistant && t.Sub(last.at) <= opts.WorkingWindow {
				c.TurnState = "working"
			}
		}
	}
	for _, a := range h.inbound[id] {
		if a.at.Before(t) && t.Sub(a.at) <= opts.ExchangeTTL {
			if closed, ok := h.closedAt[a.cid]; !ok || !closed.Before(t) {
				c.OpenAsksInbound++
			}
		}
	}
	k := [2]string{id, sender}
	if sender < id {
		k = [2]string{sender, id}
	}
	if last, ok := lastBefore(h.pairs[k], t); ok && t.Sub(last) <= opts.ExchangeTTL {
		c.LastExchangeAt, _ = stamp(&last, t)
	}
	return c
}

var backendSuffixes = []string{"claude-code", "codex", "opencode", "pi"}

// SplitPeerName parses the naming convention <project>[-<n>]-<backend>.
// A name without a known backend suffix is all project, backend empty.
func SplitPeerName(name string) (project, backend string) {
	project = name
	for _, b := range backendSuffixes {
		if rest, ok := strings.CutSuffix(name, "-"+b); ok {
			project, backend = rest, b
			break
		}
	}
	if i := strings.LastIndexByte(project, '-'); i > 0 && i < len(project)-1 && strings.Trim(project[i+1:], "0123456789") == "" {
		project = project[:i]
	}
	return project, backend
}

// Machine identities dispatch jobs; service peers are phone/relay bridges.
var (
	machineProjects = map[string]bool{"jobs": true, "daemon-job-runner": true}
	serviceProjects = map[string]bool{"jobs": true, "telegram": true, "dashboard": true, "slack": true}
)

func isMachine(name string) bool { p, _ := SplitPeerName(name); return machineProjects[p] }

// isServiceName reports a peer that is infrastructure, not a worker.
func isServiceName(name string) bool { p, _ := SplitPeerName(name); return serviceProjects[p] }

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func optStr(m map[string]any, k string) *string {
	if s := str(m, k); s != "" {
		return &s
	}
	return nil
}

func boolean(m map[string]any, k string) bool { b, _ := m[k].(bool); return b }

func listLen(m map[string]any, k string) int { l, _ := m[k].([]any); return len(l) }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// WriteRows writes one JSON line per reconstructed row.
func WriteRows(w io.Writer, rows []ReconstructedRow) error {
	enc := json.NewEncoder(w)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// ReadExamples reads a reconstructed dataset back into replay examples.
func ReadExamples(r io.Reader) ([]Example, error) {
	dec := json.NewDecoder(r)
	var out []Example
	for dec.More() {
		var row ReconstructedRow
		if err := dec.Decode(&row); err != nil {
			return nil, fmt.Errorf("reconstructed row %d: %w", len(out)+1, err)
		}
		if row.Decision.Schema != SchemaV1 {
			return nil, fmt.Errorf("reconstructed row %d: schema %q, want %q", len(out)+1, row.Decision.Schema, SchemaV1)
		}
		out = append(out, row.Example())
	}
	return out, nil
}
