package routing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Clef families. The family is both the arm prefix and the request's model
// selector, which Cloudflare requires to match the model in the URL.
const (
	ClefFamily      = "clef"
	ClefFlashFamily = "clef-flash"
)

// clefContext is each family's documented context window in tokens
// (developers.cloudflare.com/workers-ai/models/clef{,-flash}/).
var clefContext = map[string]int64{ClefFamily: 65536, ClefFlashFamily: 24576}

// Cloudflare truncates a long state to fit the context without saying so,
// which would score candidates the model never saw. The invariant is the
// provider's own count: an answer is scored only if usage.input_tokens is
// positive and more than clefCeilingTokens below the window. Before that, a
// cheap local heuristic skips paying for an obviously oversized state: the
// request body in bytes must leave clefReserveTokens of the window. Bytes
// usually overcount tokens, but not always (the Clef tokenizers NFC-normalize
// before byte-level BPE, so some characters cost more tokens than bytes), and
// the server tokenizes its own rendering of the state, not our bytes. No
// client-side byte budget is a bound; the provider count is.
const (
	clefReserveTokens = 1024
	clefCeilingTokens = 256
)

// Clef scores each candidate with Cloudflare's Clef decision model. It speaks
// System One with Jev's template, so the two differ only in the model.
type Clef struct {
	Family    string
	View      View
	APIKey    string
	AccountID string
	Model     string
	BaseURL   string
	HTTP      *http.Client
}

func (c Clef) Name() string { return c.Family + "-" + string(c.View) }

// Provider groups both Clef families under one breaker: the account's auth,
// billing and allocation are shared.
func (Clef) Provider() string { return "clef" }

// Identity is the configured model and template, kept on failure rows too.
func (c Clef) Identity() (string, string) { return c.Model, c.TemplateSHA256() }

func (c Clef) TemplateSHA256() string { return systemOneTemplate(c.View) }

// Rank records the request size, the window and, once answered, the input
// tokens in Signals, on failures as well as successes.
func (c Clef) Rank(ctx context.Context, in Input) (RouterResult, error) {
	window, known := clefContext[c.Family]
	if c.APIKey == "" || c.AccountID == "" || c.Model == "" || !known {
		return RouterResult{}, fail(FallbackUnconfigured)
	}
	state, keys := Project(in, c.View)
	body, err := json.Marshal(map[string]any{"model": c.Family, "state": state, "questions": systemOneQuestions(keys)})
	if err != nil {
		return RouterResult{}, fail(FallbackError)
	}
	signals := map[string]float64{"request_bytes": float64(len(body)), "context_tokens": float64(window)}
	noted := RouterResult{Signals: signals}
	if int64(len(body)) > window-clefReserveTokens {
		return noted, fail(FallbackStateTooLarge)
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/accounts/" + url.PathEscape(c.AccountID) + "/ai/run/" + c.Model
	status, raw, err := postJSON(ctx, c.HTTP, endpoint, c.APIKey, body)
	if err != nil {
		return noted, err
	}
	if status >= 300 {
		reason, code := clefError(status, raw)
		return noted, failHTTP(reason, status, code)
	}
	// The REST API wraps the answer in Cloudflare's {result, success, errors}
	// envelope; the model's own schema documents it bare. Accept either.
	var envelope struct {
		Result *systemOneResponse `json:"result"`
		systemOneResponse
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return noted, fail(FallbackUnparseable)
	}
	decoded := envelope.systemOneResponse
	if envelope.Result != nil {
		decoded = *envelope.Result
	}
	// Without a positive count, truncation cannot be ruled out: zero or less
	// cannot describe a non-empty request.
	if decoded.Usage == nil || decoded.Usage.InputTokens == nil {
		return noted, fail(FallbackUnparseable)
	}
	signals["input_tokens"] = float64(*decoded.Usage.InputTokens)
	if *decoded.Usage.InputTokens <= 0 {
		return noted, fail(FallbackUnparseable)
	}
	if *decoded.Usage.InputTokens >= window-clefCeilingTokens {
		return noted, fail(FallbackStateTruncated)
	}
	scores, err := decoded.scores(keys)
	if err != nil {
		return noted, err
	}
	// The configured id, not the response's model field (a bare selector),
	// so successes group with failures of the same configuration.
	top := topScore(scores)
	return RouterResult{Model: c.Model, TemplateSHA256: c.TemplateSHA256(), Scores: scores, Confidence: &top, Signals: signals}, nil
}

// clefError classifies a Cloudflare error response by its status and the
// first error's numeric code (developers.cloudflare.com/workers-ai/platform/errors/).
// No message is read.
func clefError(status int, body []byte) (reason, code string) {
	var envelope struct {
		Errors []struct {
			Code *int `json:"code"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Errors) > 0 && envelope.Errors[0].Code != nil {
		code = strconv.Itoa(*envelope.Errors[0].Code)
	}
	switch {
	case status == http.StatusRequestTimeout: // 3007 timeout, 3008 aborted: their stall, not our request
		return FallbackTimeout, code
	case code == "3036", code == "5035": // daily allocation used up; paid plan required
		return FallbackBilling, code
	}
	return statusReason(status), code
}
