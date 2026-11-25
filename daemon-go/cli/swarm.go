package cli

import (
	"encoding/json"
	"errors"
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
	a := parse(argv[1:], "force", "apply", "allow-default-server")
	switch argv[0] {
	case "restore":
		return swarmRestore(c, a)
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
	return usage("swarm snapshot [-o FILE] [--force] | swarm restore FILE [--apply] [--socket PATH | --allow-default-server]")
}

// writeSnapshot writes the manifest owner-only (it names paths and session
// ids). Without force it refuses an existing file, atomically: the finished
// temp file is hard-linked into place, which fails if the name exists.
func writeSnapshot(path string, manifest map[string]any, force bool) error {
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
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
	if force {
		return os.Rename(tmp.Name(), path)
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s exists; pass --force to replace it", path)
		}
		return err
	}
	return nil
}
