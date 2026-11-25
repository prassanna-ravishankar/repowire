package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// layoutTmux is a container plug that also describes a fixed layout.
type layoutTmux struct {
	fakeTmux
	panes []service.PaneGeometry
}

func (l *layoutTmux) Describe() ([]service.PaneGeometry, error) { return l.panes, nil }

// argvProc answers one live pid's argv.
type argvProc struct {
	pid  int
	argv []string
}

func (p argvProc) Args(pid int) ([]string, bool) {
	if pid == p.pid {
		return p.argv, true
	}
	return nil, false
}
func (p argvProc) Alive(pid int) bool { return pid == p.pid }
func (argvProc) InTree(int, int) bool { return false }
func (argvProc) Terminate(int) error  { return nil }

// A snapshot (#505) records each pane's place, and for a pane that runs an
// agent: its identity, registered path, runtime session, whether that
// session can be resumed, and the permissions its live argv shows. A plain
// shell keeps only its place; the orchestrator is recorded but marked as one
// a restore leaves to its own start path. The route exists only with the
// experiment on.
func TestSwarmSnapshotRecordsLayoutAndAgents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	reg, err := peer.NewRegistry(ctx, memStore{}, liveSet{4242: true}, service.NewWebSocketTransport())
	if err != nil {
		t.Fatal(err)
	}
	register := func(path, pane string, role proto.PeerRole, pid int, session string) proto.PeerID {
		params := peer.AllocateParams{Circle: "work", Backend: proto.AgentClaudeCode, Path: &path, PaneID: &pane, Machine: "test-host", Role: role,
			Metadata: map[string]any{"hook_session_id": session}}
		if pid > 0 {
			params.AgentPID = &pid
		}
		id, _, err := reg.AllocateAndRegister(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	alpha := register("/work/alpha", "%1", proto.RoleAgent, 4242, "sess-alpha")
	orch := register("/work/orch", "%2", proto.RoleOrchestrator, 0, "sess-orch")

	layout := "c1a2,160x48,0,0{80x48,0,0,1,79x48,81,0,2}"
	tmux := &layoutTmux{panes: []service.PaneGeometry{
		{Session: "work", WindowIndex: 1, WindowName: "agents", WindowLayout: layout, PaneIndex: 0, PaneID: "%1", CWD: "/work/alpha/sub"},
		{Session: "work", WindowIndex: 1, WindowName: "agents", WindowLayout: layout, PaneIndex: 1, PaneID: "%2", CWD: "/work/orch"},
		{Session: "work", WindowIndex: 4, WindowName: "shell", WindowLayout: "bb62,80x24,0,0,3", PaneIndex: 0, PaneID: "%3", CWD: "/tmp"},
	}}
	h := NewHubWithTransport(reg, service.NewWebSocketTransport(), "")
	h.WithSpawn(service.NewSpawnService(tmux, service.NewFileOwnership("test-host", tmux.Probe), nil, nil), reg, nil, "test-host", proto.CircleBoundarySession)
	h.spawn.proc = argvProc{pid: 4242, argv: []string{"claude", "--permission-mode", "plan"}}

	get := func(enabled bool) *httptest.ResponseRecorder {
		h.WithSwarmSnapshot(enabled)
		mux := http.NewServeMux()
		h.registerSwarmRoutes(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/swarm/snapshot", nil))
		return rec
	}
	if rec := get(false); rec.Code != http.StatusNotFound {
		t.Fatalf("experiment off: %d", rec.Code)
	}
	rec := get(true)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot: %d %s", rec.Code, rec.Body.String())
	}
	var m SwarmManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Schema != SwarmSchema || m.Machine != "test-host" || m.TakenAt == "" || len(m.Panes) != 3 {
		t.Fatalf("manifest = %+v", m)
	}
	if m.Panes[0].WindowLayout != layout || m.Panes[2].WindowIndex != 4 || m.Panes[2].CWD != "/tmp" {
		t.Fatalf("geometry lost: %+v", m.Panes)
	}

	a := m.Panes[0].Agent
	if a == nil || a.PeerID != alpha || a.Backend != "claude-code" || a.Circle != "work" || a.RuntimeSessionID != "sess-alpha" || a.Exclude != "" {
		t.Fatalf("agent pane = %+v", a)
	}
	// The registered path, not the pane's cwd: the session binding checks it.
	if a.Path != "/work/alpha" {
		t.Fatalf("path %q, want the registered /work/alpha", a.Path)
	}
	if a.Permission.Mode != "plan" || a.Permission.Source != "argv" {
		t.Fatalf("permission = %+v", a.Permission)
	}
	if a.ResumeOK {
		t.Fatal("resume_ok without a session file")
	}
	if o := m.Panes[1].Agent; o == nil || o.PeerID != orch || o.Exclude == "" {
		t.Fatalf("orchestrator pane = %+v", o)
	}
	if m.Panes[2].Agent != nil {
		t.Fatalf("a shell pane carries an agent: %+v", m.Panes[2].Agent)
	}
}
