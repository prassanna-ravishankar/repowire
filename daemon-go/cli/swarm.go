package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// runSwarm is `repowire swarm` (#505, experiments.swarm_snapshot). Snapshot
// records the tmux layout and the agents in it, read-only, so a later restore
// can rebuild them.
func runSwarm(argv []string) int {
	if len(argv) == 0 {
		return usage("swarm snapshot [-o FILE] [--force]")
	}
	c, err := newClient()
	if err != nil {
		return fatal(err)
	}
	return swarmCommand(c, argv)
}

func swarmCommand(c *client, argv []string) int {
	a := parse(argv[1:], "force")
	switch argv[0] {
	case "snapshot":
		result, err := c.request(http.MethodGet, "/swarm/snapshot", nil)
		if err != nil {
			return fatal(err)
		}
		out := a.string("o", a.string("out", ""))
		if out == "true" { // -o with no file
			return usage("swarm snapshot [-o FILE] [--force]")
		}
		if out == "" {
			printJSON(result)
			return 0
		}
		if err := writeSnapshot(out, result, a.bool("force")); err != nil {
			return fatal(err)
		}
		panes, _ := result["panes"].([]any)
		fmt.Printf("wrote %s (%d panes)\n", out, len(panes))
		return 0
	}
	return usage("swarm snapshot [-o FILE] [--force]")
}

// writeSnapshot writes the manifest owner-only (it names paths and session
// ids), refusing to replace an existing file unless force.
func writeSnapshot(path string, manifest map[string]any, force bool) error {
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s exists; pass --force to replace it", path)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".swarm-snapshot-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
