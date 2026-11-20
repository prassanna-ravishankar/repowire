package hooks

import (
	"os"
	"testing"
)

// #536: pane metadata records the agent's start time with its pid.
func TestWriteMetadataRecordsAgentStartTime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pid := os.Getpid()
	if err := writeMetadata("%901", map[string]any{"peer_id": "repow-1-z", "agent_pid": pid}); err != nil {
		t.Fatal(err)
	}
	want, ok := ProcessStartTime(pid)
	if got := ReadPaneRuntimeMetadata("%901")["agent_started"]; !ok || got != want {
		t.Fatalf("agent_started = %v, want %q", got, want)
	}
}

// The recorded start time must not depend on the caller's locale or zone.
func TestProcessStartTimeIgnoresLocale(t *testing.T) {
	pid := os.Getpid()
	t.Setenv("LC_ALL", "")
	t.Setenv("TZ", "UTC")
	base, ok := ProcessStartTime(pid)
	if !ok {
		t.Fatal("no start time for the test process")
	}
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("LC_TIME", "de_DE.UTF-8")
	t.Setenv("TZ", "Asia/Kolkata")
	if other, _ := ProcessStartTime(pid); other != base {
		t.Fatalf("start time changed with locale/zone: %q vs %q", other, base)
	}
}
