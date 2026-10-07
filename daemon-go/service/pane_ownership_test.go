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

// A record written before the live tmux server started belongs to a recycled
// pane id. Same session name, different path: without the server-start check
// this is the false-positive pane_identity_mismatch that blocked registration.
func TestValidateBootstrapForgetsRecordOlderThanTmuxServer(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	serverStart := 1790683206.0 // tmux server came up
	ownership := NewFileOwnership("host", func(string) *TmuxPaneEvidence {
		return &TmuxPaneEvidence{
			PaneID: "%76", SessionName: "1", TmuxSession: "1:turns", CurrentPath: "/work/turn-counting",
			ServerStart: serverStart,
		}
	})
	ownership.Record(OwnershipRecord{
		PaneID: "%76", Path: "/work/gen-ui-databook", Backend: "claude-code", Circle: "1", Role: "agent",
		TmuxSession: "1:gen-ui-databook", Machine: "host", CreatedAt: serverStart - 8*24*3600,
	})

	got := ownership.ValidateBootstrap("%76")
	if !got.OK || got.Record != nil || got.Evidence == nil {
		t.Fatalf("pre-server record bootstrap = %#v, want live evidence without stale ownership", got)
	}
	if _, exists := ownership.loadLocked()["%76"]; exists {
		t.Fatal("record older than the tmux server was not forgotten")
	}

	// Same shape but written after the server started: still a real same-session
	// mismatch and still fail-loud.
	ownership.Record(OwnershipRecord{
		PaneID: "%76", Path: "/work/gen-ui-databook", Backend: "claude-code", Circle: "1", Role: "agent",
		TmuxSession: "1:gen-ui-databook", Machine: "host", CreatedAt: serverStart + 60,
	})
	if got := ownership.ValidateBootstrap("%76"); got.OK || got.Error != "pane_identity_mismatch" {
		t.Fatalf("post-server mismatch = %#v, want pane_identity_mismatch", got)
	}
}

func TestValidateBootstrapDoesNotCompareForeignMachineTimestamps(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	ownership := NewFileOwnership("local", func(string) *TmuxPaneEvidence {
		return &TmuxPaneEvidence{
			PaneID: "%76", SessionName: "1", TmuxSession: "1:turns", CurrentPath: "/work/turn-counting",
			ServerStart: 200,
		}
	})
	ownership.Record(OwnershipRecord{
		PaneID: "%76", Path: "/work/gen-ui-databook", TmuxSession: "1:gen-ui-databook",
		Machine: "remote", CreatedAt: 100,
	})

	got := ownership.ValidateBootstrap("%76")
	if got.Error != "ownership_machine_mismatch" {
		t.Fatalf("foreign record bootstrap = %#v, want ownership_machine_mismatch", got)
	}
	if _, exists := ownership.loadLocked()["%76"]; !exists {
		t.Fatal("foreign-machine ownership record was deleted using an incomparable timestamp")
	}
}
