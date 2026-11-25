package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// grammarTooLarge is the body the API returned for every frontier call with
// 79 or more keyed candidates in the 2026-10-14 run (request id replaced).
const grammarTooLarge = `{"error":{"message":"The compiled grammar is too large, which would cause performance issues. Simplify your tool schemas or reduce the number of strict tools.","type":"invalid_request_error"},"request_id":"req_test","type":"error"}`

// largeInput is testInput with n candidates.
func largeInput(n int) Input {
	in := testInput()
	in.Candidates = nil
	for i := 0; i < n; i++ {
		in.Candidates = append(in.Candidates, Candidate{PeerID: fmt.Sprintf("repow-%03d", i), DisplayName: fmt.Sprintf("peer-%d", i), Project: "p", Access: AccessOK})
	}
	return in
}

// fakeAnthropic answers like the Messages API with structured outputs: a
// keyed scores schema of more than limit properties is refused with the
// stored 400, a schema breaking the documented rules (additionalProperties
// other than false, optional properties) is refused too, and otherwise the
// request is answered by answer, given the schema's scores shape and keys.
type fakeAnthropic struct {
	limit  int
	answer func(listed bool, keys []string) string
	last   map[string]any
}

func (f *fakeAnthropic) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("request body: %v", err)
		}
		f.last = req
		w.Header().Set("Content-Type", "application/json")
		schema := req["output_config"].(map[string]any)["format"].(map[string]any)["schema"].(map[string]any)
		if why := schemaRuleBroken(schema); why != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, why)
			return
		}
		scores := schema["properties"].(map[string]any)["scores"].(map[string]any)
		listed := scores["type"] == "array"
		var keys []string
		if !listed {
			for k := range scores["properties"].(map[string]any) {
				keys = append(keys, k)
			}
			if len(keys) > f.limit {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(grammarTooLarge))
				return
			}
		}
		text, _ := json.Marshal(f.answer(listed, keys))
		_, _ = fmt.Fprintf(w, `{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[{"type":"text","text":%s}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":10}}`, text)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// schemaRuleBroken checks the documented structured-output rules every
// object must follow: additionalProperties false, and every property
// required (the API allows at most 24 optional ones in all).
func schemaRuleBroken(node map[string]any) string {
	if node["type"] == "object" {
		if node["additionalProperties"] != false {
			return "additionalProperties must be false"
		}
		props, _ := node["properties"].(map[string]any)
		required := map[string]bool{}
		if list, ok := node["required"].([]any); ok {
			for _, k := range list {
				required[k.(string)] = true
			}
		}
		for k, sub := range props {
			if !required[k] {
				return "optional property " + k
			}
			if why := schemaRuleBroken(sub.(map[string]any)); why != "" {
				return why
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		return schemaRuleBroken(items)
	}
	return ""
}

// listedAnswer scores keys c1..cn, in order, with descending scores.
func listedAnswer(n int, mutate func([]map[string]any) []map[string]any) string {
	items := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		items = append(items, map[string]any{"key": fmt.Sprintf("c%d", i), "score": 1 - float64(i)/float64(n+1)})
	}
	if mutate != nil {
		items = mutate(items)
	}
	b, _ := json.Marshal(map[string]any{"scores": items, "confidence": 0.7})
	return string(b)
}

func keyedAnswer(keys []string) string {
	scores := map[string]float64{}
	for i, k := range keys {
		scores[k] = 1 / float64(i+2)
	}
	b, _ := json.Marshal(map[string]any{"scores": scores, "confidence": 0.7})
	return string(b)
}

func runFrontier(t *testing.T, srv *httptest.Server, keyedMax int, in Input) RouterResult {
	t.Helper()
	return Run(context.Background(), Frontier{View: ViewClaimed, APIKey: "k", Model: "claude-sonnet-5-5", BaseURL: srv.URL, KeyedMax: keyedMax}, in, 5*time.Second)
}

// The stored refusal: a keyed schema over the limit is a request_error the
// breaker ignores, which is what every 79+ row was before #484.
func TestKeyedScoresOverTheGrammarLimitAreRefused(t *testing.T) {
	fake := &fakeAnthropic{limit: 69, answer: func(bool, []string) string { return "" }}
	res := runFrontier(t, fake.server(t), 200, largeInput(110)) // the old, always-keyed shape
	if !res.Fallback || res.FallbackReason != FallbackRequestError || res.HTTPStatus != 400 || res.ErrorCode != "invalid_request_error" {
		t.Fatalf("result = %+v", res)
	}
	if providerFailure(res.FallbackReason) {
		t.Fatal("the grammar refusal counted against the provider")
	}
	if res.Signals["scores_listed"] != 0 {
		t.Fatalf("a refused row must still say which shape was asked for: %v", res.Signals)
	}
}

// Sets of the sizes that failed in the 2026-10-14 run are scored in full
// through the listed schema; sets at the limit stay keyed.
func TestLargeSetsAreScoredThroughTheListedSchema(t *testing.T) {
	for _, n := range []int{69, 70, 79, 100, 101, 110} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			fake := &fakeAnthropic{limit: 69, answer: func(listed bool, keys []string) string {
				if listed {
					return listedAnswer(n, nil)
				}
				return keyedAnswer(keys)
			}}
			res := runFrontier(t, fake.server(t), 0, largeInput(n)) // default threshold
			if res.Fallback || len(res.Scores) != n {
				t.Fatalf("n=%d: %+v", n, res)
			}
			wantListed := n > DefaultFrontierKeyedMax
			if got := res.Signals["scores_listed"] == 1; got != wantListed {
				t.Fatalf("n=%d listed=%v, want %v", n, got, wantListed)
			}
			system, _ := json.Marshal(fake.last["system"])
			if strings.Contains(string(system), frontierListedSystem) != wantListed {
				t.Fatalf("n=%d listed instruction present=%v", n, !wantListed)
			}
			want := Frontier{View: ViewClaimed}.templateSHA256(wantListed)
			if res.TemplateSHA256 != want || (!wantListed && res.TemplateSHA256 != (Frontier{View: ViewClaimed}).TemplateSHA256()) {
				t.Fatalf("n=%d template %s, want %s", n, res.TemplateSHA256, want)
			}
			if wantListed && res.Scores[0].PeerID != "repow-000" {
				t.Fatalf("n=%d top = %s, want the first key's peer (highest score)", n, res.Scores[0].PeerID)
			}
		})
	}
}

// The listed schema's grammar does not grow with the candidates: the
// request carries no candidate key at all.
func TestListedSchemaDoesNotNameTheCandidates(t *testing.T) {
	small, _ := scoresSchema([]string{"c1", "c2"}, 1)
	big, _ := scoresSchema(make([]string, 500), 1)
	a, _ := json.Marshal(small)
	b, _ := json.Marshal(big)
	if string(a) != string(b) {
		t.Fatalf("listed schema depends on the candidates:\n%s\n%s", a, b)
	}
	var decoded map[string]any // as the API reads it
	_ = json.Unmarshal(b, &decoded)
	if why := schemaRuleBroken(map[string]any{"type": "object", "properties": map[string]any{"scores": decoded}, "required": []any{"scores"}, "additionalProperties": false}); why != "" {
		t.Fatalf("listed schema breaks a structured-output rule: %s", why)
	}
}

func TestKeyedMaxIsConfigurable(t *testing.T) {
	fake := &fakeAnthropic{limit: 1000, answer: func(listed bool, keys []string) string {
		if listed {
			return listedAnswer(4, nil)
		}
		return keyedAnswer(keys)
	}}
	if res := runFrontier(t, fake.server(t), 3, largeInput(4)); res.Fallback || res.Signals["scores_listed"] != 1 {
		t.Fatalf("4 candidates over KeyedMax 3: %+v", res)
	}
	if res := runFrontier(t, fake.server(t), 4, largeInput(4)); res.Fallback || res.Signals["scores_listed"] != 0 {
		t.Fatalf("4 candidates at KeyedMax 4: %+v", res)
	}
}

// A listed answer that misses, repeats or invents a key is incomplete, which
// does not count against the provider; a malformed one is unparseable.
func TestListedAnswersAreCheckedForCompleteness(t *testing.T) {
	const n = 80
	cases := map[string]struct {
		mutate func([]map[string]any) []map[string]any
		text   string
		want   string
	}{
		"missing key":  {mutate: func(it []map[string]any) []map[string]any { return it[:n-1] }, want: FallbackIncomplete},
		"repeated key": {mutate: func(it []map[string]any) []map[string]any { it[1]["key"] = "c1"; return it }, want: FallbackIncomplete},
		"unknown key":  {mutate: func(it []map[string]any) []map[string]any { it[0]["key"] = "c999"; return it }, want: FallbackIncomplete},
		"extra key": {mutate: func(it []map[string]any) []map[string]any {
			return append(it, map[string]any{"key": "c81", "score": 0.1})
		}, want: FallbackIncomplete},
		"empty list":     {text: `{"scores":[],"confidence":0.5}`, want: FallbackIncomplete},
		"score > 1":      {mutate: func(it []map[string]any) []map[string]any { it[3]["score"] = 1.5; return it }, want: FallbackUnparseable},
		"no key":         {mutate: func(it []map[string]any) []map[string]any { delete(it[2], "key"); return it }, want: FallbackUnparseable},
		"keyed instead":  {text: `{"scores":{"c1":0.5},"confidence":0.5}`, want: FallbackUnparseable},
		"no confidence":  {text: `{"scores":[]}`, want: FallbackUnparseable},
		"scores missing": {text: `{"confidence":0.5}`, want: FallbackUnparseable},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAnthropic{limit: 69, answer: func(bool, []string) string {
				if c.text != "" {
					return c.text
				}
				return listedAnswer(n, c.mutate)
			}}
			res := runFrontier(t, fake.server(t), 0, largeInput(n))
			if !res.Fallback || res.FallbackReason != c.want {
				t.Fatalf("result = %+v, want %s", res, c.want)
			}
			if res.Signals["scores_listed"] != 1 || res.Signals["input_tokens"] != 10 {
				t.Fatalf("a failed answered row must keep its shape and tokens: %v", res.Signals)
			}
		})
	}
	if providerFailure(FallbackIncomplete) {
		t.Fatal("an incomplete listed answer counts against the provider")
	}
}
