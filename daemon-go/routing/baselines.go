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
// or no other asks from this sender that may vote). Counted as a miss like
// any fallback.
const FallbackNoHistory = "no_history"

// Baselines are the arms replay computes locally, always re-run.
var Baselines = []string{"random", "recent", "majority", "majority-past"}

// IsBaseline reports whether name is a replay-computed baseline.
func IsBaseline(name string) bool {
	for _, b := range Baselines {
		if b == name {
			return true
		}
	}
	return false
}

// BaselineArms builds the baselines; the majority baselines vote from history.
func BaselineArms(history []Example) map[string]Arm {
	return map[string]Arm{
		"random": Random{}, "recent": Recent{},
		"majority": NewMajority(history, false), "majority-past": NewMajority(history, true),
	}
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

// Majority ranks candidates by how often the sender addressed them among the
// replayed asks. The scored ask never votes for itself. Two variants:
//
//   - majority (whole window): every other ask in the window votes, including
//     asks made after the scored one. A retrospective ceiling for habit, not
//     a predictor anyone could deploy.
//   - majority-past: only asks decided strictly before the scored one vote,
//     so it is causal and the fair "deployable history" comparison.
//
// Candidates the sender never addressed are left unranked.
//
// Votes come from the examples passed to NewMajority, which may be wider than
// the rows being scored: the replay CLI passes the whole ledger, so --since
// and --limit narrow what is scored without starving the vote. Given only
// the scored window, majority-past is cold at the start of it.
type Majority struct {
	past   bool
	votes  map[string][]vote // sender → its labelled asks
	window History
}

// History describes the vote population, for disclosure beside results.
type History struct {
	Asks int    `json:"asks"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// History is the span of labelled asks this baseline votes from.
func (m Majority) History() History { return m.window }

type vote struct {
	trace, declared string
	at              time.Time
}

// NewMajority collects every labelled explicit ask in examples; past selects
// the causal variant.
func NewMajority(examples []Example, past bool) Majority {
	m := Majority{past: past, votes: map[string][]vote{}}
	for _, ex := range examples {
		d := ex.Decision
		if d.Arm != ArmExplicit || d.Sender == nil || d.Declared.Resolution != ResolutionResolved || d.Declared.PeerID == nil {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, d.DecidedAt)
		m.votes[d.Sender.PeerID] = append(m.votes[d.Sender.PeerID], vote{trace: ex.TraceID, declared: *d.Declared.PeerID, at: at})
		m.window.Asks++
		if m.window.From == "" || d.DecidedAt < m.window.From {
			m.window.From = d.DecidedAt
		}
		if d.DecidedAt > m.window.To {
			m.window.To = d.DecidedAt
		}
	}
	return m
}

func (m Majority) Name() string {
	if m.past {
		return "majority-past"
	}
	return "majority"
}

func (m Majority) Rank(_ context.Context, in Input) (RouterResult, error) {
	if in.Sender == nil {
		return RouterResult{}, fail(FallbackNoHistory, "no sender")
	}
	counts := map[string]int{}
	for _, v := range m.votes[in.Sender.PeerID] {
		if v.trace == in.TraceID || (m.past && (v.at.IsZero() || in.DecidedAt.IsZero() || !v.at.Before(in.DecidedAt))) {
			continue
		}
		counts[v.declared]++
	}
	var scores []Score
	for _, c := range in.Candidates {
		if n := counts[c.PeerID]; n > 0 {
			scores = append(scores, Score{PeerID: c.PeerID, Score: float64(n)})
		}
	}
	if len(scores) == 0 {
		return RouterResult{}, fail(FallbackNoHistory, "sender has no other asks to a candidate that may vote")
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
