package hub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// codexHome points CODEX_HOME at a temp dir, with the hooks transport
// installed when hooks is true, so the test never reads the real ~/.codex.
func codexHome(t *testing.T, hooks bool) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	command := "hook stop --backend=codex --reminders-only"
	if hooks {
		command = "hook session --backend=codex"
	}
	body := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"repowire ` + command + `"}]}]}}`
	if err := os.WriteFile(filepath.Join(root, "hooks.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A Codex spawn whose launch runs Codex's embedded App Server is refused
// before any pane exists: the bridge could never deliver to it (#567).
func TestSpawnRefusesEmbeddedCodexLaunch(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	codexHome(t, false)
	root := t.TempDir()
	tmux := &fakeTmux{spawnResult: service.SpawnResult{DisplayName: "worker", TmuxSession: "mesh:work", PaneID: "%13"}}
	own := service.NewFileOwnership("test-host", tmux.ProbePane)
	svc := newHermeticSpawnService(t, tmux, own, map[proto.AgentType]string{proto.AgentCodex: "codex --no-daemon"}, []string{root})
	h := &Hub{}
	h.WithSpawn(svc, &fakeSpawnRegistry{}, service.NewAskTracker(0), "test-host", proto.CircleBoundarySession)
	mux := http.NewServeMux()
	h.registerSpawnRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	backend := proto.AgentCodex
	resp := postSpawnJSON(t, srv, "/spawn", SpawnRequest{Path: root, Backend: &backend, Circle: "mesh"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "codex_embedded_app_server") {
		t.Fatalf("embedded codex spawn: want 422 codex_embedded_app_server, got %d %s", resp.StatusCode, body)
	}
	if tmux.spawnConfig.Command != "" {
		t.Fatalf("refused spawn reached tmux: %+v", tmux.spawnConfig)
	}
}

// Doctor names why a Codex peer the bridge did not register cannot receive,
// and stays quiet when the hooks transport reaches it instead.
func TestDoctorExplainsCodexPeerOffAppServer(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	path, pane := t.TempDir(), "%81"
	tmux := &fakeTmux{evidence: map[string]*service.TmuxPaneEvidence{pane: {PaneID: pane, TmuxSession: "mesh:work", CurrentPath: path}}}
	h := newTestHub(t)
	h.WithSpawn(service.NewSpawnService(tmux, service.NewFileOwnership("test-host", tmux.ProbePane), nil, nil), nil, nil, "test-host", proto.CircleBoundarySession)
	mux := http.NewServeMux()
	h.Routes(mux)
	circle := "mesh"
	rec := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{Name: "worker", Path: &path, PaneID: &pane, Backend: proto.AgentCodex, Circle: &circle})
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	var registered RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	codes := func() []string {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/peers/"+registered.PeerID+"/doctor", nil))
		var report struct {
			Contradictions []map[string]string `json:"contradictions"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatalf("doctor: %d %s", rec.Code, rec.Body.String())
		}
		var out []string
		for _, c := range report.Contradictions {
			out = append(out, c["code"])
		}
		return out
	}
	has := func(codes []string, want string) bool {
		for _, c := range codes {
			if c == want {
				return true
			}
		}
		return false
	}

	codexHome(t, false)
	if got := codes(); !has(got, "CODEX_NOT_ON_APP_SERVER") {
		t.Fatalf("App Server transport: contradictions = %v", got)
	}
	codexHome(t, true)
	if got := codes(); has(got, "CODEX_NOT_ON_APP_SERVER") {
		t.Fatalf("hooks transport: contradictions = %v", got)
	}
}
