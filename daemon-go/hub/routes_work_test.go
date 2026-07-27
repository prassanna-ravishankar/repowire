package hub

// Tests for the jobs-work-runner area: the /work create+list+status+run HTTP
// path, plus the JobRunner → SessionControl → delivery dispatch path against
// fake spawn/registry/delivery seams. No tmux, no live transport — the seams are
// faked so the test is hermetic.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"

	_ "modernc.org/sqlite"
)

// workSchemaDDL seeds the three tables this area reads: tracked_work,
// calendar_entries, operations. Stamped user_version=12 so NewStore opens.
const workSchemaDDL = `
CREATE TABLE IF NOT EXISTS tracked_work (
    work_id TEXT PRIMARY KEY,
    title TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL DEFAULT 'general',
    state TEXT NOT NULL,
    state_reason TEXT,
    phase TEXT,
    progress_json TEXT NOT NULL DEFAULT '{}',
    progress_events_json TEXT NOT NULL DEFAULT '[]',
    owner_peer_id TEXT,
    assigned_peer_id TEXT,
    repowire_session_id TEXT,
    correlation_id TEXT,
    circle TEXT,
    created_by_peer_id TEXT,
    source_kind TEXT,
    source_id TEXT,
    scope TEXT,
    visibility TEXT NOT NULL DEFAULT 'circle',
    request_json TEXT NOT NULL DEFAULT '{}',
    deadline_at TEXT,
    expires_at TEXT,
    result_summary TEXT,
    result_data_json TEXT NOT NULL DEFAULT '{}',
    error_json TEXT NOT NULL DEFAULT '{}',
    artifacts_json TEXT NOT NULL DEFAULT '[]',
    provenance_json TEXT NOT NULL DEFAULT '{}',
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    cancel_requested_at TEXT,
    cancel_requested_by_peer_id TEXT,
    cancellation_reason TEXT,
    completed_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS calendar_entries (
    calendar_id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    kind TEXT NOT NULL,
    state TEXT NOT NULL,
    cron TEXT NOT NULL,
    next_due_at TEXT NOT NULL,
    owner_peer_id TEXT,
    assigned_peer_id TEXT,
    circle TEXT,
    created_by_peer_id TEXT,
    source_kind TEXT,
    source_id TEXT,
    scope TEXT,
    visibility TEXT NOT NULL,
    request_json TEXT NOT NULL,
    provenance_json TEXT NOT NULL,
    last_occurrence_work_id TEXT,
    last_materialized_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS operations (
    operation_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    state TEXT NOT NULL,
    target_json TEXT NOT NULL DEFAULT '{}',
    strategy TEXT,
    attempts_json TEXT NOT NULL DEFAULT '[]',
    result_json TEXT NOT NULL DEFAULT '{}',
    error_json TEXT NOT NULL DEFAULT '{}',
    provenance_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    completed_at TEXT
);
PRAGMA user_version=12;
`

func newWorkTestStore(t *testing.T) *state.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seed, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if _, err := seed.Exec(workSchemaDDL); err != nil {
		t.Fatalf("apply DDL: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
	s, err := state.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// --- fakes for the cross-area seams ---

type fakeControlRegistry struct {
	peers   []*proto.Peer
	byPane  map[string]*proto.Peer
	resolve map[string][]*proto.Peer
}

func (f *fakeControlRegistry) ResolvePeerStrict(identifier string, circle *string) []*proto.Peer {
	if f.resolve != nil {
		return f.resolve[identifier]
	}
	return nil
}
func (f *fakeControlRegistry) GetAllPeers() []*proto.Peer { return f.peers }
func (f *fakeControlRegistry) GetPeer(id proto.PeerID) (*proto.Peer, bool) {
	for _, p := range f.peers {
		if p.PeerID == id {
			return p, true
		}
	}
	return nil, false
}
func (f *fakeControlRegistry) GetPeerByPane(pane string) (*proto.Peer, bool) {
	p, ok := f.byPane[pane]
	return p, ok
}
func (f *fakeControlRegistry) UnregisterPeer(ctx context.Context, identifier string, circle *string) (bool, error) {
	return true, nil
}

// fakeSpawner records the spawn call and returns a fixed pane; the registry is
// pre-seeded so awaitSpawnedPeer resolves immediately by pane id.
type fakeSpawner struct {
	calls   int
	pane    string
	display string
	session string
}

func (f *fakeSpawner) ResolveCommand(b proto.AgentType, profile *string) (string, error) {
	return "echo launch", nil
}
func (f *fakeSpawner) Spawn(cfg service.SpawnConfig) (service.SpawnResult, error) {
	f.calls++
	return service.SpawnResult{DisplayName: f.display, TmuxSession: f.session, PaneID: f.pane}, nil
}

// fakeAskOpener records the dispatch ask and returns a fixed correlation id.
type fakeAskOpener struct {
	from, to, text string
	circle         string
	replyDelivery  string
	calls          int
}

func (f *fakeAskOpener) OpenScheduledAsk(ctx context.Context, fromPeer, toPeer, text string, circle *string, replyDelivery string) (string, error) {
	f.calls++
	f.from, f.to, f.text, f.replyDelivery = fromPeer, toPeer, text, replyDelivery
	if circle != nil {
		f.circle = *circle
	}
	return "ask-cid-123", nil
}

// TestWorkRoutesCreateListStatus drives the primary HTTP path: POST /work
// (per_fire defaulting), GET /work (list shows it), GET /work/{id}/status.
func TestWorkRoutesCreateListStatus(t *testing.T) {
	store := newWorkTestStore(t)
	h := &Hub{work: &workRoutes{store: store}}
	mux := http.NewServeMux()
	h.registerWorkRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body := `{"title":"nightly build","prompt":"build it","path":"/tmp/proj","backend":"claude-code"}`
	resp, err := http.Post(srv.URL+"/work", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /work: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /work status = %d, want 200", resp.StatusCode)
	}
	var created map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	resp.Body.Close()
	workID, _ := created["work_id"].(string)
	if workID == "" || created["job_id"] != workID {
		t.Fatalf("create response missing matching job_id/work_id: %#v", created)
	}
	statusMap, _ := created["status"].(map[string]any)
	if statusMap["state"] != "queued" {
		t.Fatalf("new work state = %v, want queued", statusMap["state"])
	}
	// per_fire defaulting: no assigned peer + path + backend → process_scope=per_fire.
	execution, _ := statusMap["execution"].(map[string]any)
	if execution["process_scope"] != "per_fire" {
		t.Fatalf("process_scope = %v, want per_fire", execution["process_scope"])
	}

	// GET /work lists it.
	listResp, err := http.Get(srv.URL + "/work")
	if err != nil {
		t.Fatalf("GET /work: %v", err)
	}
	var list map[string]any
	_ = json.NewDecoder(listResp.Body).Decode(&list)
	listResp.Body.Close()
	work, _ := list["work"].([]any)
	if len(work) != 1 {
		t.Fatalf("list returned %d work items, want 1", len(work))
	}

	// GET /jobs/{id} aliases status.
	stResp, err := http.Get(srv.URL + "/jobs/" + workID)
	if err != nil {
		t.Fatalf("GET /jobs/{id}: %v", err)
	}
	if stResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /jobs/{id} status = %d, want 200", stResp.StatusCode)
	}
	var st map[string]any
	_ = json.NewDecoder(stResp.Body).Decode(&st)
	stResp.Body.Close()
	got, _ := st["status"].(map[string]any)
	if got["work_id"] != workID {
		t.Fatalf("status work_id = %v, want %s", got["work_id"], workID)
	}

	// The legacy subtree dispatcher accepted a trailing slash on item routes.
	trailing, _ := http.Get(srv.URL + "/work/" + workID + "/")
	if trailing.StatusCode != http.StatusOK {
		t.Fatalf("trailing-slash status = %d, want 200", trailing.StatusCode)
	}
	trailing.Body.Close()

	// Unknown id → 404.
	missing, _ := http.Get(srv.URL + "/work/work-nope/status")
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", missing.StatusCode)
	}
	missing.Body.Close()
}

func TestWorkRunAndRetryReturn503WithoutStore(t *testing.T) {
	h := &Hub{work: &workRoutes{}}
	mux := http.NewServeMux()
	h.registerWorkRoutes(mux)

	for _, path := range []string{"/work/work-1/run", "/jobs/work-1/retry"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s status = %d, want 503", path, res.Code)
		}
	}
}

// TestJobRunnerDispatchSpawnsAndDelivers exercises the full dispatch path: a
// queued per_fire job acquires an executor via SessionControl (spawn strategy,
// resolved by pane id), records the attempt, and delivers through the scheduled
// ask opener with reply_delivery="pull".
func TestJobRunnerDispatchSpawnsAndDelivers(t *testing.T) {
	store := newWorkTestStore(t)
	ctx := context.Background()

	pane := "%42"
	spawnedPeer := &proto.Peer{
		PeerID:      "repow-default-abc12345",
		DisplayName: "claude-1",
		Path:        "/tmp/proj",
		Backend:     proto.AgentClaudeCode,
		Circle:      "default",
		Status:      proto.StatusOnline,
		PaneID:      &pane,
	}
	reg := &fakeControlRegistry{
		peers:  []*proto.Peer{spawnedPeer},
		byPane: map[string]*proto.Peer{pane: spawnedPeer},
	}
	spawner := &fakeSpawner{pane: pane, display: "claude-1", session: "default:0"}
	opener := &fakeAskOpener{}

	control := service.NewSessionControl(reg, spawner, store)
	runner := service.NewJobRunner(store, opener, control)
	runner.SetSenderPeerID("repow-jobs-svc")

	// Create a per_fire job (no assigned peer, path+backend set).
	merged := map[string]any{
		"execution": map[string]any{
			"prompt":        map[string]any{"body": "do the thing", "source": "inline"},
			"target":        map[string]any{"path": "/tmp/proj", "backend": "claude-code"},
			"process_scope": "per_fire",
			"continuity":    "fresh",
		},
	}
	work, err := store.CreateWork(ctx, state.WorkCreate{Title: "job", Request: merged, Circle: strp("default")})
	if err != nil {
		t.Fatalf("CreateWork: %v", err)
	}

	out, err := runner.RunJob(ctx, work.WorkID, true, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if out == nil {
		t.Fatal("RunJob returned nil work")
	}
	if out.State != "delivered" {
		t.Fatalf("work state = %q, want delivered", out.State)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner called %d times, want 1", spawner.calls)
	}
	if opener.calls != 1 {
		t.Fatalf("ask opener called %d times, want 1", opener.calls)
	}
	if opener.to != string(spawnedPeer.PeerID) {
		t.Fatalf("ask delivered to %q, want %q", opener.to, spawnedPeer.PeerID)
	}
	if opener.from != "repow-jobs-svc" {
		t.Fatalf("ask sender = %q, want repow-jobs-svc", opener.from)
	}
	if opener.replyDelivery != "pull" {
		t.Fatalf("reply_delivery = %q, want pull", opener.replyDelivery)
	}
	if !strings.Contains(opener.text, "do the thing") {
		t.Fatalf("dispatch prompt missing job body: %q", opener.text)
	}
	if out.CorrelationID == nil || *out.CorrelationID != "ask-cid-123" {
		t.Fatalf("correlation_id = %v, want ask-cid-123", out.CorrelationID)
	}

	// A durable acquire operation was recorded (audit trail).
	ops, err := store.ListOperations(ctx, "session.acquire_executor", "")
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}
	if len(ops) != 1 || ops[0].State != "completed" {
		t.Fatalf("acquire operation = %#v, want one completed", ops)
	}

	// The current attempt carries a per-fire release handle pointing at the pane.
	if !hasReleaseHandle(out) {
		t.Fatal("expected a per-fire release handle on the delivered attempt")
	}
}

// TestRunJobRejectsTerminalState confirms RunJob is a no-op for work not in a
// runnable state (the route layer gates this with a 409; the store gates the
// acquire as a defense-in-depth no-op).
func TestRunJobRejectsTerminalState(t *testing.T) {
	store := newWorkTestStore(t)
	ctx := context.Background()
	work, err := store.CreateWork(ctx, state.WorkCreate{Title: "done"})
	if err != nil {
		t.Fatalf("CreateWork: %v", err)
	}
	if _, err := store.UpdateWorkState(ctx, work.WorkID, state.WorkUpdate{State: "completed"}); err != nil {
		t.Fatalf("UpdateWorkState: %v", err)
	}
	runner := service.NewJobRunner(store, &fakeAskOpener{}, nil)
	out, err := runner.RunJob(ctx, work.WorkID, true, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if out != nil {
		t.Fatalf("RunJob on completed work returned %#v, want nil", out)
	}
}
