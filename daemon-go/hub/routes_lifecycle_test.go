package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
)

// fakePaneLister returns a fixed set of live panes for the session-closed gate.
type fakePaneLister struct{ panes []PaneInfo }

func (f fakePaneLister) ListAllPanes() []PaneInfo { return f.panes }

// registerPanePeer registers a peer occupying the given tmux pane in a circle.
func registerPanePeer(t *testing.T, reg *peer.Registry, circle, pane string) proto.PeerID {
	t.Helper()
	tmuxSession := circle + ":window"
	id, _, err := reg.AllocateAndRegister(context.Background(), peer.AllocateParams{
		Circle:      circle,
		Backend:     proto.AgentClaudeCode,
		Role:        proto.RoleAgent,
		PaneID:      &pane,
		TmuxSession: &tmuxSession,
		Machine:     "test",
	})
	if err != nil {
		t.Fatalf("AllocateAndRegister: %v", err)
	}
	return id
}

func postLifecycle(t *testing.T, mux *http.ServeMux, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.RemoteAddr = "127.0.0.1:55555" // localhost-only gate
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestPaneDiedOfflinesPeer is the primary-endpoint test: a peer occupying a pane
// is ONLINE; POST /hooks/lifecycle/pane-died for its pane offlines it (non-
// terminal) and returns {"ok": true}.
func TestPaneDiedOfflinesPeer(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	lh := NewLifecycleHandler(h.reg, h.transport, fakePaneLister{}, nil, nil, proto.CircleBoundarySession)
	h.WithLifecycle(lh)
	h.Routes(mux)

	id := registerPanePeer(t, h.reg, "default", "%42")
	if p, ok := h.reg.GetPeer(id); !ok || p.Status != proto.StatusOnline {
		t.Fatalf("peer should start online, got %+v ok=%v", p, ok)
	}

	rec := postLifecycle(t, mux, "/hooks/lifecycle/pane-died", paneDiedRequest{PaneID: "%42"})
	if rec.Code != http.StatusOK {
		t.Fatalf("pane-died: status %d body %s", rec.Code, rec.Body.String())
	}
	var resp okResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.OK {
		t.Fatalf("expected {ok:true}, got %s (err=%v)", rec.Body.String(), err)
	}

	p, ok := h.reg.GetPeer(id)
	if !ok {
		t.Fatalf("peer should still exist (non-terminal offline)")
	}
	if p.Status != proto.StatusOffline {
		t.Fatalf("peer should be OFFLINE after pane-died, got %s", p.Status)
	}
}

// TestPaneDiedForgetsSpawnOwnership: pane death must drop the pane from the
// spawn-ownership store (the forgetSpawnedPane callback), so destructivePaneProof
// can't later authorize kill/restart against a reused pane id. Guards the call
// site that main.go wires to SpawnService.Ownership().Forget.
func TestPaneDiedForgetsSpawnOwnership(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	var forgotten []string
	lh := NewLifecycleHandler(h.reg, h.transport, fakePaneLister{},
		func(pane string) { forgotten = append(forgotten, pane) }, nil, proto.CircleBoundarySession)
	h.WithLifecycle(lh)
	h.Routes(mux)

	registerPanePeer(t, h.reg, "default", "%77")
	rec := postLifecycle(t, mux, "/hooks/lifecycle/pane-died", paneDiedRequest{PaneID: "%77"})
	if rec.Code != http.StatusOK {
		t.Fatalf("pane-died: status %d body %s", rec.Code, rec.Body.String())
	}
	if len(forgotten) != 1 || forgotten[0] != "%77" {
		t.Fatalf("forgetSpawnedPane should be called with %%77, got %v", forgotten)
	}
}

// TestPaneDiedNoPeerStillOK: a pane with no peer returns 200 (clears state, no-op).
func TestPaneDiedNoPeerStillOK(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.WithLifecycle(NewLifecycleHandler(h.reg, h.transport, fakePaneLister{}, nil, nil, proto.CircleBoundarySession))
	h.Routes(mux)

	rec := postLifecycle(t, mux, "/hooks/lifecycle/pane-died", paneDiedRequest{PaneID: "%999"})
	if rec.Code != http.StatusOK {
		t.Fatalf("pane-died with no peer should be 200, got %d", rec.Code)
	}
}

// TestPaneDiedRejectsRemote: a non-loopback client is 403 (localhost-only).
func TestPaneDiedRejectsRemote(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.WithLifecycle(NewLifecycleHandler(h.reg, h.transport, fakePaneLister{}, nil, nil, proto.CircleBoundarySession))
	h.Routes(mux)

	buf, _ := json.Marshal(paneDiedRequest{PaneID: "%1"})
	req := httptest.NewRequest(http.MethodPost, "/hooks/lifecycle/pane-died", bytes.NewReader(buf))
	req.RemoteAddr = "10.0.0.5:1234"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote client must be 403, got %d", rec.Code)
	}
}

// TestSessionClosedInconclusiveProbe: an empty pane probe is INCONCLUSIVE — the
// circle's peers must NOT be offlined (refuse to treat "no evidence" as "all
// gone"). This is the b9e5a66 evidence-gate.
func TestSessionClosedInconclusiveProbe(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	// Empty lister → probe returns nothing → inconclusive.
	h.WithLifecycle(NewLifecycleHandler(h.reg, h.transport, fakePaneLister{}, nil, nil, proto.CircleBoundarySession))
	h.Routes(mux)

	id := registerPanePeer(t, h.reg, "circle-a", "%7")

	rec := postLifecycle(t, mux, "/hooks/lifecycle/session-closed", sessionClosedRequest{SessionName: "circle-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("session-closed: status %d", rec.Code)
	}
	if p, _ := h.reg.GetPeer(id); p.Status != proto.StatusOnline {
		t.Fatalf("inconclusive probe must NOT offline the peer; got %s", p.Status)
	}
}

// TestSessionClosedSpuriousWhenSessionLive: a still-live named session is a
// spurious close — do nothing.
func TestSessionClosedSpuriousWhenSessionLive(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	// The session still has a live pane → spurious.
	lister := fakePaneLister{panes: []PaneInfo{{PaneID: "%7", Session: "circle-a"}}}
	h.WithLifecycle(NewLifecycleHandler(h.reg, h.transport, lister, nil, nil, proto.CircleBoundarySession))
	h.Routes(mux)

	id := registerPanePeer(t, h.reg, "circle-a", "%7")

	rec := postLifecycle(t, mux, "/hooks/lifecycle/session-closed", sessionClosedRequest{SessionName: "circle-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if p, _ := h.reg.GetPeer(id); p.Status != proto.StatusOnline {
		t.Fatalf("spurious close (session still live) must NOT offline; got %s", p.Status)
	}
}

func TestSessionClosedSelectsPeersByTmuxSession(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	lister := fakePaneLister{panes: []PaneInfo{{PaneID: "%other", Session: "other"}}}
	h.WithLifecycle(NewLifecycleHandler(h.reg, h.transport, lister, nil, nil, proto.CircleBoundaryWindow))
	h.Routes(mux)
	id := registerPanePeer(t, h.reg, "window-7", "%7")
	h.reg.UpdateTmuxSession(context.Background(), id, "closing-session:window")
	legacyPane := "%8"
	legacyID, _, err := h.reg.AllocateAndRegister(context.Background(), peer.AllocateParams{
		Circle: "closing-session", Backend: proto.AgentClaudeCode, Role: proto.RoleAgent,
		PaneID: &legacyPane, Machine: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	unrelatedID := registerPanePeer(t, h.reg, "closing-session", "%9")
	h.reg.UpdateTmuxSession(context.Background(), unrelatedID, "other:window")

	rec := postLifecycle(t, mux, "/hooks/lifecycle/session-closed", sessionClosedRequest{SessionName: "closing-session"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if p, _ := h.reg.GetPeer(id); p.Status != proto.StatusOffline {
		t.Fatalf("peer status = %s, want OFFLINE", p.Status)
	}
	if p, _ := h.reg.GetPeer(legacyID); p.Status != proto.StatusOffline {
		t.Fatalf("legacy peer status = %s, want OFFLINE", p.Status)
	}
	if p, _ := h.reg.GetPeer(unrelatedID); p.Status == proto.StatusOffline {
		t.Fatal("same-named logical circle in another tmux session was offlined")
	}
}

// TestSessionClosedOfflinesGonePane: session gone from tmux AND the peer's pane
// absent → offline it; a peer whose pane is still live is spared.
func TestSessionClosedOfflinesGonePane(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	// tmux is alive (some other session present) but circle-a is gone; %7 absent,
	// %8 still live.
	lister := fakePaneLister{panes: []PaneInfo{
		{PaneID: "%8", Session: "other"},
	}}
	h.WithLifecycle(NewLifecycleHandler(h.reg, h.transport, lister, nil, nil, proto.CircleBoundarySession))
	h.Routes(mux)

	dead := registerPanePeer(t, h.reg, "circle-a", "%7")  // pane gone → offline
	alive := registerPanePeer(t, h.reg, "circle-a", "%8") // pane live → spared

	rec := postLifecycle(t, mux, "/hooks/lifecycle/session-closed", sessionClosedRequest{SessionName: "circle-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if p, _ := h.reg.GetPeer(dead); p.Status != proto.StatusOffline {
		t.Fatalf("peer with absent pane must be offlined, got %s", p.Status)
	}
	if p, _ := h.reg.GetPeer(alive); p.Status != proto.StatusOnline {
		t.Fatalf("peer with live pane must be spared (pane-died owns it), got %s", p.Status)
	}
}

// TestSessionRenamedReCircles: peers identified by pane id move to the new circle.
func TestSessionRenamedReCircles(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.WithLifecycle(NewLifecycleHandler(h.reg, h.transport, fakePaneLister{}, nil, nil, proto.CircleBoundarySession))
	h.Routes(mux)

	id := registerPanePeer(t, h.reg, "old-circle", "%3")
	bad := postLifecycle(t, mux, "/hooks/lifecycle/session-renamed",
		sessionRenamedRequest{NewName: "bad name", PaneIDs: []string{"%3"}})
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid circle status %d", bad.Code)
	}

	rec := postLifecycle(t, mux, "/hooks/lifecycle/session-renamed",
		sessionRenamedRequest{NewName: "new-circle", PaneIDs: []string{"%3"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if p, _ := h.reg.GetPeer(id); p.Circle != "new-circle" || derefString(p.TmuxSession) != "new-circle:window" {
		t.Fatalf("renamed peer = %+v", p)
	}
}

func TestWindowRenamedUpdatesLocatorButNotCircle(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.WithLifecycle(NewLifecycleHandler(h.reg, h.transport, fakePaneLister{}, nil, nil, proto.CircleBoundarySession))
	h.Routes(mux)

	id := registerPanePeer(t, h.reg, "circle-x", "%5")

	rec := postLifecycle(t, mux, "/hooks/lifecycle/window-renamed",
		windowRenamedRequest{SessionName: "circle-x", NewName: "renamed-window", PaneIDs: []string{"%5"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	p, _ := h.reg.GetPeer(id)
	if p.Circle != "circle-x" {
		t.Fatalf("window rename must NOT change circle, got %s", p.Circle)
	}
	if derefString(p.TmuxSession) != "circle-x:renamed-window" {
		t.Fatalf("tmux session = %q", derefString(p.TmuxSession))
	}
}

func TestWindowBoundarySessionRenameKeepsCircle(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.WithLifecycle(NewLifecycleHandler(h.reg, h.transport, fakePaneLister{}, nil, nil, proto.CircleBoundaryWindow))
	h.Routes(mux)
	id := registerPanePeer(t, h.reg, "window-9", "%9")

	rec := postLifecycle(t, mux, "/hooks/lifecycle/session-renamed",
		sessionRenamedRequest{NewName: "renamed-session", PaneIDs: []string{"%9"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	p, _ := h.reg.GetPeer(id)
	if p.Circle != "window-9" || derefString(p.TmuxSession) != "renamed-session:window" {
		t.Fatalf("renamed window-boundary peer = %+v", p)
	}
}

// TestNameFieldLengthCap: a name field over 64 chars is rejected with 422.
func TestNameFieldLengthCap(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.WithLifecycle(NewLifecycleHandler(h.reg, h.transport, fakePaneLister{}, nil, nil, proto.CircleBoundarySession))
	h.Routes(mux)

	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	rec := postLifecycle(t, mux, "/hooks/lifecycle/pane-died", paneDiedRequest{PaneID: string(long)})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("over-length pane_id must be 422, got %d", rec.Code)
	}
}

// ensure the time import is exercised (registry hydration deadline parity).
var _ = time.Second
