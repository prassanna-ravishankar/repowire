package routing

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Jev scores each candidate with one yes/no question in a single System One
// request and ranks by the yes probability.
type Jev struct {
	View    View
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client
}

func (j Jev) Name() string { return "jev-" + string(j.View) }

// Provider groups the Jev arms under one breaker.
func (Jev) Provider() string { return "jev" }

// Identity is the configured model and template, kept on failure rows too.
func (j Jev) Identity() (string, string) { return j.Model, j.TemplateSHA256() }

func (j Jev) TemplateSHA256() string { return systemOneTemplate(j.View) }

func (j Jev) Rank(ctx context.Context, in Input) (RouterResult, error) {
	if j.APIKey == "" {
		return RouterResult{}, fail(FallbackUnconfigured)
	}
	state, keys := Project(in, j.View)
	body, err := json.Marshal(map[string]any{"state": state, "model": j.Model, "questions": systemOneQuestions(keys)})
	if err != nil {
		return RouterResult{}, fail(FallbackError)
	}
	// Every row that built a request records its size: a provider refusing
	// a large card set (DecisionBench c1, about 125 KB, 400 on every row,
	// #462) is then read as request_error at that size, not a bare 400.
	signals := map[string]float64{"request_bytes": float64(len(body))}
	status, raw, err := postJSON(ctx, j.HTTP, strings.TrimRight(j.BaseURL, "/")+"/v1/systemone", j.APIKey, body)
	if err != nil {
		return RouterResult{Signals: signals}, err
	}
	if status >= 300 {
		return RouterResult{Signals: signals}, failHTTP(statusReason(status), status, jevCode(raw))
	}
	var decoded systemOneResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return RouterResult{Signals: signals}, fail(FallbackUnparseable)
	}
	signals = decoded.tokens(signals)
	scores, err := decoded.scores(keys)
	if err != nil {
		return RouterResult{Signals: signals}, err
	}
	model := decoded.Model
	if model == "" {
		model = j.Model
	}
	top := topScore(scores)
	res := RouterResult{Model: model, TemplateSHA256: j.TemplateSHA256(), Scores: scores, Confidence: &top, Signals: signals}
	return res, nil
}

// jevCode reads TypeSafe's detail.error_type from an error body.
func jevCode(body []byte) string {
	var envelope struct {
		Detail struct {
			ErrorType string `json:"error_type"`
		} `json:"detail"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return ""
	}
	return envelope.Detail.ErrorType
}
