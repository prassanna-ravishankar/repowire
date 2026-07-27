package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// postJSON is a small helper: marshal body, POST it to the mux, return the
// recorder.
func postLifecycleJSON(t *testing.T, mux *http.ServeMux, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestRegisterPeerEndpoint is the primary handler test: POST /peers registers a
// peer through the registry FSM and returns the canonical peer_id + assigned
// display_name in the Python wire shape. A follow-up offline + unregister
// exercises the rest of the lifecycle group, including the 404 path.
func TestRegisterPeerEndpoint(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)

	path := "/work/myproj"
	rec := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{
		Name:    "myproj-claude-code",
		Path:    &path,
		Backend: proto.AgentClaudeCode,
		Circle:  strptr("default"),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /peers: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode RegisterResponse: %v", err)
	}
	if !resp.OK {
		t.Fatalf("RegisterResponse.ok must be true: %+v", resp)
	}
	if resp.PeerID == "" {
		t.Fatalf("a canonical peer_id must be minted, got empty")
	}
	if resp.DisplayName != "myproj-claude-code" {
		t.Fatalf("display_name: want myproj-claude-code, got %q", resp.DisplayName)
	}
	if !resp.PaneAssigned {
		t.Fatalf("pane_assigned must be true when no pane was requested")
	}

	// The peer must now resolve in the registry, ONLINE, keyed by its peer_id.
	p, ok := h.reg.GetPeer(proto.PeerID(resp.PeerID))
	if !ok {
		t.Fatalf("registered peer must resolve by peer_id %q", resp.PeerID)
	}
	if p.Status != proto.StatusOnline {
		t.Fatalf("freshly registered (no pane+runtime) peer must be ONLINE, got %s", p.Status)
	}

	// POST /peers/{name}/offline → 200 with the OfflineResponse shape.
	offRec := postLifecycleJSON(t, mux, "/peers/"+resp.DisplayName+"/offline", OfflineRequest{})
	if offRec.Code != http.StatusOK {
		t.Fatalf("offline: want 200, got %d (%s)", offRec.Code, offRec.Body.String())
	}
	var off OfflineResponse
	if err := json.Unmarshal(offRec.Body.Bytes(), &off); err != nil {
		t.Fatalf("decode OfflineResponse: %v", err)
	}
	if !off.OK {
		t.Fatalf("OfflineResponse.ok must be true")
	}
	if p2, _ := h.reg.GetPeer(proto.PeerID(resp.PeerID)); p2.Status != proto.StatusOffline {
		t.Fatalf("peer must be OFFLINE after /offline, got %s", p2.Status)
	}

	// POST /peer/unregister of a live name → 200; a second call → 404.
	unRec := postLifecycleJSON(t, mux, "/peer/unregister", UnregisterPeerRequest{Name: resp.DisplayName})
	if unRec.Code != http.StatusOK {
		t.Fatalf("unregister: want 200, got %d (%s)", unRec.Code, unRec.Body.String())
	}
	if _, ok := h.reg.GetPeer(proto.PeerID(resp.PeerID)); ok {
		t.Fatalf("peer must be gone from the registry after unregister")
	}
	gone := postLifecycleJSON(t, mux, "/peer/unregister", UnregisterPeerRequest{Name: resp.DisplayName})
	if gone.Code != http.StatusNotFound {
		t.Fatalf("unregister of an unknown peer: want 404, got %d (%s)", gone.Code, gone.Body.String())
	}
}

func TestRegisterPeerRequiresCircleButAcceptsLiteralDefault(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)

	missing := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{Name: "worker"})
	if missing.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing circle: want 422, got %d (%s)", missing.Code, missing.Body.String())
	}
	explicit := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{Name: "worker", Circle: strptr("default")})
	if explicit.Code != http.StatusOK {
		t.Fatalf("explicit literal default: want 200, got %d (%s)", explicit.Code, explicit.Body.String())
	}
}

func TestPaneRegistrationDerivesCircleAndRoleFromSpawnOwnership(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	path, pane := t.TempDir(), "%77"
	evidence := &service.TmuxPaneEvidence{PaneID: pane, TmuxSession: "trusted:worker", CurrentPath: path}
	ownership := service.NewFileOwnership("test-host", func(id string) *service.TmuxPaneEvidence {
		if id == pane {
			return evidence
		}
		return nil
	})
	ownership.Record(service.OwnershipRecord{
		PaneID: pane, Path: path, Backend: string(proto.AgentCodex), Circle: "trusted",
		Role: string(proto.RoleAgent), TmuxSession: evidence.TmuxSession, Machine: "test-host",
	})

	h := newTestHub(t)
	h.WithSpawn(service.NewSpawnService(nil, ownership, nil, nil), nil, nil, "test-host", proto.CircleBoundarySession)
	mux := http.NewServeMux()
	h.Routes(mux)

	// No caller-selected circle or role: both come from the live ownership proof.
	rec := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{
		Name: "worker", Path: &path, PaneID: &pane, Backend: proto.AgentCodex,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("pane registration: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var registered RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	p, ok := h.reg.GetPeer(proto.PeerID(registered.PeerID))
	if !ok || p.Circle != "trusted" || p.Role != proto.RoleAgent {
		t.Fatalf("pane peer = %+v, want trusted agent", p)
	}
}

func TestManualPaneRegistrationUsesLiveTmuxCircleAndAgentRole(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	path, pane := t.TempDir(), "%79"
	ownership := service.NewFileOwnership("test-host", func(id string) *service.TmuxPaneEvidence {
		if id == pane {
			return &service.TmuxPaneEvidence{PaneID: pane, TmuxSession: "manual:window", CurrentPath: path}
		}
		return nil
	})
	h := newTestHub(t)
	h.WithSpawn(service.NewSpawnService(nil, ownership, nil, nil), nil, nil, "test-host", proto.CircleBoundarySession)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{Name: "manual", Path: &path, PaneID: &pane})
	if rec.Code != http.StatusOK {
		t.Fatalf("manual pane registration: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var registered RegisterResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &registered)
	p, ok := h.reg.GetPeer(proto.PeerID(registered.PeerID))
	if !ok || p.Circle != "manual" || p.Role != proto.RoleAgent {
		t.Fatalf("manual pane peer = %+v, want manual agent", p)
	}
}

func TestWindowBoundaryRegistrationUsesStableWindowCircle(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	path, pane := t.TempDir(), "%80"
	ownership := service.NewFileOwnership("test-host", func(id string) *service.TmuxPaneEvidence {
		if id == pane {
			return &service.TmuxPaneEvidence{PaneID: pane, SessionName: "mesh", WindowID: "@19", TmuxSession: "mesh:renamable", CurrentPath: path}
		}
		return nil
	})
	h := newTestHub(t)
	h.WithSpawn(service.NewSpawnService(nil, ownership, nil, nil), nil, nil, "test-host", proto.CircleBoundaryWindow)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{Name: "manual", Path: &path, PaneID: &pane})
	if rec.Code != http.StatusOK {
		t.Fatalf("window pane registration: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var registered RegisterResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &registered)
	p, _ := h.reg.GetPeer(proto.PeerID(registered.PeerID))
	if p.Circle != "window-19" {
		t.Fatalf("circle = %q, want window-19", p.Circle)
	}
}

func TestPaneRegistrationRejectsUnprovedOrContradictoryIdentity(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)
	path, pane := t.TempDir(), "%88"
	if rec := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{
		Name: "worker", Path: &path, PaneID: &pane, Circle: strptr("default"),
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("unproved pane registration: want 403, got %d (%s)", rec.Code, rec.Body.String())
	}

	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	evidence := &service.TmuxPaneEvidence{PaneID: pane, TmuxSession: "trusted:worker", CurrentPath: path}
	ownership := service.NewFileOwnership("test-host", func(id string) *service.TmuxPaneEvidence {
		if id == pane {
			return evidence
		}
		return nil
	})
	ownership.Record(service.OwnershipRecord{
		PaneID: pane, Path: path, Backend: string(proto.AgentClaudeCode), Circle: "trusted",
		Role: string(proto.RoleAgent), TmuxSession: evidence.TmuxSession, Machine: "test-host",
	})
	h.WithSpawn(service.NewSpawnService(nil, ownership, nil, nil), nil, nil, "test-host", proto.CircleBoundarySession)

	if rec := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{
		Name: "worker", Path: &path, PaneID: &pane, Circle: strptr("other"),
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("contradictory pane circle: want 403, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{
		Name: "worker", Path: &path, PaneID: &pane, Role: proto.RoleOrchestrator,
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("contradictory pane role: want 403, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestSetDescriptionUnknownPeer404 covers the description endpoint's 404 path:
// an unknown name must not be papered over.
func TestSetDescriptionUnknownPeer404(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := postLifecycleJSON(t, mux, "/peers/nope-claude-code/description", SetDescriptionRequest{
		Description: "reviewing PR #1",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("description for unknown peer: want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestRegisterPeerPersistsResumeCapability(t *testing.T) {
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	h := newTestHub(t)
	h.store = store
	mux := http.NewServeMux()
	h.Routes(mux)

	path := t.TempDir()
	runtimeID := "runtime-123"
	rec := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{
		Name:    "codex-1",
		Path:    &path,
		Backend: proto.AgentCodex,
		Circle:  strptr("default"),
		Metadata: map[string]any{
			"runtime_session_id": runtimeID,
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /peers: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	backend := string(proto.AgentCodex)
	binding, err := store.GetByRuntimeSession(context.Background(), runtimeID, &backend, &path)
	if err != nil {
		t.Fatalf("GetByRuntimeSession: %v", err)
	}
	if binding == nil {
		t.Fatal("expected session binding for registered peer")
	}
	if binding.ResumeCapability["strategy"] != "codex_resume" {
		t.Fatalf("resume_capability = %v, want codex_resume strategy", binding.ResumeCapability)
	}
	if binding.ResumeCapability["supported"] != true {
		t.Fatalf("resume_capability.supported = %v, want true", binding.ResumeCapability["supported"])
	}
}

func strptr(s string) *string { return &s }
