package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

type scRegistry struct {
	peers  []*proto.Peer
	byPane map[string]*proto.Peer
}

func (r *scRegistry) ResolvePeerStrict(identifier string, circle *string) []*proto.Peer {
	var out []*proto.Peer
	for _, p := range r.peers {
		if string(p.PeerID) == identifier || string(p.DisplayName) == identifier {
			if circle != nil && p.Circle != *circle {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}
func (r *scRegistry) GetAllPeers() []*proto.Peer { return r.peers }
func (r *scRegistry) GetPeer(id proto.PeerID) (*proto.Peer, bool) {
	for _, p := range r.peers {
		if p.PeerID == id {
			return p, true
		}
	}
	return nil, false
}
func (r *scRegistry) GetPeerByPane(pane string) (*proto.Peer, bool) {
	p, ok := r.byPane[pane]
	return p, ok
}
func (r *scRegistry) UnregisterPeer(context.Context, string, *string) (bool, error) {
	return true, nil
}

type scSpawner struct {
	command string
	pane    string
	last    SpawnConfig
}

func (s *scSpawner) ResolveCommand(proto.AgentType, *string) (string, error) {
	return s.command, nil
}
func (s *scSpawner) Spawn(cfg SpawnConfig) (SpawnResult, error) {
	s.last = cfg
	return SpawnResult{DisplayName: "codex-1", TmuxSession: "default:0", PaneID: s.pane}, nil
}

type scResume struct{ plan map[string]any }

func (r scResume) Resolve(proto.AgentType, string, string, *string, map[string]any) (map[string]any, bool) {
	return r.plan, r.plan != nil
}

func scStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestBuildResumeCommandMatchesPythonBackends(t *testing.T) {
	cases := []struct {
		backend proto.AgentType
		base    string
		want    string
	}{
		{proto.AgentClaudeCode, "claude --dangerously-skip-permissions", "claude --dangerously-skip-permissions --resume runtime-123"},
		{proto.AgentCodex, "codex --dangerously-bypass-approvals-and-sandbox", "codex --dangerously-bypass-approvals-and-sandbox resume runtime-123"},
		{proto.AgentGemini, "gemini --yolo", "gemini --yolo --resume runtime-123"},
		{proto.AgentOpenCode, "opencode", "opencode --session runtime-123"},
		{proto.AgentAntigravity, "agy --dangerously-skip-permissions", "agy --dangerously-skip-permissions --conversation runtime-123"},
		{proto.AgentPi, "pi", "pi --session runtime-123"},
	}
	for _, tc := range cases {
		got, err := BuildResumeCommand(tc.base, tc.backend, "runtime-123")
		if err != nil {
			t.Fatalf("%s BuildResumeCommand: %v", tc.backend, err)
		}
		if got != tc.want {
			t.Fatalf("%s command = %q, want %q", tc.backend, got, tc.want)
		}
	}
}

func TestSessionControlBackendResumePassesResumeCommandToSpawn(t *testing.T) {
	ctx := context.Background()
	store := scStore(t)
	projectPath := t.TempDir()
	circle := "window-7"
	sourceKind := "calendar"
	pane := "%42"

	cal, err := store.CreateCalendarEntry(ctx, &state.CalendarEntry{
		Title:     "nightly",
		Kind:      "general",
		Cron:      "* * * * *",
		NextDueAt: "2026-07-08T12:00:00+00:00",
		Circle:    &circle,
		Request:   map[string]any{},
	})
	if err != nil {
		t.Fatalf("CreateCalendarEntry: %v", err)
	}
	_, err = store.UpdateCalendarRuntimeBinding(ctx, cal.CalendarID, map[string]any{
		"backend":            string(proto.AgentCodex),
		"path":               projectPath,
		"circle":             circle,
		"runtime_session_id": "runtime-123",
		"resume_capability": map[string]any{
			"supported": true,
			"strategy":  "codex_resume",
		},
	})
	if err != nil {
		t.Fatalf("UpdateCalendarRuntimeBinding: %v", err)
	}

	work, err := store.CreateWork(ctx, state.WorkCreate{
		Title:      "run job",
		Circle:     &circle,
		SourceKind: &sourceKind,
		SourceID:   &cal.CalendarID,
		Request: map[string]any{"execution": map[string]any{
			"target":        map[string]any{"path": projectPath, "backend": string(proto.AgentCodex)},
			"process_scope": "per_fire",
			"continuity":    "resume",
		}},
	})
	if err != nil {
		t.Fatalf("CreateWork: %v", err)
	}

	spawned := &proto.Peer{
		PeerID:      "repow-default-codex1",
		DisplayName: "codex-1",
		Path:        projectPath,
		Backend:     proto.AgentCodex,
		Circle:      circle,
		Status:      proto.StatusOnline,
		PaneID:      &pane,
	}
	reg := &scRegistry{peers: []*proto.Peer{spawned}, byPane: map[string]*proto.Peer{pane: spawned}}
	spawner := &scSpawner{command: "codex --dangerously-bypass-approvals-and-sandbox", pane: pane}
	resume := scResume{plan: map[string]any{
		"backend":            string(proto.AgentCodex),
		"runtime_session_id": "runtime-123",
		"capability":         map[string]any{"supported": true, "strategy": "codex_resume"},
	}}
	control := NewSessionControl(reg, spawner, store).WithResume(resume.Resolve)

	acq, err := control.AcquireExecutorForWork(ctx, work, map[string]any{"path": projectPath, "backend": string(proto.AgentCodex)}, "tester")
	if err != nil {
		t.Fatalf("AcquireExecutorForWork: %v", err)
	}
	if acq.Strategy != "backend_resume" {
		t.Fatalf("strategy = %q, want backend_resume", acq.Strategy)
	}
	want := "codex --dangerously-bypass-approvals-and-sandbox resume runtime-123"
	if spawner.last.Command != want {
		t.Fatalf("spawn command = %q, want %q", spawner.last.Command, want)
	}
}

func TestResolveLocalResumeRequiresCodexSessionFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projectPath := t.TempDir()
	sessionDir := filepath.Join(home, ".codex", "sessions", "2026", "07", "08")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	sessionPath := filepath.Join(sessionDir, "rollout-runtime-123.jsonl")
	line := `{"type":"session_meta","payload":{"cwd":` + mustJSONStr(t, projectPath) + `}}` + "\n"
	if err := os.WriteFile(sessionPath, []byte(line), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	plan, ok := ResolveLocalResume(proto.AgentCodex, projectPath, "runtime-123", nil, map[string]any{"supported": true})
	if !ok {
		t.Fatal("expected codex session file to validate as resumable")
	}
	if plan["runtime_session_id"] != "runtime-123" {
		t.Fatalf("runtime_session_id = %v, want runtime-123", plan["runtime_session_id"])
	}
	if _, ok := ResolveLocalResume(proto.AgentCodex, projectPath, "missing", nil, map[string]any{"supported": true}); ok {
		t.Fatal("missing codex session id must not validate")
	}
}

func TestRuntimeBindingForPeerEnrichesFromSessionBinding(t *testing.T) {
	ctx := context.Background()
	store := scStore(t)
	projectPath := t.TempDir()
	peerID := "repow-default-codex1"
	runtimeID := "runtime-123"
	sourceURI := "codex-rollout://2026/07/08/rollout-runtime-123.jsonl"
	created, err := store.UpsertObservation(ctx, state.Observation{
		PeerID:           &peerID,
		Backend:          string(proto.AgentCodex),
		ProjectPath:      &projectPath,
		RuntimeSessionID: &runtimeID,
		RuntimeSourceURI: &sourceURI,
		SourceCursor:     map[string]any{"line": float64(7)},
		ResumeCapability: map[string]any{"supported": true, "strategy": "codex_resume"},
		Status:           state.BindingResumable,
	})
	if err != nil {
		t.Fatalf("UpsertObservation: %v", err)
	}
	peer := &proto.Peer{
		PeerID:      proto.PeerID(peerID),
		DisplayName: "codex-1",
		Path:        projectPath,
		Backend:     proto.AgentCodex,
		Circle:      "default",
		Status:      proto.StatusOnline,
		Metadata:    map[string]any{"runtime_session_id": runtimeID},
	}

	binding := runtimeBindingForPeer(ctx, store, peer, nil)
	if binding["repowire_session_id"] != created.RepowireSessionID {
		t.Fatalf("repowire_session_id = %v, want %s", binding["repowire_session_id"], created.RepowireSessionID)
	}
	if binding["runtime_source_uri"] != sourceURI {
		t.Fatalf("runtime_source_uri = %v, want %s", binding["runtime_source_uri"], sourceURI)
	}
	if binding["binding_status"] != string(state.BindingResumable) {
		t.Fatalf("binding_status = %v, want %s", binding["binding_status"], state.BindingResumable)
	}
	capability, _ := binding["resume_capability"].(map[string]any)
	if capability["strategy"] != "codex_resume" {
		t.Fatalf("resume_capability = %v, want codex_resume strategy", capability)
	}
}

func mustJSONStr(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(b)
}
