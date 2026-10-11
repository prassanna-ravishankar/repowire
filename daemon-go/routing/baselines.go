package routing

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Replay-only baselines. They read the recorded ledger, cost nothing, and are
// the controls an attested arm has to beat: if jev-attested cannot beat
// "whoever the sender talked to last" or "whoever the sender usually asks",
// its advantage was recency or habit, not state.

// FallbackNoHistory: a baseline had nothing to rank on (no recorded exchange,
// or no prior asks from this sender). Counted as a miss like any fallback.
const FallbackNoHistory = "no_history"

// Baselines are the arms replay computes locally, always re-run.
var Baselines = []string{"random", "recent", "majority"}

// IsBaseline reports whether name is a replay-computed baseline.
func IsBaseline(name string) bool {
	for _, b := range Baselines {
		if b == name {
			return true
		}
	}
	return false
}

// BaselineArms builds the baselines over the replayed window.
func BaselineArms(examples []Example) map[string]Arm {
	return map[string]Arm{"random": Random{}, "recent": Recent{}, "majority": NewMajority(examples)}
}

// Recent ranks candidates by their last exchange with the sender, newest
// first. Candidates with no recorded exchange are left unranked.
type Recent struct{}

func (Recent) Name() string { return "recent" }

func (Recent) Rank(_ context.Context, in Input) (RouterResult, error) {
	now := in.DecidedAt
	if now.IsZero() {
		now = time.Now()
	}
	var scores []Score
	for _, c := range in.Candidates {
		if t, ok := exchangedAt(c); ok {
			age := max(now.Sub(t).Seconds(), 0)
			scores = append(scores, Score{PeerID: c.PeerID, Score: 1 / (1 + age)})
		}
	}
	if len(scores) == 0 {
		return RouterResult{}, fail(FallbackNoHistory, "no candidate has a recorded exchange with the sender")
	}
	return RouterResult{Scores: scores}, nil
}

func exchangedAt(c Candidate) (time.Time, bool) {
	if c.LastExchangeAt == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, *c.LastExchangeAt)
	return t, err == nil
}

// DeclaredIsRecentContact reports whether the declared target is the
// candidate (or tied for the candidate) the sender exchanged with most
// recently. False when no candidate has a recorded exchange.
func DeclaredIsRecentContact(in Input, declared string) bool {
	var latest, mine time.Time
	for _, c := range in.Candidates {
		if t, ok := exchangedAt(c); ok {
			if t.After(latest) {
				latest = t
			}
			if c.PeerID == declared {
				mine = t
			}
		}
	}
	return !mine.IsZero() && mine.Equal(latest)
}

// Majority ranks candidates by how often the sender addressed them across the
// replayed window, leaving out the row being scored so its own label never
// votes for itself. Candidates the sender never addressed are left unranked.
type Majority struct {
	counts map[string]map[string]int // sender → declared → asks
	labels map[string]label          // trace → its own vote
}

type label struct{ sender, declared string }

// NewMajority counts every labelled explicit ask in examples.
func NewMajority(examples []Example) Majority {
	m := Majority{counts: map[string]map[string]int{}, labels: map[string]label{}}
	for _, ex := range examples {
		l, ok := labelOf(ex.Decision)
		if !ok {
			continue
		}
		if m.counts[l.sender] == nil {
			m.counts[l.sender] = map[string]int{}
		}
		m.counts[l.sender][l.declared]++
		m.labels[ex.TraceID] = l
	}
	return m
}

func labelOf(d Decision) (label, bool) {
	if d.Arm != ArmExplicit || d.Sender == nil || d.Declared.Resolution != ResolutionResolved || d.Declared.PeerID == nil {
		return label{}, false
	}
	return label{sender: d.Sender.PeerID, declared: *d.Declared.PeerID}, true
}

func (Majority) Name() string { return "majority" }

func (m Majority) Rank(_ context.Context, in Input) (RouterResult, error) {
	if in.Sender == nil {
		return RouterResult{}, fail(FallbackNoHistory, "no sender")
	}
	own, hasOwn := m.labels[in.TraceID]
	var scores []Score
	for _, c := range in.Candidates {
		n := m.counts[in.Sender.PeerID][c.PeerID]
		if hasOwn && own.sender == in.Sender.PeerID && own.declared == c.PeerID {
			n-- // leave one out
		}
		if n > 0 {
			scores = append(scores, Score{PeerID: c.PeerID, Score: float64(n)})
		}
	}
	if len(scores) == 0 {
		return RouterResult{}, fail(FallbackNoHistory, "sender has no other asks to a candidate in the window")
	}
	return RouterResult{Scores: scores}, nil
}

// maskable maps each --mask field (its Candidate JSON name) to how it is
// blanked. Identity and eligibility fields are not maskable.
var maskable = map[string]func(*Candidate){
	"description":                  func(c *Candidate) { c.Description = "" },
	"description_age_s":            func(c *Candidate) { c.DescriptionAgeS, c.DescriptionSetAt = nil, nil },
	"backend":                      func(c *Candidate) { c.Backend = "" },
	"project":                      func(c *Candidate) { c.Project = "" },
	"path":                         func(c *Candidate) { c.Path = "" },
	"role":                         func(c *Candidate) { c.Role = "" },
	"status":                       func(c *Candidate) { c.Status = "" },
	"turn_state":                   func(c *Candidate) { c.TurnState = "" },
	"last_seen_age_s":              func(c *Candidate) { c.LastSeenAgeS, c.LastSeen = nil, nil },
	"open_asks_inbound":            func(c *Candidate) { c.OpenAsksInbound = 0 },
	"last_exchange_with_sender_at": func(c *Candidate) { c.LastExchangeAt = nil },
}

// MaskFields lists the fields --mask accepts.
func MaskFields() []string {
	names := make([]string, 0, len(maskable))
	for name := range maskable {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ParseMask validates a comma-separated --mask value.
func ParseMask(raw string) ([]string, error) {
	var fields []string
	for _, f := range strings.Split(raw, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		if _, ok := maskable[f]; !ok {
			return nil, fmt.Errorf("unknown mask field %q (maskable: %s)", f, strings.Join(MaskFields(), ", "))
		}
		fields = append(fields, f)
	}
	return fields, nil
}

// Mask returns in with the named fields blanked on every candidate. The
// sender snapshot is left as recorded. Fields must come from ParseMask.
func Mask(in Input, fields []string) Input {
	if len(fields) == 0 {
		return in
	}
	candidates := make([]Candidate, len(in.Candidates))
	for i, c := range in.Candidates {
		for _, f := range fields {
			maskable[f](&c)
		}
		candidates[i] = c
	}
	in.Candidates = candidates
	return in
}
