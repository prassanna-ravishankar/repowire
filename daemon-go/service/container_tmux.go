package service

// container_tmux.go is the tmux plug of the Container socket: everything
// that shells to tmux to place, launch, stop or probe a spawned agent.

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/repowire/repowire/daemon-go/proto"
)

// tmuxContainer is the tmux plug of the Container socket
// (rfcs/agent-container.md). It shells to the `tmux` CLI. It reproduces spawn_peer's
// get-or-create-session + unique-window-name flow without libtmux, writes the
// spawn hint before launch (so codex's late MCP boot can discover its circle),
// then starts the command directly in the newly created pane. Commands can
// contain a multi-kilobyte captured PATH, so launch uses respawn-pane's direct
// shell-command argument rather than interactive terminal input.
type tmuxContainer struct{}

// NewTmuxContainer returns the tmux plug (shells to `tmux`).
func NewTmuxContainer() Container { return tmuxContainer{} }

// Kind names the plug.
func (tmuxContainer) Kind() string { return "tmux" }

// Launch writes the spawn hint and replaces the placeholder shell of a fresh
// (or reserved) pane with the launch command.
func (c tmuxContainer) Launch(cfg SpawnConfig) (SpawnResult, error) {
	placement := cfg.Reserved
	if placement == nil {
		created, err := c.Reserve(cfg)
		if err != nil {
			return SpawnResult{}, err
		}
		placement = &created
	}

	// Drop the spawn hint BEFORE launch so a fast-registering runtime sees it.
	var rolePtr *string
	if cfg.Role != "" {
		r := string(cfg.Role)
		rolePtr = &r
	}
	writeHint(cfg.Path, string(cfg.Backend), cfg.Circle, placement.PaneID, rolePtr, cfg.PeerID, cfg.Message != nil)

	command := commandWithEnv(cfg.Command, cfg.Env)
	if err := tmuxStartCommand(placement.PaneID, command); err != nil {
		return SpawnResult{}, fmt.Errorf("tmux launch command: %w", err)
	}

	return SpawnResult{
		DisplayName: placement.DisplayName,
		TmuxSession: placement.TmuxSession,
		PaneID:      placement.PaneID,
		Message:     cfg.Message,
	}, nil
}

// Reserve creates a placeholder-shell pane according to the configured
// circle boundary (or beside TargetPane when SameWindow is set).
func (tmuxContainer) Reserve(cfg SpawnConfig) (PanePlacement, error) {
	displayName := filepath.Base(cfg.Path)
	if cfg.CircleBoundary == proto.CircleBoundaryWindow || (cfg.SameWindow && cfg.TargetPane != "") {
		paneID, err := tmuxQuery(windowSplitArgs(cfg.TargetPane, cfg.Path)...)
		if err == nil && paneID != "" {
			target, err := tmuxQuery("display-message", "-t", paneID, "-p", "#{session_name}:#{window_name}")
			if err != nil || target == "" {
				return PanePlacement{}, fmt.Errorf("tmux could not resolve window for %s: %w", paneID, err)
			}
			return PanePlacement{PaneID: paneID, TmuxSession: target, DisplayName: displayName}, nil
		}
		if cfg.CircleBoundary == proto.CircleBoundaryWindow {
			return PanePlacement{}, fmt.Errorf("tmux split-window: %w", err)
		}
	}

	paneID, err := ensureSession(cfg.Circle, cfg.Path, displayName)
	if err != nil {
		return PanePlacement{}, err
	}
	windowName := displayName
	if paneID == "" {
		windowName = uniqueWindowName(cfg.Circle, displayName)
		// Qualify the exact session; a bare numeric circle is a window index.
		paneID, err = tmuxQuery("new-window", "-P", "-F", "#{pane_id}", "-t", "="+cfg.Circle+":", "-n", windowName, "-c", cfg.Path)
		if err != nil {
			return PanePlacement{}, fmt.Errorf("tmux new-window: %w", err)
		}
	}
	target := cfg.Circle + ":" + windowName
	if paneID == "" {
		return PanePlacement{}, fmt.Errorf("tmux did not return a pane for %s", target)
	}
	return PanePlacement{PaneID: paneID, TmuxSession: target, DisplayName: windowName}, nil
}

// tmuxStartCommand replaces the fresh shell in paneID with command. The pane is
// created immediately above and its spawn hint has already been persisted, so
// killing that placeholder shell is safe and a fast runtime can still recover
// its requested identity. Passing command as respawn-pane's shell-command avoids
// interactive editor limits while retaining tmux's normal shell parsing.
func tmuxStartCommand(paneID, command string) error {
	cmd, cancel := TmuxCommand("respawn-pane", "-k", "-t", paneID, command)
	defer cancel()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("respawn-pane: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func windowSplitArgs(targetPane, path string) []string {
	return []string{"split-window", "-P", "-F", "#{pane_id}", "-t", targetPane, "-c", path}
}

// Stop wraps `tmux kill-pane -t <pane>`; true on success. Pane ids survive
// renames, so this is the preferred kill handle for daemon-spawned peers.
func (tmuxContainer) Stop(paneID string) bool {
	if paneID == "" {
		return false
	}
	cmd, cancel := TmuxCommand("kill-pane", "-t", paneID)
	defer cancel()
	return cmd.Run() == nil
}

// Probe wraps `tmux display-message` (delegates to realProbeTmuxPane).
func (tmuxContainer) Probe(paneID string) *TmuxPaneEvidence {
	return realProbeTmuxPane(paneID)
}

// ensureSession returns the first pane of a new session, or an empty string
// when the exact session already exists.
func ensureSession(session, dir, windowName string) (string, error) {
	if _, err := tmuxQuery("has-session", "-t", "="+session); err == nil {
		return "", nil
	}
	paneID, err := tmuxQuery("new-session", "-d", "-P", "-F", "#{pane_id}", "-s", session, "-c", dir, "-n", windowName)
	if err != nil {
		return "", fmt.Errorf("tmux new-session: %w", err)
	}
	return paneID, nil
}

// uniqueWindowName ports _unique_window_name: append -2, -3, ... until the name
// is free in the session. Best-effort: a listing failure returns the base name
// (tmux will still pick a working window).
func uniqueWindowName(session, base string) string {
	out, err := tmuxQuery("list-windows", "-t", "="+session, "-F", "#{window_name}")
	if err != nil {
		return base
	}
	existing := map[string]struct{}{}
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if name != "" {
			existing[name] = struct{}{}
		}
	}
	if _, taken := existing[base]; !taken {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if _, taken := existing[candidate]; !taken {
			return candidate
		}
	}
}

// realProbeTmuxPane ports probe_tmux_pane: `tmux display-message` for one pane,
// returning nil when tmux is unavailable, the pane is gone, or the fields are
// incomplete. This is the default probe seam for NewFileOwnership and the tmux plug's
// Probe.
func realProbeTmuxPane(paneID string) *TmuxPaneEvidence {
	if paneID == "" {
		return nil
	}
	cmd, cancel := TmuxCommand("display-message", "-t", paneID, "-p",
		"#{session_name}\t#{window_id}\t#{window_name}\t#{pane_current_path}\t#{pane_pid}\t#{window_panes}\t#{start_time}")
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	parts := strings.Split(strings.TrimRight(string(out), "\n"), "\t")
	if len(parts) < 6 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[4] == "" {
		return nil
	}
	serverStart := 0.0
	if len(parts) >= 7 {
		serverStart, _ = strconv.ParseFloat(parts[6], 64) // older tmux: no start_time, stays 0
	}
	windowPanes, err := strconv.Atoi(parts[5])
	if err != nil || windowPanes < 1 {
		return nil
	}
	return &TmuxPaneEvidence{
		PaneID:      paneID,
		SessionName: parts[0],
		WindowID:    parts[1],
		WindowPanes: windowPanes,
		TmuxSession: parts[0] + ":" + parts[2],
		CurrentPath: parts[3],
		PanePID:     parts[4],
		ServerStart: serverStart,
	}
}

// tmuxQuery returns trimmed stdout, preserving stderr diagnostics on failure.
func tmuxQuery(args ...string) (string, error) {
	cmd, cancel := TmuxCommand(args...)
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
