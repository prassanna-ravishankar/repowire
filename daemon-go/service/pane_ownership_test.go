package service

import "testing"

func TestValidateBootstrapForgetsRecordFromReusedPaneID(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	path := t.TempDir()
	ownership := NewFileOwnership("host", func(string) *TmuxPaneEvidence {
		return &TmuxPaneEvidence{
			PaneID: "%26", SessionName: "current", TmuxSession: "current:agent-dj", CurrentPath: path,
		}
	})
	ownership.Record(OwnershipRecord{
		PaneID: "%26", Path: "/old/project", Backend: "codex", Circle: "old", Role: "agent",
		TmuxSession: "old:repowire", Machine: "host",
	})

	got := ownership.ValidateBootstrap("%26")
	if !got.OK || got.Record != nil || got.Evidence == nil {
		t.Fatalf("reused-pane bootstrap = %#v, want live evidence without stale ownership", got)
	}
	if _, exists := ownership.loadLocked()["%26"]; exists {
		t.Fatal("stale ownership record was not forgotten")
	}
}

func TestValidateBootstrapRejectsMismatchWithinSameSession(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	ownership := NewFileOwnership("host", func(string) *TmuxPaneEvidence {
		return &TmuxPaneEvidence{
			PaneID: "%26", SessionName: "mesh", TmuxSession: "mesh:renamed", CurrentPath: "/other/project",
		}
	})
	ownership.Record(OwnershipRecord{
		PaneID: "%26", Path: "/expected/project", Backend: "claude-code", Circle: "mesh", Role: "agent",
		TmuxSession: "mesh:original", Machine: "host",
	})

	got := ownership.ValidateBootstrap("%26")
	if got.OK || got.Error != "pane_identity_mismatch" {
		t.Fatalf("same-session mismatch = %#v, want pane_identity_mismatch", got)
	}
	if _, exists := ownership.loadLocked()["%26"]; !exists {
		t.Fatal("same-session ownership record must remain for fail-loud reconciliation")
	}
}
