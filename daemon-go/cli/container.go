package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

const containerUsage = "container report [--since 7d|RFC3339] [--db PATH] [--json]"

// runContainer is the agent container's CLI (rfcs/agent-container.md).
func runContainer(argv []string) int {
	if len(argv) == 0 || argv[0] != "report" {
		return usage(containerUsage)
	}
	a := parse(argv[1:], "json")
	since, err := parseSince(a.string("since", "7d"), time.Now())
	if err != nil {
		return fatal(fmt.Errorf("container report: %w", err))
	}
	// Read-only, so it is safe beside a running daemon.
	store, err := state.OpenReadOnly(a.string("db", state.DefaultPath()))
	if err != nil {
		return fatal(err)
	}
	defer store.Close()
	rows, err := store.EventsOfTypes(context.Background(), service.EventAgentExitObserved, service.EventExitSignalChecked)
	if err != nil {
		return fatal(err)
	}
	report := service.SummarizeExits(rows, since)
	if a.bool("json") {
		printJSON(report)
	} else {
		report.Write(os.Stdout)
	}
	return 0
}

// parseSince reads "all", a number of days ("7d"), a Go duration ("36h") or
// an RFC-3339 time.
func parseSince(raw string, now time.Time) (time.Time, error) {
	if raw == "" || raw == "all" {
		return time.Time{}, nil
	}
	var days float64
	if _, err := fmt.Sscanf(raw, "%gd", &days); err == nil && days > 0 && raw[len(raw)-1] == 'd' {
		return now.Add(-time.Duration(days * float64(24*time.Hour))), nil
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since must be all, 7d, 36h or an RFC-3339 time")
}
