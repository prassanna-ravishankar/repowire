package routing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func at(ts string) *string { return &ts }

func TestRecentRanksNewestExchangeAndTiesByKey(t *testing.T) {
	in := testInput()
	in.Candidates = []Candidate{
		{PeerID: "repow-b", LastExchangeAt: at("2026-10-11T09:00:00Z"), Access: AccessOK},
		{PeerID: "repow-c", LastExchangeAt: at("2026-10-11T10:00:00Z"), Access: AccessOK},
		{PeerID: "repow-d", LastExchangeAt: at("2026-10-11T10:00:00Z"), Access: AccessOK},
		{PeerID: "repow-e", Access: AccessOK},
	}
	res := Run(context.Background(), Recent{}, in, time.Second)
	got := []string{}
	for _, s := range res.Scores {
		got = append(got, s.PeerID)
	}
	// c and d tie on the newest exchange, so key order decides; e has no
	// exchange and is left unranked.
	if res.Fallback || !slices.Equal(got, []string{"repow-c", "repow-d", "repow-b"}) {
		t.Fatalf("recent = %v (%+v)", got, res)
	}

	in.Candidates = []Candidate{{PeerID: "repow-e", Access: AccessOK}}
	if res := Run(context.Background(), Recent{}, in, time.Second); !res.Fallback || res.FallbackReason != FallbackNoHistory {
		t.Fatalf("no exchanges = %+v, want no_history fallback", res)
	}
}

func TestDeclaredIsRecentContact(t *testing.T) {
	in := testInput() // repow-b has the only recorded exchange
	if !DeclaredIsRecentContact(in, "repow-b") || DeclaredIsRecentContact(in, "repow-c") {
		t.Fatal("repow-b is the recent contact, repow-c is not")
	}
	in.Candidates[1].LastExchangeAt = at("2026-10-11T10:01:00Z")
	if DeclaredIsRecentContact(in, "repow-b") || !DeclaredIsRecentContact(in, "repow-c") {
		t.Fatal("a newer exchange with repow-c makes it the recent contact")
	}
	in.Candidates[0].LastExchangeAt, in.Candidates[1].LastExchangeAt = nil, nil
	if DeclaredIsRecentContact(in, "repow-b") {
		t.Fatal("no exchange data is not a recent contact")
	}
}

func TestMajorityLeavesTheScoredRowOut(t *testing.T) {
	// A lone ask has nothing but itself to vote: leave-one-out must find no
	// history rather than predicting its own label.
	lone := []Example{example("ask-1", "repow-c", ArmExplicit, nil)}
	m := NewMajority(lone, false)
	if res := Run(context.Background(), m, InputFrom("ask-1", lone[0].Decision), time.Second); !res.Fallback || res.FallbackReason != FallbackNoHistory {
		t.Fatalf("lone ask = %+v, want no_history", res)
	}

	// Two asks to b, one to c. Scoring an ask to b sees b=1, c=1 (tie → key
	// order picks b); scoring the ask to c sees b=2, c=0, so it misses.
	examples := []Example{
		example("ask-1", "repow-b", ArmExplicit, nil),
		example("ask-2", "repow-b", ArmExplicit, nil),
		example("ask-3", "repow-c", ArmExplicit, nil),
	}
	m = NewMajority(examples, false)
	rep, rows := Replay(context.Background(), examples, "majority", func(ctx context.Context, in Input) RouterResult {
		return Run(ctx, m, in, time.Second)
	}, Options{})
	if rep.Scored != 3 || rep.Hit1 != 2 || rows[2].Hit1 || rows[2].Hit3 {
		t.Fatalf("majority report = %+v rows = %+v", rep, rows)
	}
	if rows[2].Top[0] != "repow-b" || len(rows[2].Top) != 1 {
		t.Fatalf("ask to c should rank only b, got %v", rows[2].Top)
	}
}

func TestMajorityPastOnlyCountsEarlierAsks(t *testing.T) {
	stamp := func(ex Example, ts string) Example { ex.Decision.DecidedAt = ts; return ex }
	examples := []Example{
		stamp(example("ask-1", "repow-b", ArmExplicit, nil), "2026-10-11T10:00:00Z"),
		stamp(example("ask-2", "repow-b", ArmExplicit, nil), "2026-10-11T11:00:00Z"),
		stamp(example("ask-3", "repow-c", ArmExplicit, nil), "2026-10-11T12:00:00Z"),
		stamp(example("ask-4", "repow-c", ArmExplicit, nil), "2026-10-11T12:00:00Z"), // same instant as ask-3
	}
	past, whole := NewMajority(examples, true), NewMajority(examples, false)
	top := func(m Majority, i int) string {
		res := Run(context.Background(), m, InputFrom(examples[i].TraceID, examples[i].Decision), time.Second)
		if res.Fallback {
			return res.FallbackReason
		}
		return res.Scores[0].PeerID
	}
	// The first ask has no past; in the whole window the later asks vote
	// (b once, c twice), so the future decides it.
	if got := top(past, 0); got != FallbackNoHistory {
		t.Fatalf("majority-past on the first ask = %s, want no_history", got)
	}
	if got := top(whole, 0); got != "repow-c" {
		t.Fatalf("whole-window majority on the first ask = %s", got)
	}
	// ask-4 shares ask-3's instant: strictly-before excludes it, so only the
	// two asks to b vote. The whole window sees c (ask-3) as well.
	if res := Run(context.Background(), past, InputFrom("ask-4", examples[3].Decision), time.Second); res.Fallback ||
		!slices.Equal(res.Scores, []Score{{PeerID: "repow-b", Score: 2}}) {
		t.Fatalf("majority-past on ask-4 = %+v, want exactly b=2 and no same-instant c", res)
	}
	if res := Run(context.Background(), whole, InputFrom("ask-4", examples[3].Decision), time.Second); len(res.Scores) != 2 {
		t.Fatalf("whole-window majority on ask-4 = %+v", res)
	}
	if past.Name() != "majority-past" || whole.Name() != "majority" {
		t.Fatal("variant names")
	}
}

func TestMajorityPastIgnoresTiedTimeHistory(t *testing.T) {
	stamp := func(ex Example) Example { ex.Decision.DecidedAt = "2026-10-11T12:00:00Z"; return ex }
	examples := []Example{
		stamp(example("ask-1", "repow-b", ArmExplicit, nil)),
		stamp(example("ask-2", "repow-c", ArmExplicit, nil)),
	}
	past := NewMajority(examples, true)
	for _, ex := range examples {
		if res := Run(context.Background(), past, InputFrom(ex.TraceID, ex.Decision), time.Second); res.FallbackReason != FallbackNoHistory {
			t.Fatalf("%s with only same-instant history = %+v, want no_history", ex.TraceID, res)
		}
	}
}

// Scoring a narrow window must not starve the vote: history passed to
// NewMajority can be wider than the rows scored.
func TestMajorityPastVotesFromHistoryBeyondTheScoredWindow(t *testing.T) {
	stamp := func(ex Example, ts string) Example { ex.Decision.DecidedAt = ts; return ex }
	history := []Example{
		stamp(example("ask-1", "repow-b", ArmExplicit, nil), "2026-10-11T10:00:00Z"),
		stamp(example("ask-2", "repow-b", ArmExplicit, nil), "2026-10-11T11:00:00Z"),
	}
	window := history[1:]
	run := func(m Majority) Report {
		rep, _ := Replay(context.Background(), window, "majority-past", func(ctx context.Context, in Input) RouterResult {
			return Run(ctx, m, in, time.Second)
		}, Options{})
		return rep
	}
	if rep := run(NewMajority(window, true)); rep.Hit1 != 0 || rep.Fallbacks != 1 {
		t.Fatalf("window-only votes should be cold: %+v", rep)
	}
	full := NewMajority(history, true)
	if rep := run(full); rep.Hit1 != 1 {
		t.Fatalf("full-history votes should hit: %+v", rep)
	}
	if h := full.History(); h.Asks != 2 || h.From != "2026-10-11T10:00:00Z" || h.To != "2026-10-11T11:00:00Z" {
		t.Fatalf("history = %+v", h)
	}
}

func TestMajorityHistoryOrdersByTimeNotText(t *testing.T) {
	stamp := func(ex Example, ts string) Example { ex.Decision.DecidedAt = ts; return ex }
	m := NewMajority([]Example{
		stamp(example("ask-1", "repow-b", ArmExplicit, nil), "2026-10-11T12:00:00.1Z"),
		stamp(example("ask-2", "repow-b", ArmExplicit, nil), "2026-10-11T12:00:00Z"),
		stamp(example("ask-3", "repow-b", ArmExplicit, nil), "2026-10-11T12:30:00+01:00"), // 11:30Z, the earliest
	}, true)
	if h := m.History(); h.From != "2026-10-11T11:30:00Z" || h.To != "2026-10-11T12:00:00.1Z" || h.Asks != 3 {
		t.Fatalf("history = %+v", h)
	}
}

func TestErrorCodeAllowlist(t *testing.T) {
	for code, want := range map[string]string{
		"billing_error": "billing_error", "enforced_spend_limit_reached": "enforced_spend_limit_reached",
		"overloaded_error": "overloaded_error", "KEY_SENTINEL_44ab": "other", "": "other", "Billing_Error": "other",
	} {
		if got := errorCode(code); got != want {
			t.Errorf("errorCode(%q) = %q, want %q", code, got, want)
		}
	}
	// End to end: Jev's declared error_type round-trips; an unknown one is "other".
	for body, want := range map[string]string{
		`{"detail":{"error_type":"billing_error","message":"no credits"}}`: "billing_error",
		`{"detail":{"error_type":"KEY_SENTINEL_44ab"}}`:                    "other",
		`not json`: "other",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(body))
		}))
		res := Run(context.Background(), Jev{View: ViewAttested, APIKey: "k", BaseURL: srv.URL}, testInput(), time.Second)
		srv.Close()
		if res.ErrorCode != want || res.HTTPStatus != 402 || res.FallbackReason != FallbackBilling || res.Error != "" {
			t.Fatalf("body %s: %+v, want code %s and no error text", body, res, want)
		}
	}
}

// Legacy rows (pre-#420) hold raw provider text in "error". Export recovers
// the status and allowlisted codes from both recorded forms and never the text.
func TestReplayExportsLegacyRowsAsStructureOnly(t *testing.T) {
	cases := []struct {
		arm, text, provider, code string
		status                    int
	}{
		{"jev-attested", `error: jev status 402: {"detail":{"error_type":"billing_error","message":"Your organization has no credits. LEGACYKEY987"}}`, "jev", "billing_error", 402},
		{"frontier-claimed", `error: frontier request: POST "https://api.anthropic.com/v1/messages": 400 Bad Request (Request-ID: req_1) {"type":"error","error":{"type":"invalid_request_error","message":"Who owns the lexer? LEGACYKEY987"}}`, "anthropic", "invalid_request_error", 400},
		{"jev-attested", `error: jev status 422: {"echo":"Bearer LEGACYKEY987"}`, "jev", "other", 422},
	}
	for _, tc := range cases {
		ex := planned(example("ask-1", "repow-b", ArmExplicit, nil), tc.arm)
		ex.Shadow = map[string]RouterResult{tc.arm: {Arm: tc.arm, Scores: []Score{}, Fallback: true, FallbackReason: FallbackError, Error: tc.text}}
		rep, rows := Replay(context.Background(), []Example{ex}, tc.arm, nil, Options{Recorded: true})
		out, _ := json.Marshal(struct {
			R []Result
			P Report
		}{rows, rep})
		for _, leak := range []string{"LEGACYKEY987", "lexer", "Bearer", "organization"} {
			if strings.Contains(string(out), leak) {
				t.Fatalf("%s export leaked %q: %s", tc.arm, leak, out)
			}
		}
		if r := rows[0]; r.FallbackStatus != tc.status || r.FallbackProvider != tc.provider || r.FallbackCode != tc.code {
			t.Fatalf("%s legacy attribution = %+v", tc.arm, r)
		}
	}
}

func TestReplayExportsStructuredFallback(t *testing.T) {
	res := RouterResult{Arm: "stub", Scores: []Score{}, Fallback: true, FallbackReason: FallbackRequestError, HTTPStatus: 422, Provider: "jev", ErrorCode: "other"}
	examples := []Example{example("ask-1", "repow-b", ArmExplicit, nil), example("ask-2", "repow-b", ArmExplicit, nil)}
	rep, rows := Replay(context.Background(), examples, "stub", func(context.Context, Input) RouterResult { return res }, Options{})
	if r := rows[0]; r.FallbackStatus != 422 || r.FallbackProvider != "jev" || r.FallbackCode != "other" || rep.FallbackCodes[FallbackRequestError]["422 other"] != 2 {
		t.Fatalf("rows %+v codes %v", rows[0], rep.FallbackCodes)
	}
}

func TestMajorityCountsOnlyTheSendersOwnLabelledAsks(t *testing.T) {
	other := example("ask-9", "repow-c", ArmExplicit, nil)
	other.Decision.Sender = &Candidate{PeerID: "repow-z"}
	routed := example("ask-8", "repow-c", "jev-attested", nil)
	examples := []Example{example("ask-1", "repow-b", ArmExplicit, nil), other, routed}
	m := NewMajority(examples, false)
	res := Run(context.Background(), m, InputFrom("ask-x", examples[0].Decision), time.Second)
	if res.Fallback || len(res.Scores) != 1 || res.Scores[0].PeerID != "repow-b" {
		t.Fatalf("majority = %+v, want only repow-b (other senders and routed asks do not vote)", res)
	}
}

func TestParseMaskRejectsUnknownAndIdentityFields(t *testing.T) {
	if got, err := ParseMask(" last_exchange_with_sender_at, open_asks_inbound ,"); err != nil || !slices.Equal(got, []string{"last_exchange_with_sender_at", "open_asks_inbound"}) {
		t.Fatalf("ParseMask = %v, %v", got, err)
	}
	for _, bad := range []string{"peer_id", "access", "turnstate"} {
		if _, err := ParseMask(bad); err == nil {
			t.Fatalf("ParseMask(%q) accepted", bad)
		}
	}
}

func TestMaskBlanksCandidatesAndSenderWithoutTouchingTheRecord(t *testing.T) {
	in := testInput()
	masked := Mask(in, []string{"last_exchange_with_sender_at", "description", "last_seen_age_s"})
	c := masked.Candidates[0]
	if c.LastExchangeAt != nil || c.Description != "" || c.LastSeenAgeS != nil || c.Project != "lexer" {
		t.Fatalf("masked candidate = %+v", c)
	}
	if in.Candidates[0].LastExchangeAt == nil || in.Candidates[0].Description == "" {
		t.Fatal("Mask mutated the recorded input")
	}
	if masked.Sender.Description != "" || in.Sender.Description != "parser" {
		t.Fatal("the sender snapshot must be masked like the candidates, without touching the record")
	}
}

func TestReplayRecordsMaskAndStratifiesByRecency(t *testing.T) {
	examples := []Example{
		example("ask-1", "repow-b", ArmExplicit, nil), // b is the recent contact
		example("ask-2", "repow-c", ArmExplicit, nil), // against recency
	}
	var seen []Input
	rep, rows := Replay(context.Background(), examples, "stub", func(_ context.Context, in Input) RouterResult {
		seen = append(seen, in)
		return ranked("repow-b", "repow-c")
	}, Options{Mask: []string{"last_exchange_with_sender_at"}})
	for _, in := range seen {
		if in.Candidates[0].LastExchangeAt != nil {
			t.Fatal("the arm saw an unmasked field")
		}
	}
	if !slices.Equal(rep.Mask, []string{"last_exchange_with_sender_at"}) || !slices.Equal(rows[0].Mask, rep.Mask) {
		t.Fatalf("mask not recorded: report %v row %v", rep.Mask, rows[0].Mask)
	}
	// Stratification reads the unmasked snapshot.
	if !rows[0].DeclaredIsRecentContact || rows[1].DeclaredIsRecentContact {
		t.Fatalf("rows = %+v", rows)
	}
	if r := rep.Recency[StratumRecentContact]; r == nil || r.Scored != 1 || r.Hit1 != 1 {
		t.Fatalf("recent stratum = %+v", r)
	}
	if o := rep.Recency[StratumOtherContact]; o == nil || o.Scored != 1 || o.Hit1 != 0 || o.Hit3 != 1 {
		t.Fatalf("other stratum = %+v", o)
	}

	// With no exchange data anywhere, recency is unknown, not "other".
	blind := example("ask-3", "repow-c", ArmExplicit, nil)
	blind.Decision.Candidates[0].LastExchangeAt = nil
	rep, rows = Replay(context.Background(), []Example{blind}, "stub", func(context.Context, Input) RouterResult { return ranked("repow-c") }, Options{})
	if rows[0].Recency != StratumNoExchange || rep.Recency[StratumNoExchange] == nil || rep.Recency[StratumOtherContact] != nil {
		t.Fatalf("no-data row = %+v, strata %v", rows[0], rep.Recency)
	}

	recorded := planned(examples[0], "paid")
	recorded.Shadow = map[string]RouterResult{"paid": ranked("repow-b")}
	rep, _ = Replay(context.Background(), []Example{recorded}, "paid", nil, Options{Recorded: true, Mask: []string{"description"}})
	if len(rep.Mask) != 0 {
		t.Fatalf("a recorded replay ran unmasked and must not claim a mask: %v", rep.Mask)
	}
}

// candidateState decodes the candidates an arm's request carried.
func candidateState(t *testing.T, state any) map[string]map[string]any {
	t.Helper()
	raw, _ := json.Marshal(state)
	var s struct {
		Candidates map[string]map[string]any `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s.Candidates
}

func maskedRerun(t *testing.T, arm Arm, mask []string) {
	t.Helper()
	ex := example("ask-1", "repow-b", ArmExplicit, nil)
	ex.Decision.Candidates[0].OpenAsksInbound = 2
	Replay(context.Background(), []Example{ex}, arm.Name(), func(ctx context.Context, in Input) RouterResult {
		return Run(ctx, arm, in, 5*time.Second)
	}, Options{Mask: mask})
}

func TestMaskedRerunReachesModelTemplates(t *testing.T) {
	mask := []string{"last_exchange_with_sender_at", "open_asks_inbound"}
	check := func(t *testing.T, candidates map[string]map[string]any, masked bool) {
		c1 := candidates["c1"]
		if masked && (c1["last_exchange_with_sender_age_s"] != nil || c1["open_asks_inbound"] != float64(0)) {
			t.Fatalf("masked template state = %v", c1)
		}
		if !masked && (c1["last_exchange_with_sender_age_s"] != float64(300) || c1["open_asks_inbound"] != float64(2)) {
			t.Fatalf("unmasked template state = %v", c1)
		}
	}

	t.Run("jev", func(t *testing.T) {
		var state any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			state = body["state"]
			_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"c1":{"type":"noul","noul":0.2},"c2":{"type":"noul","noul":0.7}}}`))
		}))
		defer srv.Close()
		arm := Jev{View: ViewAttested, APIKey: "k", Model: "jev-1.13.0", BaseURL: srv.URL}
		maskedRerun(t, arm, nil)
		check(t, candidateState(t, state), false)
		maskedRerun(t, arm, mask)
		check(t, candidateState(t, state), true)
	})

	t.Run("frontier", func(t *testing.T) {
		var state any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Messages []struct {
					Content []struct{ Text string } `json:"content"`
				} `json:"messages"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			_ = json.Unmarshal([]byte(body.Messages[0].Content[0].Text), &state)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
				"content":[{"type":"text","text":"{\"ranking\":[\"c2\",\"c1\"],\"confidence\":0.6}"}],
				"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		}))
		defer srv.Close()
		arm := Frontier{View: ViewAttested, APIKey: "k", Model: "claude-opus-5-5", BaseURL: srv.URL}
		maskedRerun(t, arm, nil)
		check(t, candidateState(t, state), false)
		maskedRerun(t, arm, mask)
		check(t, candidateState(t, state), true)
	})
}

func TestNewestRanksSmallestLastSeenAgeFirst(t *testing.T) {
	age := func(v float64) *float64 { return &v }
	in := Input{Candidates: []Candidate{
		{PeerID: "old", LastSeenAgeS: age(3600)},
		{PeerID: "new", LastSeenAgeS: age(2)},
		{PeerID: "unknown"},
	}}
	res := Run(context.Background(), Newest{}, in, time.Second)
	if res.Fallback || len(res.Scores) != 2 || res.Scores[0].PeerID != "new" || res.Scores[1].PeerID != "old" {
		t.Fatalf("newest = %+v", res)
	}
	if res := Run(context.Background(), Newest{}, Input{Candidates: []Candidate{{PeerID: "x"}}}, time.Second); !res.Fallback || res.FallbackReason != FallbackNoHistory {
		t.Fatalf("no last_seen must fall back with no_history: %+v", res)
	}
}

// A mask is global to the request body: nothing a masked field carries may
// reach a model through the sender block either.
func TestMaskBlanksSenderAndCandidates(t *testing.T) {
	age, stamp := 42.0, "2026-06-01T12:00:00Z"
	loud := Candidate{PeerID: "p", LastSeen: &stamp, LastSeenAgeS: &age, TurnState: "working", OpenAsksInbound: 3, LastExchangeAt: &stamp, Project: "keep"}
	sender := loud
	in := Input{DecidedAt: time.Date(2026, 6, 1, 12, 5, 0, 0, time.UTC), Sender: &sender, Candidates: []Candidate{loud}}
	strict := []string{"last_exchange_with_sender_at", "open_asks_inbound", "last_seen_age_s", "turn_state"}
	state, _ := Project(Mask(in, strict), ViewAttested)
	blanked := func(who string, f map[string]any) {
		t.Helper()
		if f["last_seen_age_s"] != nil || f["turn_state"] != "" || f["open_asks_inbound"] != 0 || f["last_exchange_with_sender_age_s"] != nil {
			t.Errorf("%s still carries masked state: %+v", who, f)
		}
		if f["project"] != "keep" {
			t.Errorf("%s lost an unmasked field: %+v", who, f)
		}
	}
	blanked("sender", state["sender"].(map[string]any))
	blanked("candidate", state["candidates"].(map[string]any)["c1"].(map[string]any))
	if in.Sender.OpenAsksInbound != 3 || sender.LastSeenAgeS == nil {
		t.Fatal("Mask must not mutate its input")
	}
}
