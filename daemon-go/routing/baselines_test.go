package routing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
	m := NewMajority(lone)
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
	m = NewMajority(examples)
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

func TestMajorityCountsOnlyTheSendersOwnLabelledAsks(t *testing.T) {
	other := example("ask-9", "repow-c", ArmExplicit, nil)
	other.Decision.Sender = &Candidate{PeerID: "repow-z"}
	routed := example("ask-8", "repow-c", "jev-attested", nil)
	examples := []Example{example("ask-1", "repow-b", ArmExplicit, nil), other, routed}
	m := NewMajority(examples)
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

func TestMaskBlanksCandidatesWithoutTouchingTheRecord(t *testing.T) {
	in := testInput()
	masked := Mask(in, []string{"last_exchange_with_sender_at", "description", "last_seen_age_s"})
	c := masked.Candidates[0]
	if c.LastExchangeAt != nil || c.Description != "" || c.LastSeenAgeS != nil || c.Project != "lexer" {
		t.Fatalf("masked candidate = %+v", c)
	}
	if in.Candidates[0].LastExchangeAt == nil || in.Candidates[0].Description == "" {
		t.Fatal("Mask mutated the recorded input")
	}
	if masked.Sender.Description != "parser" {
		t.Fatal("the sender snapshot is not masked")
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
