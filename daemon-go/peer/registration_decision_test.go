package peer

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// The registration_decision records (#493) let an offline reader replay the
// identity rules: each says what evidence the registrant carried, what every
// step concluded about which candidate, and which step chose the id, without
// the mutable mapping and binding tables.

func decisionsIn(s *memStore) []map[string]any {
	var out []map[string]any
	for _, e := range eventsOfType(s, "registration_decision") {
		out = append(out, e.Payload)
	}
	return out
}

func lastDecision(t *testing.T, s *memStore) map[string]any {
	t.Helper()
	ds := decisionsIn(s)
	if len(ds) == 0 {
		t.Fatal("no registration_decision recorded")
	}
	return ds[len(ds)-1]
}

// noteOf is the decision's first note for step with outcome, or nil.
func noteOf(d map[string]any, step, outcome string) map[string]any {
	for _, n := range d["steps"].([]map[string]any) {
		if n["step"] == step && n["outcome"] == outcome {
			return n
		}
	}
	return nil
}

func wantNote(t *testing.T, d map[string]any, step, outcome string, id proto.PeerID, reason string) map[string]any {
	t.Helper()
	n := noteOf(d, step, outcome)
	if n == nil {
		t.Fatalf("no %s/%s note in %v", step, outcome, d["steps"])
	}
	if id != "" && n["peer_id"] != string(id) {
		t.Fatalf("%s/%s names %v, want %s", step, outcome, n["peer_id"], id)
	}
	if reason != "" && n["reason"] != reason {
		t.Fatalf("%s/%s reason %q, want %q", step, outcome, n["reason"], reason)
	}
	return n
}

func wantSelected(t *testing.T, d map[string]any, id proto.PeerID, step string) {
	t.Helper()
	sel, _ := d["selected"].(map[string]any)
	if d["outcome"] == "refused" || sel["peer_id"] != string(id) || sel["step"] != step {
		t.Fatalf("selected %v (outcome %v), want %s by step %s", sel, d["outcome"], id, step)
	}
}

func TestRegistrationDecisionRecordsEvidenceAndTheSelectingStep(t *testing.T) {
	ctx := context.Background()
	h := newSessionHarness(t)
	a, _ := h.register(proto.AgentClaudeCode, "/p/app", "sess-1", 100)

	d := lastDecision(t, h.store)
	wantSelected(t, d, a, stepMint)
	if d["outcome"] != "registered" || d["rules"] != RegistrationRules || d["seq"] != int64(1) {
		t.Fatalf("header: outcome %v rules %v seq %v", d["outcome"], d["rules"], d["seq"])
	}
	in := d["inputs"].(map[string]any)
	if in["base_name"] != "app-claude-code" || in["path"] != "/p/app" || len(in["path_key"].(string)) != 16 || in["preferred_display_name"] != nil {
		t.Fatalf("inputs %v", in)
	}
	// Absent evidence is null; a claim the daemon did not verify is the claim
	// with runtime_identity_verified false.
	ev := d["evidence"].(map[string]any)
	want := map[string]any{"agent_pid": 100, "parent_pid": nil, "runtime_session_id": "sess-1", "claimed_peer_id": nil,
		"runtime_identity_verified": false, "pane_id": nil}
	if !reflect.DeepEqual(ev, want) {
		t.Fatalf("evidence %v, want %v", ev, want)
	}
	wantNote(t, d, stepRuntimePID, "skipped", "", "no_peer_holds_the_pid")
	wantNote(t, d, stepClaim, "skipped", "", "no_claim")
	wantNote(t, d, stepSessionBinding, "skipped", "", "session_not_bound")
	wantNote(t, d, stepNameTuple, "skipped", "", "no_candidate")

	// The record is for the journal, not the live window and its stream.
	for _, e := range h.r.GetEvents() {
		if e["type"] == "registration_decision" {
			t.Fatalf("registration_decision reached the live window: %v", e)
		}
	}

	// peer_online names the decision it came from.
	online := eventsOfType(h.store, "peer_online")
	last := online[len(online)-1].Payload
	if last["run_id"] != d["run_id"] || last["decision_seq"] != d["seq"] {
		t.Fatalf("peer_online %v does not name decision %v/%v", last, d["run_id"], d["seq"])
	}

	// The same process again is a reconnect chosen by its live pid.
	h.register(proto.AgentClaudeCode, "/p/app", "sess-1", 100)
	d = lastDecision(t, h.store)
	wantSelected(t, d, a, stepRuntimePID)
	wantNote(t, d, stepRuntimePID, "selected", a, "live_runtime_pid_holder")
	if d["outcome"] != "reconnect" || d["seq"] != int64(2) {
		t.Fatalf("outcome %v seq %v", d["outcome"], d["seq"])
	}

	// An unverified claim to an id nobody holds is recorded as such.
	ghost := proto.PeerID("repow-work-ghost")
	if _, _, err := h.r.AllocateAndRegister(ctx, AllocateParams{Circle: "work", Backend: proto.AgentCodex, Path: ptr("/p/other"),
		Machine: "m", Role: proto.RoleAgent, ClaimedPeerID: &ghost}); err != nil {
		t.Fatal(err)
	}
	d = lastDecision(t, h.store)
	if ev := d["evidence"].(map[string]any); ev["claimed_peer_id"] != ghost || ev["runtime_identity_verified"] != false || ev["agent_pid"] != nil {
		t.Fatalf("claim evidence %v", ev)
	}
	wantNote(t, d, stepClaim, "skipped", ghost, "unknown_peer_id")
	wantNote(t, d, stepRuntimePID, "skipped", "", "no_agent_pid")
}

// Name-tuple adoption: an offline peer, then, after a daemon restart, its
// durable mapping. The restart starts a new run and a new sequence.
func TestRegistrationDecisionTupleAdoptionAcrossARestart(t *testing.T) {
	h := newSessionHarness(t)
	a, _ := h.register(proto.AgentClaudeCode, "/p/solo", "", 100)
	h.offline(a, 100)
	if got, _ := h.register(proto.AgentClaudeCode, "/p/solo", "", 300); got != a {
		t.Fatalf("tuple reclaim got %s, want %s", got, a)
	}
	before := lastDecision(t, h.store)
	wantSelected(t, before, a, stepNameTuple)
	wantNote(t, before, stepNameTuple, "selected", a, "offline_peer")

	h.offline(a, 300)
	h.restart()
	if got, _ := h.register(proto.AgentClaudeCode, "/p/solo", "", 400); got != a {
		t.Fatalf("after restart got %s, want %s", got, a)
	}
	after := lastDecision(t, h.store)
	wantSelected(t, after, a, stepNameTuple)
	wantNote(t, after, stepNameTuple, "selected", a, "durable_mapping")
	if after["run_id"] == before["run_id"] || after["seq"] != int64(1) {
		t.Fatalf("restart: run %v -> %v, seq %v", before["run_id"], after["run_id"], after["seq"])
	}
}

// Several candidates on one tuple: the decision lists them, with their
// retirement, and the mint that followed.
func TestRegistrationDecisionAmbiguousCandidates(t *testing.T) {
	h := newSessionHarness(t)
	a, aName := h.register(proto.AgentClaudeCode, "/p/app", "", 100)
	b, _ := h.register(proto.AgentClaudeCode, "/p/app", "", 101)
	h.offline(a, 100)
	h.offline(b, 101)
	h.r.mu.Lock()
	h.r.peers[b].peer.DisplayName = aName
	h.r.mappings[b].DisplayName = aName
	h.r.mu.Unlock()

	got, _ := h.register(proto.AgentClaudeCode, "/p/app", "", 102)
	d := lastDecision(t, h.store)
	wantSelected(t, d, got, stepMint)
	n := wantNote(t, d, stepNameTuple, "ambiguous", "", "several_offline_peers")
	ids := []string{string(a), string(b)}
	slices.Sort(ids)
	want := []map[string]any{{"peer_id": ids[0], "retirement": "none"}, {"peer_id": ids[1], "retirement": "none"}}
	if !reflect.DeepEqual(n["candidates"], want) {
		t.Fatalf("candidates %v, want %v", n["candidates"], want)
	}
}

// A live sibling (a fork) claiming the id is rejected, with the liveness
// answer that decided it.
func TestRegistrationDecisionLiveSiblingFork(t *testing.T) {
	ctx := context.Background()
	h := newSessionHarness(t)
	a, _ := h.register(proto.AgentClaudeCode, "/p/fork", "sess-1", 100)
	pid := 200
	h.live.alive[pid] = true
	got, _, err := h.r.AllocateAndRegister(ctx, AllocateParams{Circle: "work", Backend: proto.AgentClaudeCode, Path: ptr("/p/fork"),
		Machine: "m", Role: proto.RoleAgent, ClaimedPeerID: &a, AgentPID: &pid})
	if err != nil || got == a {
		t.Fatalf("fork got %s (%v)", got, err)
	}
	d := lastDecision(t, h.store)
	wantNote(t, d, stepClaim, "rejected", a, "fork_live_in_another_process")
	wantSelected(t, d, got, stepMint)
	if !slices.ContainsFunc(d["pid_checks"].([]map[string]any), func(c map[string]any) bool { return c["pid"] == 100 && c["alive"] == true }) {
		t.Fatalf("pid checks %v lack the live holder", d["pid_checks"])
	}
}

// Soft and hard retirement: a refusal is recorded too, under the claimed id,
// and a lifted soft retirement names its proof.
func TestRegistrationDecisionRetirement(t *testing.T) {
	ctx := context.Background()
	h := newSessionHarness(t)
	a, _ := h.register(proto.AgentCodex, "/p/ret", "thread-1", 0)
	if _, err := h.r.MarkOffline(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	claim := func(verified bool) error {
		_, _, err := h.r.AllocateAndRegister(ctx, AllocateParams{Circle: "work", Backend: proto.AgentCodex, Path: ptr("/p/ret"),
			Machine: "m", Role: proto.RoleAgent, ClaimedPeerID: &a, RuntimeIdentityVerified: verified,
			Metadata: map[string]any{"runtime_session_id": "thread-1"}})
		return err
	}

	if err := claim(false); !errors.Is(err, ErrPeerRetired) {
		t.Fatalf("unproven claim: %v", err)
	}
	refusal := eventsOfType(h.store, "registration_decision")
	ev := refusal[len(refusal)-1]
	d := ev.Payload
	if d["outcome"] != "refused" || d["selected"] != nil || ev.PeerID != a || !strings.Contains(d["error"].(string), "retired") {
		t.Fatalf("refusal recorded as %v (peer %s)", d, ev.PeerID)
	}
	if n := wantNote(t, d, stepRetirementGuard, "refused", a, "soft_retired_without_proof"); n["retirement"] != "soft" {
		t.Fatalf("retirement %v", n["retirement"])
	}

	if err := claim(true); err != nil {
		t.Fatal(err)
	}
	d = lastDecision(t, h.store)
	if n := wantNote(t, d, stepRetirementGuard, "unretired", a, "verified_runtime_identity"); n["retirement"] != "soft" {
		t.Fatalf("retirement before the lift %v", n["retirement"])
	}
	if sel := d["selected"].(map[string]any); sel["peer_id"] != string(a) {
		t.Fatalf("selected %v", sel)
	}

	if _, err := h.r.ClosePeer(ctx, a, "test"); err != nil {
		t.Fatal(err)
	}
	if err := claim(true); !errors.Is(err, ErrPeerClosed) {
		t.Fatalf("claim on a closed peer: %v", err)
	}
	if n := wantNote(t, lastDecision(t, h.store), stepRetirementGuard, "refused", a, "hard_retired"); n["retirement"] != "hard" {
		t.Fatalf("retirement %v", n["retirement"])
	}
}

// The #474 sequence: a pid-only registration mints a placeholder, then the
// session's registration takes the process back for the bound peer.
func TestRegistrationDecisionPidOnlyThenSessionClaim(t *testing.T) {
	h := newSessionHarness(t)
	orig, _ := h.register(proto.AgentClaudeCode, "/p/orch", "sess-1", 100)
	if _, err := h.r.MarkOffline(context.Background(), orig, true); err != nil {
		t.Fatal(err)
	}
	delete(h.live.alive, 100)

	placeholder, _ := h.register(proto.AgentClaudeCode, "/p/orch", "", 200)
	d := lastDecision(t, h.store)
	wantSelected(t, d, placeholder, stepMint)
	if ev := d["evidence"].(map[string]any); ev["runtime_session_id"] != nil || ev["agent_pid"] != 200 {
		t.Fatalf("pid-only evidence %v", ev)
	}

	if got, _ := h.register(proto.AgentClaudeCode, "/p/orch", "sess-1", 200); got != orig {
		t.Fatalf("session claim got %s, want %s", got, orig)
	}
	d = lastDecision(t, h.store)
	wantNote(t, d, stepRuntimePID, "yielded", placeholder, "placeholder_without_session")
	n := wantNote(t, d, stepRuntimePID, "selected", orig, "session_bound_peer_takes_back_its_process")
	if n["retirement"] != "soft" {
		t.Fatalf("bound peer's retirement at the decision %v", n["retirement"])
	}
	wantSelected(t, d, orig, stepRuntimePID)
}
