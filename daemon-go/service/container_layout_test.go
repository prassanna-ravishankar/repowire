package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/internal/tmuxtest"
)

// Describe reads the sandbox server's geometry: sessions, window indices and
// names, the window layout tmux would reapply, pane order and each pane's cwd.
func TestTmuxDescribeReadsTheSandboxLayout(t *testing.T) {
	srv := tmuxtest.New(t)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	srv.Start("-s", "work", "-n", "agents", "-c", a, "sleep 60")
	if _, err := srv.Run("split-window", "-d", "-t", "work:agents", "-c", b, "sleep 60"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Run("new-window", "-d", "-t", "work:5", "-n", "shell", "-c", b, "sleep 60"); err != nil {
		t.Fatal(err)
	}
	layout, err := srv.Run("display-message", "-p", "-t", "work:agents", "#{window_layout}")
	if err != nil {
		t.Fatal(err)
	}

	real := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
	// pane_current_path is read from the pane's process, which may not have
	// entered its -c directory yet right after the pane is created (seen on
	// Linux CI): wait, bounded, until every pane reports where it started.
	var got []PaneGeometry
	settled := func() bool {
		var err error
		if got, err = (tmuxContainer{}).Describe(); err != nil {
			t.Fatal(err)
		}
		return len(got) == 3 && real(got[0].CWD) == real(a) && real(got[1].CWD) == real(b) && real(got[2].CWD) == real(b)
	}
	for deadline := time.Now().Add(5 * time.Second); !settled() && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if len(got) != 3 {
		t.Fatalf("panes = %+v", got)
	}
	want := []PaneGeometry{
		{Session: "work", WindowName: "agents", WindowLayout: layout, PaneIndex: 0, CWD: real(a)},
		{Session: "work", WindowName: "agents", WindowLayout: layout, PaneIndex: 1, CWD: real(b)},
		{Session: "work", WindowIndex: 5, WindowName: "shell", PaneIndex: 0, CWD: real(b)},
	}
	for i, w := range want {
		g := got[i]
		if g.PaneID == "" || g.Session != w.Session || g.WindowName != w.WindowName || g.PaneIndex != w.PaneIndex || real(g.CWD) != w.CWD {
			t.Fatalf("pane %d = %+v, want %+v", i, g, w)
		}
		if w.WindowLayout != "" && g.WindowLayout != w.WindowLayout {
			t.Fatalf("pane %d layout %q, want %q", i, g.WindowLayout, w.WindowLayout)
		}
	}
	if got[2].WindowIndex != 5 {
		t.Fatalf("explicit window index lost: %+v", got[2])
	}
}

func TestParsePaneGeometryRefusesMalformedLines(t *testing.T) {
	for _, out := range []string{"work\t0\tagents\tlayout\t0\t%1", "work\tx\tagents\tlayout\t0\t%1\t/p", "\t0\tw\tl\t0\t%1\t/p"} {
		if _, err := parsePaneGeometry(out); err == nil {
			t.Fatalf("accepted %q", out)
		}
	}
	got, err := parsePaneGeometry("work\t1\tmy window\tbb62,80x24,0,0,1\t0\t%3\t/p with space\n")
	if err != nil || len(got) != 1 || got[0].WindowName != "my window" || got[0].CWD != "/p with space" || got[0].PaneID != "%3" {
		t.Fatalf("got %+v %v", got, err)
	}
}
