package service

import (
	"fmt"
	"strconv"
	"strings"
)

// PaneGeometry is one pane as tmux lays it out: where it sits and what it
// shows, nothing about what runs in it (#505).
type PaneGeometry struct {
	Session      string `json:"session"`
	WindowIndex  int    `json:"window_index"`
	WindowName   string `json:"window_name"`
	WindowLayout string `json:"window_layout"`
	PaneIndex    int    `json:"pane_index"`
	PaneID       string `json:"pane_id"`
	CWD          string `json:"cwd"`
}

// LayoutDescriber is a Container plug that can say how its places are laid
// out. Read-only: describing never creates, changes or stops anything.
type LayoutDescriber interface {
	Describe() ([]PaneGeometry, error)
}

const paneGeometryFormat = "#{session_name}\t#{window_index}\t#{window_name}\t#{window_layout}\t#{pane_index}\t#{pane_id}\t#{pane_current_path}"

// Describe lists every pane of the server the daemon reaches, in tmux's own
// order (session, window, pane).
func (tmuxContainer) Describe() ([]PaneGeometry, error) {
	cmd, cancel := TmuxCommand("list-panes", "-a", "-F", paneGeometryFormat)
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tmux list-panes: %w", err)
	}
	return parsePaneGeometry(string(out))
}

// parsePaneGeometry reads list-panes output in paneGeometryFormat. A window
// name or path may hold anything but a tab or newline; a malformed line is an
// error, never a guess.
func parsePaneGeometry(out string) ([]PaneGeometry, error) {
	var panes []PaneGeometry
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 7 {
			return nil, fmt.Errorf("tmux list-panes: unexpected line %q", line)
		}
		window, werr := strconv.Atoi(f[1])
		pane, perr := strconv.Atoi(f[4])
		if werr != nil || perr != nil || f[0] == "" || f[5] == "" {
			return nil, fmt.Errorf("tmux list-panes: unexpected line %q", line)
		}
		panes = append(panes, PaneGeometry{Session: f[0], WindowIndex: window, WindowName: f[2], WindowLayout: f[3],
			PaneIndex: pane, PaneID: f[5], CWD: f[6]})
	}
	return panes, nil
}
