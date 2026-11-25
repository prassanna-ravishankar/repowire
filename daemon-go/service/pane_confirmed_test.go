package service

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/repowire/repowire/daemon-go/hooks"
)

func writePaneMeta(t *testing.T, pane string, meta map[string]any) {
	t.Helper()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooks.WSHookMetaPath(pane), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPaneConfirmedPeerNeedsTheSameProcess(t *testing.T) {
	t.Setenv("REPOWIRE_CACHE_DIR", t.TempDir())
	pid := os.Getpid()
	started, ok := hooks.ProcessStartTime(pid)
	if !ok {
		t.Skip("ps cannot read this process's start time")
	}
	cases := []struct {
		name string
		meta map[string]any
		want string
	}{
		{"same pid and start time", map[string]any{"peer_id": "repow-1-latest", "agent_pid": pid, "agent_started": started}, "repow-1-latest"},
		{"metadata from before start times were recorded", map[string]any{"peer_id": "repow-1-latest", "agent_pid": pid}, "repow-1-latest"},
		{"a recycled pid", map[string]any{"peer_id": "repow-1-latest", "agent_pid": pid, "agent_started": "Thu Jan  1 00:00:00 1970"}, ""},
		{"another process", map[string]any{"peer_id": "repow-1-latest", "agent_pid": pid + 1}, ""},
		{"no identity recorded", map[string]any{"agent_pid": pid, "agent_started": started}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writePaneMeta(t, "%29", tc.meta)
			got, ok := PaneConfirmedPeer("%29", pid)
			if string(got) != tc.want || ok != (tc.want != "") {
				t.Fatalf("PaneConfirmedPeer = %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
}
