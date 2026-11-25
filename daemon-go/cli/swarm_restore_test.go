package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/internal/tmuxtest"
)

// fakeRestoreDaemon answers /swarm/plan with plan and records what restore
// tells it, in order.
type fakeRestoreDaemon struct {
	mu     sync.Mutex
	plan   restorePlan
	events []string
	placed []placedPane
}

func (f *fakeRestoreDaemon) serve(t *testing.T) *client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/swarm/plan":
			_ = json.NewEncoder(w).Encode(f.plan)
		case "/swarm/placed":
			var pl placedPane
			_ = json.NewDecoder(r.Body).Decode(&pl)
			f.placed = append(f.placed, pl)
			f.events = append(f.events, "placed "+pl.PeerID)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/swarm/verify":
			var body struct {
				Placed []placedPane `json:"placed"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var agents []map[string]any
			for _, p := range body.Placed {
				f.events = append(f.events, "verify "+p.PeerID)
				agents = append(agents, map[string]any{"pane_id": p.PaneID, "peer_id": p.PeerID, "outcome": "back"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": agents})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, http: srv.Client()}
}

func writeManifest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "swarm.json")
	if err := os.WriteFile(path, []byte(`{"schema":1,"panes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var fastRestore = restoreOptions{apply: true, register: 2 * time.Second, poll: 20 * time.Millisecond}

// Restore never picks a tmux server by itself: with no --socket it refuses,
// and a --socket that is the default server (where the user's agents run)
// needs --allow-default-server. Neither touches tmux.
func TestSwarmRestoreRefusesTheDefaultServer(t *testing.T) {
	if _, err := restoreSocket(restoreOptions{apply: true}); err == nil {
		t.Fatal("restore chose a server without --socket or --allow-default-server")
	}
	def := defaultTmuxSocket()
	if _, err := restoreSocket(restoreOptions{apply: true, socket: def}); err == nil || !strings.Contains(err.Error(), "--allow-default-server") {
		t.Fatalf("--socket naming the default server: %v", err)
	}
	if got, err := restoreSocket(restoreOptions{apply: true, socket: def, allowDefault: true}); err != nil || got != def {
		t.Fatalf("explicitly allowed default server: %q %v", got, err)
	}
	f := &fakeRestoreDaemon{plan: restorePlan{Schema: 1, PlanID: "p1", Panes: []restorePane{{Session: "work", Action: "shell", CWD: "/tmp"}}}}
	if code := runRestore(f.serve(t), writeManifest(t), restoreOptions{apply: true, register: time.Second, poll: time.Millisecond}); code == 0 {
		t.Fatal("applied with no server named")
	}
	if _, err := os.Stat(def); err == nil {
		t.Fatal("the refused default server was started")
	}
}

// Without --apply, restore only prints the plan: no server is started.
func TestSwarmRestoreDryRunCreatesNothing(t *testing.T) {
	srv := tmuxtest.New(t, tmuxtest.FollowEnv())
	f := &fakeRestoreDaemon{plan: restorePlan{Schema: 1, PlanID: "p1", Panes: []restorePane{{Session: "work", Action: "shell", CWD: "/tmp"}}}}
	if code := runRestore(f.serve(t), writeManifest(t), restoreOptions{socket: srv.Socket}); code != 0 {
		t.Fatalf("dry run exit %d", code)
	}
	if _, err := os.Stat(srv.Socket); err == nil {
		t.Fatal("a dry run started the tmux server")
	}
}

// --apply rebuilds the plan on the named server: each window at its recorded
// index and name, each pane in its cwd, its layout applied, agents started
// in their panes running their command, a second agent at a path started
// only after the first registered, and each launch reported as placed.
func TestSwarmRestoreAppliesThePlanInASandbox(t *testing.T) {
	srv := tmuxtest.New(t, tmuxtest.FollowEnv())
	dir := t.TempDir()
	agents, notes := filepath.Join(dir, "agents"), filepath.Join(dir, "notes")
	for _, d := range []string{agents, notes} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A real three-pane layout from this server, so applying it is exact.
	srv.Start("-s", "probe", "sleep 60")
	for i := 0; i < 2; i++ {
		if _, err := srv.Run("split-window", "-d", "-h", "-t", "probe:", "sleep 60"); err != nil {
			t.Fatal(err)
		}
	}
	layout, err := srv.Run("display-message", "-p", "-t", "probe:", "#{window_layout}")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRestoreDaemon{plan: restorePlan{Schema: 1, PlanID: "plan-1", Panes: []restorePane{
		{Session: "work", WindowIndex: 3, WindowName: "agents", WindowLayout: layout, PaneIndex: 0, Action: "launch", CWD: agents, PeerID: "repow-a", Command: "sleep 300"},
		{Session: "work", WindowIndex: 3, WindowName: "agents", WindowLayout: layout, PaneIndex: 1, Action: "launch", CWD: agents, PeerID: "repow-b", Command: "sleep 301"},
		{Session: "work", WindowIndex: 3, WindowName: "agents", WindowLayout: layout, PaneIndex: 2, Action: "shell", CWD: notes, PeerID: "repow-c", Skip: "online now"},
		{Session: "work", WindowIndex: 7, WindowName: "tail", PaneIndex: 0, Action: "shell", CWD: notes},
	}}}
	opts := fastRestore
	opts.socket = srv.Socket
	if code := runRestore(f.serve(t), writeManifest(t), opts); code != 0 {
		t.Fatalf("apply exit %d", code)
	}

	// A fresh pane's process may not have exec'd its command yet (on Linux
	// it reads "tmux" for a moment): wait, bounded, for both agents to run.
	var out string
	var lines []string
	for deadline := time.Now().Add(5 * time.Second); ; {
		var err error
		if out, err = srv.Run("list-panes", "-s", "-t", "work", "-F", "#{window_index}\t#{window_name}\t#{pane_index}\t#{pane_current_command}\t#{pane_id}"); err != nil {
			t.Fatal(err)
		}
		lines = strings.Split(out, "\n")
		if len(lines) == 4 && strings.Contains(lines[0], "\tsleep\t") && strings.Contains(lines[1], "\tsleep\t") || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(lines) != 4 {
		t.Fatalf("panes:\n%s", out)
	}
	for i, want := range []string{"3\tagents\t0\tsleep", "3\tagents\t1\tsleep", "3\tagents\t2\t", "7\ttail\t0\t"} {
		if !strings.HasPrefix(lines[i], want) {
			t.Errorf("pane %d = %q, want prefix %q", i, lines[i], want)
		}
	}
	for i, want := range map[int]string{2: notes} {
		target := "work:3." + string(rune('0'+i))
		deadline := time.Now().Add(5 * time.Second)
		var cwd string
		for time.Now().Before(deadline) {
			if cwd, _ = srv.Run("display-message", "-p", "-t", target, "#{pane_current_path}"); realPath(cwd) == realPath(want) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if realPath(cwd) != realPath(want) {
			t.Errorf("%s cwd %q, want %q", target, cwd, want)
		}
	}
	if got, _ := srv.Run("display-message", "-p", "-t", "work:3", "#{window_layout}"); layoutGeometry(got) != layoutGeometry(layout) {
		t.Errorf("layout %q, want the geometry of %q", got, layout)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.placed) != 2 || f.placed[0].PeerID != "repow-a" || f.placed[1].PeerID != "repow-b" || f.placed[0].PlanID != "plan-1" ||
		!strings.Contains(out, f.placed[0].PaneID) || f.placed[0].TmuxSession != "work" {
		t.Fatalf("placed = %+v", f.placed)
	}
	// The second agent at the path started only after the first registered.
	first := strings.Join(f.events, ",")
	if !strings.HasPrefix(first, "placed repow-a,verify repow-a,placed repow-b") {
		t.Fatalf("events = %s", first)
	}
}

// Restore is create-only: when a session it would create already exists,
// nothing is created and the existing session is left as it was.
func TestSwarmRestoreRefusesAnExistingSession(t *testing.T) {
	srv := tmuxtest.New(t, tmuxtest.FollowEnv())
	srv.Start("-s", "work", "sleep 60")
	before, _ := srv.Run("list-panes", "-a", "-F", "#{pane_id}")
	f := &fakeRestoreDaemon{plan: restorePlan{Schema: 1, PlanID: "plan-1", Panes: []restorePane{
		{Session: "fresh", WindowIndex: 0, WindowName: "a", Action: "shell", CWD: "/tmp"},
		{Session: "work", WindowIndex: 4, WindowName: "agents", Action: "launch", CWD: "/tmp", PeerID: "repow-a", Command: "sleep 300"},
	}}}
	opts := fastRestore
	opts.socket = srv.Socket
	if code := runRestore(f.serve(t), writeManifest(t), opts); code == 0 {
		t.Fatal("restored into an existing session")
	}
	after, _ := srv.Run("list-panes", "-a", "-F", "#{pane_id}")
	if after != before {
		t.Fatalf("panes changed:\nbefore %q\nafter  %q", before, after)
	}
	if sessions, _ := srv.Run("list-sessions", "-F", "#{session_name}"); sessions != "work" {
		t.Fatalf("sessions = %q, want only the existing one", sessions)
	}
	if len(f.placed) != 0 {
		t.Fatalf("placed = %+v", f.placed)
	}
}

// layoutGeometry is a window layout without its checksum and pane ids, which
// differ between servers: only the cells' sizes and offsets.
func layoutGeometry(layout string) string {
	if _, rest, ok := strings.Cut(layout, ","); ok {
		layout = rest
	}
	return regexp.MustCompile(`(\d+x\d+,\d+,\d+),\d+`).ReplaceAllString(layout, "$1")
}
