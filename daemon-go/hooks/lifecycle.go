package hooks

import (
	"os/exec"
	"strings"
)

func RunLifecycle(args []string) int {
	if len(args) == 0 {
		return 2
	}
	path := "/hooks/lifecycle/" + args[0]
	body := map[string]any{}
	switch args[0] {
	case "pane-died":
		if len(args) < 2 {
			return 2
		}
		body["pane_id"] = args[1]
	case "session-closed", "client-detached":
		if len(args) < 2 {
			return 2
		}
		body["session_name"] = args[1]
	case "session-renamed":
		if len(args) < 2 {
			return 2
		}
		body["new_name"], body["pane_ids"] = args[1], listHookPanes(true)
	case "window-renamed":
		if len(args) < 3 {
			return 2
		}
		body["new_name"], body["session_name"], body["pane_ids"] = args[1], args[2], listHookPanes(false)
	default:
		return 2
	}
	if daemonPost(path, body) == nil {
		return 1
	}
	return 0
}

func listHookPanes(session bool) []string {
	args := []string{"list-panes"}
	if session {
		args = append(args, "-s")
	}
	args = append(args, "-F", "#{pane_id}")
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return nil
	}
	var panes []string
	for _, pane := range strings.Fields(string(out)) {
		panes = append(panes, pane)
	}
	return panes
}
