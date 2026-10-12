package routing

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

var reconT0 = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func atT(d time.Duration) time.Time { return reconT0.Add(d) }

func evAsk(when time.Duration, cid, from, fromID, to, toID string, extra map[string]any) state.EventRow {
	p := map[string]any{"correlation_id": cid, "from": from, "from_peer_id": fromID, "to": to, "to_peer_id": toID, "text": "do " + cid, "self_target": false}
	for k, v := range extra {
		p[k] = v
	}
	return state.EventRow{Type: "ask", At: atT(when), Payload: p}
}

func evTurn(when time.Duration, id, name, role string) state.EventRow {
	return state.EventRow{Type: "chat_turn", At: atT(when), PeerID: id, PeerName: name, Payload: map[string]any{"role": role}}
}

func evAck(when time.Duration, cid, from, fromID, to, toID string) state.EventRow {
	return state.EventRow{Type: "ack", At: atT(when), Payload: map[string]any{"correlation_id": cid, "from": from, "from_peer_id": fromID, "to": to, "to_peer_id": toID}}
}

func evGone(when time.Duration, typ, id string) state.EventRow {
	return state.EventRow{Type: typ, At: atT(when), PeerID: id, Payload: map[string]any{}}
}

func rowsByTrace(rows []ReconstructedRow) map[string]ReconstructedRow {
	out := map[string]ReconstructedRow{}
	for _, r := range rows {
		out[r.TraceID] = r
	}
	return out
}

func candOf(t *testing.T, r ReconstructedRow, id string) Candidate {
	t.Helper()
	for _, c := range r.Decision.Candidates {
		if c.PeerID == id {
			return c
		}
	}
	t.Fatalf("%s: %s not a candidate", r.TraceID, id)
	return Candidate{}
}

func hasCand(r ReconstructedRow, id string) bool {
	for _, c := range r.Decision.Candidates {
		if c.PeerID == id {
			return true
		}
	}
	return false
}

func TestSplitPeerName(t *testing.T) {
	for name, want := range map[string][2]string{
		"moltbook-driver-3-claude-code":          {"moltbook-driver", "claude-code"},
		"orchestrator-codex":                     {"orchestrator", "codex"},
		"repowire.route-reconstruct-claude-code": {"repowire.route-reconstruct", "claude-code"},
		"agent-dj-5-codex":                       {"agent-dj", "codex"},
		"jobs":                                   {"jobs", ""},
		"mesh-health-api":                        {"mesh-health-api", ""},
		"thing-pi":                               {"thing", "pi"},
	} {
		if p, b := SplitPeerName(name); p != want[0] || b != want[1] {
			t.Errorf("SplitPeerName(%q) = %q, %q; want %q, %q", name, p, b, want[0], want[1])
		}
	}
}

func TestReconstructExclusionsAreCountedOnce(t *testing.T) {
	events := []state.EventRow{
		evTurn(0, "w1", "worker-claude-code", "assistant"),
		evAsk(time.Minute, "a-machine", "jobs", "j", "worker-claude-code", "w1", nil),
		evAsk(time.Minute, "a-self", "worker-claude-code", "w1", "worker-claude-code", "w1", map[string]any{"self_target": true}),
		evAsk(time.Minute, "a-service", "orchestrator-codex", "o1", "telegram-claude-code", "tg", nil),
		evAsk(time.Minute, "a-noid", "orchestrator-codex", "o1", "worker-claude-code", "", nil),
		evAsk(time.Minute, "a-ok", "orchestrator-codex", "o1", "worker-claude-code", "w1", nil),
	}
	rows, sum := Reconstruct(events, ReconstructOptions{})
	if sum.Asks != 5 || sum.Excluded[ExcludeMachineSender] != 1 || sum.Excluded[ExcludeSelfTarget] != 1 ||
		sum.Excluded[ExcludeServiceTarget] != 1 || sum.Excluded[ExcludeNoTargetID] != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	// a-ok: target w1 is online, the sender o1 has never been seen acting.
	if len(rows) != 1 || rows[0].TraceID != "a-ok" || sum.Scorable != 1 {
		t.Fatalf("rows = %+v, summary %+v", rows, sum)
	}
}

func TestReconstructLiveness(t *testing.T) {
	h := time.Hour
	events := []state.EventRow{
		evTurn(0, "fresh", "fresh-codex", "assistant"),
		evTurn(0, "stale", "stale-codex", "assistant"),
		evTurn(0, "left", "left-codex", "assistant"),
		evTurn(0, "reaped", "reaped-codex", "assistant"),
		evTurn(0, "jobs-id", "jobs", "assistant"),
		evGone(h, "peer_offline", "left"),
		evGone(2*h, "peer_reaped", "reaped"),
		evTurn(30*h, "fresh", "fresh-codex", "assistant"),
		evTurn(30*h, "orch", "orchestrator-codex", "assistant"),
		evAsk(31*h, "a1", "orchestrator-codex", "orch", "fresh-codex", "fresh", nil),
		evAsk(31*h, "a-stale", "orchestrator-codex", "orch", "stale-codex", "stale", nil),
		evAsk(31*h, "a-left", "orchestrator-codex", "orch", "left-codex", "left", nil),
		evAsk(31*h, "a-new", "orchestrator-codex", "orch", "unseen-codex", "unseen", nil),
	}
	rows, sum := Reconstruct(events, ReconstructOptions{LiveWindow: 24 * h})
	got := rowsByTrace(rows)
	if r := got["a1"]; !r.Strata.TargetOnline || r.Strata.TargetEvidence != EvidenceOnline {
		t.Fatalf("a1 strata = %+v", r.Strata)
	}
	for trace, want := range map[string]string{"a-stale": EvidenceStale, "a-left": EvidenceOffline, "a-new": EvidenceNoPrior} {
		if r := got[trace]; r.Strata.TargetOnline || r.Strata.TargetEvidence != want {
			t.Errorf("%s strata = %+v, want evidence %s", trace, r.Strata, want)
		}
	}
	if sum.Scorable != 1 || sum.TargetNotOnline[EvidenceStale+"/worker_bound"] != 1 || sum.TargetNotOnline[EvidenceOffline+"/worker_bound"] != 1 || sum.TargetNotOnline[EvidenceNoPrior+"/worker_bound"] != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	// Candidates at a1: only the live peers, never the sender or a machine id.
	r := got["a1"]
	for _, id := range []string{"stale", "left", "reaped", "jobs-id", "orch"} {
		if hasCand(r, id) {
			t.Errorf("%s must not be a candidate at a1", id)
		}
	}
	if !hasCand(r, "fresh") || len(r.Decision.Candidates) != 1 {
		t.Fatalf("candidates = %+v", r.Decision.Candidates)
	}
	// Activity after the offline event revives the peer.
	revived := append(events[:len(events):len(events)], evTurn(40*h, "left", "left-codex", "assistant"), evAsk(41*h, "a-back", "orchestrator-codex", "orch", "left-codex", "left", nil))
	rows, _ = Reconstruct(revived, ReconstructOptions{LiveWindow: 24 * h})
	if r := rowsByTrace(rows)["a-back"]; !r.Strata.TargetOnline {
		t.Fatalf("revived peer not online: %+v", r.Strata)
	}
}

func TestReconstructAttestedFields(t *testing.T) {
	m := time.Minute
	events := []state.EventRow{
		evTurn(0, "w1", "alpha-claude-code", "assistant"),
		evTurn(0, "w2", "beta-2-codex", "assistant"),
		evTurn(0, "orch", "orchestrator-codex", "assistant"),
		// w1 and the sender exchanged an hour ago; w2 and the sender never did.
		evAsk(-60*m, "old", "orchestrator-codex", "orch", "alpha-claude-code", "w1", nil),
		evAck(-59*m, "old", "alpha-claude-code", "w1", "orchestrator-codex", "orch"),
		// Two asks to w2 from someone else: one acked, one answered by reply_to, one open.
		evAsk(-30*m, "x1", "other-codex", "oth", "beta-2-codex", "w2", nil),
		evAck(-29*m, "x1", "beta-2-codex", "w2", "other-codex", "oth"),
		evAsk(-20*m, "x2", "other-codex", "oth", "beta-2-codex", "w2", nil),
		evAsk(-19*m, "x2r", "beta-2-codex", "w2", "other-codex", "oth", map[string]any{"reply_to": "x2"}),
		evAsk(-10*m, "x3", "other-codex", "oth", "beta-2-codex", "w2", nil),
		evTurn(-2*m, "w2", "beta-2-codex", "assistant"),
		evTurn(-6*m, "w1", "alpha-claude-code", "assistant"),
		// The ask under test, plus an event at exactly t that must stay invisible.
		evAsk(0, "target", "orchestrator-codex", "orch", "alpha-claude-code", "w1", nil),
		evAck(0, "x3", "beta-2-codex", "w2", "other-codex", "oth"),
	}
	rows, _ := Reconstruct(events, ReconstructOptions{})
	r := rowsByTrace(rows)["target"]
	if r.Strata.HubBound || !r.Strata.TargetOnline {
		t.Fatalf("strata = %+v", r.Strata)
	}
	w1, w2 := candOf(t, r, "w1"), candOf(t, r, "w2")
	if w1.Project != "alpha" || w1.Backend != "claude-code" || w2.Project != "beta" || w2.Backend != "codex" {
		t.Errorf("name parse: %+v %+v", w1, w2)
	}
	if w1.LastExchangeAt == nil || *w1.LastExchangeAt != atT(-59*m).Format(time.RFC3339Nano) || w2.LastExchangeAt != nil {
		t.Errorf("last exchange: w1 %v, w2 %v", w1.LastExchangeAt, w2.LastExchangeAt)
	}
	if w2.OpenAsksInbound != 1 || w1.OpenAsksInbound != 0 {
		t.Errorf("open inbound: w1 %d, w2 %d (want 0, 1: x3 is open, its ack lands at t)", w1.OpenAsksInbound, w2.OpenAsksInbound)
	}
	if w2.TurnState != "working" || w1.TurnState != "idle" {
		t.Errorf("turn state: w1 %q, w2 %q", w1.TurnState, w2.TurnState)
	}
	if w1.LastSeenAgeS == nil || *w1.LastSeenAgeS != (6*m).Seconds() {
		t.Errorf("last seen age = %v, want 360 (last activity is the turn 6m earlier)", w1.LastSeenAgeS)
	}
	if w1.Description != "" || w1.DescriptionAgeS != nil || w1.Path != "" || w1.Ephemeral || !w1.Addressable || w1.Access != AccessOK {
		t.Errorf("claimed/eligibility fields: %+v", w1)
	}
	if r.Decision.Sender == nil || r.Decision.Sender.PeerID != "orch" || r.Decision.Request.Text != "do target" {
		t.Errorf("sender/request: %+v", r.Decision)
	}
	// The exchange is older than the TTL when the TTL is short.
	rows, _ = Reconstruct(events, ReconstructOptions{ExchangeTTL: 30 * m})
	if w1 := candOf(t, rowsByTrace(rows)["target"], "w1"); w1.LastExchangeAt != nil {
		t.Errorf("exchange outside the TTL must be dropped, got %v", *w1.LastExchangeAt)
	}
}

func TestReconstructNameIsAsOfT(t *testing.T) {
	events := []state.EventRow{
		evTurn(0, "w", "old-name-codex", "assistant"),
		evTurn(time.Hour, "w", "new-name-codex", "assistant"),
		evTurn(0, "orch", "orchestrator-codex", "assistant"),
		evAsk(30*time.Minute, "early", "orchestrator-codex", "orch", "old-name-codex", "w", nil),
		evAsk(90*time.Minute, "late", "orchestrator-codex", "orch", "new-name-codex", "w", nil),
	}
	rows, _ := Reconstruct(events, ReconstructOptions{})
	got := rowsByTrace(rows)
	if n := candOf(t, got["early"], "w").DisplayName; n != "old-name-codex" {
		t.Errorf("early name = %q", n)
	}
	if n := candOf(t, got["late"], "w").DisplayName; n != "new-name-codex" {
		t.Errorf("late name = %q", n)
	}
}

// A reconstructed dataset replays through the baselines, rows whose target was
// not online are skipped under their own reason, and strata survive the file.
func TestReconstructedRowsReplay(t *testing.T) {
	h := time.Hour
	events := []state.EventRow{
		evTurn(0, "a", "alpha-codex", "assistant"),
		evTurn(0, "b", "beta-codex", "assistant"),
		evTurn(0, "orch", "orchestrator-codex", "assistant"),
		evAsk(time.Minute, "q1", "orchestrator-codex", "orch", "alpha-codex", "a", nil),
		evAsk(2*time.Minute, "q2", "orchestrator-codex", "orch", "alpha-codex", "a", nil),
		evAsk(3*time.Minute, "q3", "orchestrator-codex", "orch", "ghost-codex", "ghost", nil),
		evAsk(40*h, "q4", "alpha-codex", "a", "orchestrator-codex", "orch", nil),
	}
	rows, _ := Reconstruct(events, ReconstructOptions{})
	var buf bytes.Buffer
	if err := WriteRows(&buf, rows); err != nil {
		t.Fatal(err)
	}
	examples, err := ReadExamples(&buf)
	if err != nil || len(examples) != len(rows) {
		t.Fatalf("read back %d examples, err %v", len(examples), err)
	}
	if examples[2].Strata == nil || examples[2].Strata.TargetOnline {
		t.Fatalf("q3 strata lost: %+v", examples[2].Strata)
	}
	arm := NewMajority(examples, true)
	rep, results := Replay(context.Background(), examples, arm.Name(), func(ctx context.Context, in Input) RouterResult { return Run(ctx, arm, in, time.Second) }, Options{})
	if rep.Skipped[SkipTargetNotOnline] != 2 { // q3 (never seen) and q4 (orchestrator stale at 40h)
		t.Fatalf("skipped = %v", rep.Skipped)
	}
	// q2 is the only scored row with past history (q1 voted alpha).
	if rep.Scored != 2 || rep.Hit1 != 1 {
		t.Fatalf("scored %d hit1 %d: %+v", rep.Scored, rep.Hit1, results)
	}
	if results[1].Strata == nil || results[1].Strata.HubBound {
		t.Fatalf("result strata = %+v", results[1].Strata)
	}
}
