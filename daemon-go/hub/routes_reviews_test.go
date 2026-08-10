package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// newReviewTestHub builds a Hub with only the review store wired (auth disabled)
// and a mux carrying just the review routes. It avoids the full transport/
// registry wiring so the handler test stays hermetic.
func newReviewTestHub(t *testing.T) (*http.ServeMux, *ReviewQueueStore) {
	t.Helper()
	store := NewReviewQueueStore(filepath.Join(t.TempDir(), "review_queue.json"))
	h := &Hub{reviews: store}
	mux := http.NewServeMux()
	h.registerReviewRoutes(mux)
	return mux, store
}

// TestReviewsPrimaryPath exercises the load-bearing flow: mark a PR reviewed
// (with an explicit SHA so no `gh api` shell-out happens), list it back, then
// delete it. A parse-failing PR URL means fetchPRInfo returns state="unknown"
// without touching the network, keeping the test offline-deterministic.
func TestReviewsPrimaryPath(t *testing.T) {
	mux, store := newReviewTestHub(t)

	// Use a non-github URL so enrichment short-circuits to state=unknown offline.
	prURL := "https://example.invalid/pr/1"
	sha := "abc123"

	// POST /reviews
	body, _ := json.Marshal(map[string]any{
		"reviewer":          "reviewer-bot",
		"pr_url":            prURL,
		"last_reviewed_sha": sha,
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reviews", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /reviews: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if entries := store.ListFor("reviewer-bot"); len(entries) != 1 || entries[0].LastReviewedSHA != sha {
		t.Fatalf("store after upsert: %+v", entries)
	}

	// GET /reviews?reviewer=
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reviews?reviewer=reviewer-bot", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /reviews: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Reviews []reviewItem `json:"reviews"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v; body=%s", err, rec.Body.String())
	}
	if len(listed.Reviews) != 1 {
		t.Fatalf("expected 1 review, got %d", len(listed.Reviews))
	}
	got := listed.Reviews[0]
	if got.PRURL != prURL {
		t.Errorf("pr_url: got %q, want %q", got.PRURL, prURL)
	}
	if got.LastReviewedSHA == nil || *got.LastReviewedSHA != sha {
		t.Errorf("last_reviewed_sha: got %v, want %q", got.LastReviewedSHA, sha)
	}
	if got.State != "unknown" {
		t.Errorf("state: got %q, want unknown (offline non-github URL)", got.State)
	}
	if got.MyAction != "unknown" {
		t.Errorf("my_action: got %q, want unknown", got.MyAction)
	}

	// DELETE /reviews/{pr_url} — known entry → 200.
	rec = httptest.NewRecorder()
	delPath := "/reviews/" + pathEscapeReview(prURL) + "?reviewer=reviewer-bot"
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, delPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE existing: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if entries := store.ListFor("reviewer-bot"); len(entries) != 0 {
		t.Fatalf("store after delete should be empty, got %+v", entries)
	}

	// DELETE again — now absent → 404.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, delPath, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE absent: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// pathEscapeReview mirrors how a client encodes a pr_url into the DELETE path.
func pathEscapeReview(s string) string {
	// The handler url.PathUnescape's the trailing segment; encode reserved chars.
	out := ""
	for _, c := range []byte(s) {
		switch {
		case c == '/' || c == ':' || c == '?' || c == '#' || c == '%':
			out += "%" + string("0123456789ABCDEF"[c>>4]) + string("0123456789ABCDEF"[c&0xf])
		default:
			out += string(c)
		}
	}
	return out
}
