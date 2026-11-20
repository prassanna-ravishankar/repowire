package hooks

import (
	"encoding/json"
	"os"
	"os/exec"
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

// Rewriting the same pid keeps its recorded start time, so a reconnect that
// still names a recycled pid cannot re-stamp it; a new pid is stamped fresh.
func TestWriteMetadataKeepsTheStartTimeOfAnUnchangedPID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pid := os.Getpid()
	if err := writeMetadata("%902", map[string]any{"agent_pid": pid}); err != nil {
		t.Fatal(err)
	}
	stale := ReadPaneRuntimeMetadata("%902")
	stale["agent_started"] = "Mon Jan  1 00:00:00 2001"
	raw, _ := json.Marshal(stale)
	if err := os.WriteFile(WSHookMetaPath("%902"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeMetadata("%902", map[string]any{"agent_pid": pid, "peer_id": "repow-1-k"}); err != nil {
		t.Fatal(err)
	}
	if got := ReadPaneRuntimeMetadata("%902")["agent_started"]; got != "Mon Jan  1 00:00:00 2001" {
		t.Fatalf("an unchanged pid was re-stamped: %v", got)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := writeMetadata("%902", map[string]any{"agent_pid": cmd.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	want, _ := ProcessStartTime(cmd.Process.Pid)
	if got := ReadPaneRuntimeMetadata("%902")["agent_started"]; got != want {
		t.Fatalf("a new pid = %v, want stamped %q", got, want)
	}
}
