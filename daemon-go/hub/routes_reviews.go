package hub

// routes_reviews.go owns the "reviews" HTTP route group: tracked PRs awaiting an
// agent's re-review. Port of repowire/daemon/routes/reviews.py +
// repowire/daemon/review_queue_store.py + repowire/daemon/gh_pr.py.
//
//	POST   /reviews            mark a PR reviewed (reviewer, pr_url, last_reviewed_sha?)
//	GET    /reviews?reviewer=  list tracked PRs for a reviewer, enriched via `gh api`
//	DELETE /reviews/{pr_url}    remove a tracked PR (404 if absent)
//
// The store records (reviewer, pr_url, last_reviewed_sha) in a JSON file
// (~/.repowire/review_queue.json) — there is no SQLite table for it (matches
// Python: review_queue_store.py is JSON-backed, schema-v12 has no review_queue
// table). At GET time each entry is enriched with the PR's current head SHA +
// state via `gh api` (cached 60s), and my_action is derived. Fail loud over
// silent-degrade only where it matters: an unreachable PR enriches to
// state="unknown" / my_action="unknown" rather than erroring the whole list
// (matches Python — the merged-since-review surface is the value).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ReviewEntry is a tracked PR for a single reviewer. JSON tags match the Python
// ReviewEntry dataclass so the on-disk file is interchangeable with the Python
// daemon's review_queue.json.
type ReviewEntry struct {
	PRURL           string `json:"pr_url"`
	LastReviewedSHA string `json:"last_reviewed_sha,omitempty"`
	RecordedAt      string `json:"recorded_at"`
}

// ReviewQueueStore is a JSON-backed store of reviewer -> []ReviewEntry. All
// methods are safe for concurrent use; the file is rewritten atomically on every
// mutation (small, low-frequency data — no dirty-flag debounce, matching Python).
type ReviewQueueStore struct {
	path string
	mu   sync.Mutex
	data map[string][]ReviewEntry
}

// NewReviewQueueStore opens (or lazily creates) the JSON store at path, loading
// any existing rows. A corrupt or wrong-shaped file is logged-by-skip: we start
// empty rather than refuse to boot (matches Python _load).
func NewReviewQueueStore(path string) *ReviewQueueStore {
	s := &ReviewQueueStore{path: path, data: map[string][]ReviewEntry{}}
	s.load()
	return s
}

// DefaultReviewQueuePath returns ~/.repowire/review_queue.json, matching the
// Python default.
func DefaultReviewQueuePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "review_queue.json"
	}
	return filepath.Join(home, ".repowire", "review_queue.json")
}

func (s *ReviewQueueStore) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return // missing file → empty store
	}
	var parsed map[string][]ReviewEntry
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// Corrupt/wrong-shape file: start empty rather than crash (Python parity).
		return
	}
	for reviewer, entries := range parsed {
		kept := make([]ReviewEntry, 0, len(entries))
		for _, e := range entries {
			if e.PRURL == "" {
				continue
			}
			if e.RecordedAt == "" {
				e.RecordedAt = nowISO()
			}
			kept = append(kept, e)
		}
		s.data[reviewer] = kept
	}
}

// persist rewrites the file atomically (tmp + rename), mirroring
// peer_registry._persist_mappings. Best-effort: a write failure is swallowed
// (the in-memory state stays authoritative for this process life).
func (s *ReviewQueueStore) persist() {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	payload, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// Upsert inserts or updates the (reviewer, pr_url) entry. Returns the stored row.
func (s *ReviewQueueStore) Upsert(reviewer, prURL, lastReviewedSHA string) ReviewEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowISO()
	entries := s.data[reviewer]
	for i := range entries {
		if entries[i].PRURL == prURL {
			entries[i].LastReviewedSHA = lastReviewedSHA
			entries[i].RecordedAt = now
			s.data[reviewer] = entries
			s.persist()
			return entries[i]
		}
	}
	e := ReviewEntry{PRURL: prURL, LastReviewedSHA: lastReviewedSHA, RecordedAt: now}
	s.data[reviewer] = append(entries, e)
	s.persist()
	return e
}

// ListFor returns a copy of the tracked entries for reviewer.
func (s *ReviewQueueStore) ListFor(reviewer string) []ReviewEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ReviewEntry, len(s.data[reviewer]))
	copy(out, s.data[reviewer])
	return out
}

// Delete removes the (reviewer, pr_url) entry. Returns true if something was
// removed.
func (s *ReviewQueueStore) Delete(reviewer, prURL string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.data[reviewer]
	if len(entries) == 0 {
		return false
	}
	kept := entries[:0:0]
	for _, e := range entries {
		if e.PRURL != prURL {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(entries) {
		return false
	}
	if len(kept) > 0 {
		s.data[reviewer] = kept
	} else {
		delete(s.data, reviewer)
	}
	s.persist()
	return true
}

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

// ---------------------------------------------------------------------------
// gh api enrichment (port of gh_pr.py): best-effort PR head SHA + state with a
// 60s TTL cache keyed on the PR URL.
// ---------------------------------------------------------------------------

// prInfo is a snapshot of a PR's current state. state ∈ {open, merged, closed,
// unknown}. unknown means the API was unreachable (network/gh-auth/not-found) —
// callers fall back to stored values.
type prInfo struct {
	HeadSHA string
	State   string
}

const ghCacheTTL = 60 * time.Second

var prURLRe = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

type prCacheEntry struct {
	at   time.Time
	info prInfo
}

var (
	prCacheMu sync.Mutex
	prCache   = map[string]prCacheEntry{}
)

func parsePRURL(prURL string) (owner, repo, number string, ok bool) {
	m := prURLRe.FindStringSubmatch(strings.TrimSpace(prURL))
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}

// runGHAPI invokes `gh api <path>` and returns parsed JSON, or nil on any
// failure (gh missing, timeout, non-zero exit, bad JSON).
func runGHAPI(path string) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "api", path).Output()
	if err != nil {
		return nil
	}
	var data map[string]any
	if err := json.Unmarshal(out, &data); err != nil {
		return nil
	}
	return data
}

// fetchPRInfo returns the PR's current head SHA and merge/close state, cached for
// ghCacheTTL. On any error returns {state: "unknown"} so callers fall back to
// stored values.
func fetchPRInfo(prURL string) prInfo {
	owner, repo, number, ok := parsePRURL(prURL)
	if !ok {
		return prInfo{State: "unknown"}
	}
	now := time.Now()
	prCacheMu.Lock()
	if c, found := prCache[prURL]; found && now.Sub(c.at) < ghCacheTTL {
		prCacheMu.Unlock()
		return c.info
	}
	prCacheMu.Unlock()

	data := runGHAPI("repos/" + owner + "/" + repo + "/pulls/" + number)
	var info prInfo
	if data == nil {
		info = prInfo{State: "unknown"}
	} else {
		if head, hok := data["head"].(map[string]any); hok {
			if sha, sok := head["sha"].(string); sok {
				info.HeadSHA = sha
			}
		}
		if merged, _ := data["merged"].(bool); merged {
			info.State = "merged"
		} else if rs, _ := data["state"].(string); rs == "open" || rs == "closed" {
			info.State = rs
		} else {
			info.State = "unknown"
		}
	}

	prCacheMu.Lock()
	prCache[prURL] = prCacheEntry{at: now, info: info}
	prCacheMu.Unlock()
	return info
}

func deriveReviewAction(state, lastReviewedSHA, currentHeadSHA string) string {
	switch state {
	case "open":
		if lastReviewedSHA != "" && currentHeadSHA != "" && lastReviewedSHA == currentHeadSHA {
			return "none-needed"
		}
		return "re-review-suggested"
	case "merged":
		return "merged-since-review"
	case "closed":
		return "closed-since-review"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// HTTP routes.
// ---------------------------------------------------------------------------

// markReviewedRequest mirrors the Python MarkReviewedRequest body.
type markReviewedRequest struct {
	Reviewer        string  `json:"reviewer"`
	PRURL           string  `json:"pr_url"`
	LastReviewedSHA *string `json:"last_reviewed_sha,omitempty"`
}

// reviewItem mirrors the Python ReviewItem wire shape.
type reviewItem struct {
	PRURL           string  `json:"pr_url"`
	LastReviewedSHA *string `json:"last_reviewed_sha"`
	RecordedAt      string  `json:"recorded_at"`
	CurrentHeadSHA  *string `json:"current_head_sha"`
	State           string  `json:"state"`
	MyAction        string  `json:"my_action"`
}

// registerReviewRoutes attaches POST/GET /reviews and DELETE /reviews/{pr_url}.
// DELETE is served off the "/reviews/" subtree prefix so the stdlib mux dispatches
// the path-encoded pr_url without a router dependency (the pr_url is URL-encoded).
func (h *Hub) registerReviewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /reviews", h.requireAuth(h.markReviewed))
	mux.HandleFunc("GET /reviews", h.requireAuth(h.listReviews))
	mux.HandleFunc("DELETE /reviews/{pr_url}", h.requireAuth(h.handleDeleteReview))
}

func (h *Hub) markReviewed(w http.ResponseWriter, r *http.Request) {
	var req markReviewedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "malformed request: "+err.Error())
		return
	}
	if err := h.markReviewedDirect(req); err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

func (h *Hub) markReviewedDirect(req markReviewedRequest) error {
	if req.Reviewer == "" || req.PRURL == "" {
		return routeErr(http.StatusUnprocessableEntity, "reviewer and pr_url are required")
	}
	sha := ""
	if req.LastReviewedSHA != nil {
		sha = *req.LastReviewedSHA
	}
	if sha == "" {
		// Best-effort fill of the current head SHA; degrade to empty on failure
		// (every future read surfaces re-review-suggested until filled in).
		sha = fetchPRInfo(req.PRURL).HeadSHA
	}
	h.reviews.Upsert(req.Reviewer, req.PRURL, sha)
	return nil
}

func (h *Hub) listReviews(w http.ResponseWriter, r *http.Request) {
	reviewer := r.URL.Query().Get("reviewer")
	items, err := h.listReviewsDirect(reviewer)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": items})
}

func (h *Hub) listReviewsDirect(reviewer string) ([]reviewItem, error) {
	if reviewer == "" {
		return nil, routeErr(http.StatusUnprocessableEntity, "reviewer query parameter is required")
	}
	entries := h.reviews.ListFor(reviewer)
	items := make([]reviewItem, 0, len(entries))
	for _, e := range entries {
		info := fetchPRInfo(e.PRURL)
		items = append(items, reviewItem{
			PRURL:           e.PRURL,
			LastReviewedSHA: strPtr(e.LastReviewedSHA),
			RecordedAt:      e.RecordedAt,
			CurrentHeadSHA:  strPtr(info.HeadSHA),
			State:           info.State,
			MyAction:        deriveReviewAction(info.State, e.LastReviewedSHA, info.HeadSHA),
		})
	}
	// Stable order so the dashboard list doesn't jitter between reads.
	sort.SliceStable(items, func(i, j int) bool { return items[i].RecordedAt < items[j].RecordedAt })
	return items, nil
}

func (h *Hub) handleDeleteReview(w http.ResponseWriter, r *http.Request) {
	encoded := r.PathValue("pr_url")
	prURL, err := url.PathUnescape(encoded)
	if err != nil {
		prURL = encoded // tolerate an already-decoded path
	}
	reviewer := r.URL.Query().Get("reviewer")
	if reviewer == "" {
		writeError(w, http.StatusUnprocessableEntity, "reviewer query parameter is required")
		return
	}
	if !h.reviews.Delete(reviewer, prURL) {
		writeError(w, http.StatusNotFound, "No tracked review for that PR")
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}
