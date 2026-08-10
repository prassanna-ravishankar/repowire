package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

func TestWindowSplitArgs(t *testing.T) {
	got := windowSplitArgs("%42", "/work/project")
	want := []string{"split-window", "-P", "-F", "#{pane_id}", "-t", "%42", "-c", "/work/project"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("split-window args = %#v, want %#v", got, want)
	}
}

type placementTmux struct{ evidence map[string]*TmuxPaneEvidence }

func (t placementTmux) Spawn(SpawnConfig) (SpawnResult, error) { return SpawnResult{}, nil }
func (t placementTmux) KillPane(string) bool                   { return true }
func (t placementTmux) ProbePane(target string) *TmuxPaneEvidence {
	return t.evidence[target]
}

func TestPrepareSpawnDerivesAndValidatesWindowTarget(t *testing.T) {
	tmux := placementTmux{evidence: map[string]*TmuxPaneEvidence{
		"%42": {PaneID: "%42", SessionName: "mesh", WindowID: "@7", WindowPanes: 2},
		"@7":  {PaneID: "%42", SessionName: "mesh", WindowID: "@7", WindowPanes: 2},
	}}
	svc := NewSpawnService(tmux, nil, nil, nil).WithCircleBoundary(proto.CircleBoundaryWindow)
	cfg, err := svc.PrepareSpawn(SpawnConfig{Circle: "window-7"})
	if err != nil {
		t.Fatalf("PrepareSpawn: %v", err)
	}
	if cfg.CircleBoundary != proto.CircleBoundaryWindow || cfg.TargetPane != "@7" {
		t.Fatalf("prepared config = %+v, want window boundary target @7", cfg)
	}
	if _, err := svc.PrepareReplacement(SpawnConfig{Circle: "window-7"}); err != nil {
		t.Fatalf("two-pane replacement target rejected: %v", err)
	}
	replacement, err := svc.PrepareReplacement(SpawnConfig{Circle: "window-7", TargetPane: "%42"})
	if err != nil || replacement.TargetPane != "@7" {
		t.Fatalf("replacement target = %+v, err=%v; want stable @7", replacement, err)
	}
	tmux.evidence["@7"].WindowPanes = 1
	_, err = svc.PrepareReplacement(SpawnConfig{Circle: "window-7"})
	var spawnErr *SpawnError
	if !errors.As(err, &spawnErr) || spawnErr.Status != 409 {
		t.Fatalf("last-pane replacement = %v, want 409 SpawnError", err)
	}

	_, err = svc.PrepareSpawn(SpawnConfig{Circle: "window-8"})
	if !errors.As(err, &spawnErr) || spawnErr.Status != 409 {
		t.Fatalf("missing live window target = %v, want 409 SpawnError", err)
	}
}

func TestPrepareSpawnLeavesSessionPlacementUnchanged(t *testing.T) {
	svc := NewSpawnService(nil, nil, nil, nil)
	cfg, err := svc.PrepareSpawn(SpawnConfig{Circle: "mesh"})
	if err != nil || cfg.CircleBoundary != proto.CircleBoundarySession || cfg.TargetPane != "" {
		t.Fatalf("session config = %+v, err=%v", cfg, err)
	}
}

func TestOwnershipUpdatesTmuxSessionAfterRename(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	ownership := NewFileOwnership("test", nil)
	ownership.Record(OwnershipRecord{PaneID: "%7", Path: t.TempDir(), Backend: "codex", Circle: "window-1", Role: "agent", TmuxSession: "old:window"})
	ownership.UpdatePlacement("%7", "new:renamed", "new")
	record, ok := ownership.loadLocked()["%7"]
	if !ok || record.TmuxSession != "new:renamed" || record.Circle != "new" {
		t.Fatalf("ownership record = %#v, want updated tmux session", record)
	}
}

func TestResolveCommandUsesConfiguredPath(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "custom-agent")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	svc := NewSpawnService(nil, nil, map[proto.AgentType]string{proto.AgentCodex: "custom-agent"}, []string{dir}).WithRuntimeConfig(nil, map[string]string{"PATH": dir})
	if _, err := svc.ResolveCommand(proto.AgentCodex, nil); err != nil {
		t.Fatalf("configured executable should resolve: %v", err)
	}
	quotedDir := filepath.Join(t.TempDir(), "with space")
	if err := os.MkdirAll(quotedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotedCommand := filepath.Join(quotedDir, "custom agent")
	if err := os.WriteFile(quotedCommand, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	svc.commands[proto.AgentCodex] = "'" + quotedCommand + "' --flag"
	if _, err := svc.ResolveCommand(proto.AgentCodex, nil); err != nil {
		t.Fatalf("quoted executable path should resolve: %v", err)
	}
	svc.commands[proto.AgentCodex] = "missing-agent"
	_, err := svc.ResolveCommand(proto.AgentCodex, nil)
	var spawnErr *SpawnError
	if !errors.As(err, &spawnErr) || spawnErr.Status != 422 {
		t.Fatalf("missing executable: want 422 SpawnError, got %v", err)
	}
}
