package cli

import (
	"fmt"
	"net/http"
	"sort"
)

const retentionUsage = "retention [show|sweep] [--dry-run] [--vacuum] [--json]"

// runRetention shows the retention policy and what the next sweep would
// change, or runs a sweep now (rfcs/retention.md).
func runRetention(argv []string) int {
	c, err := newClient()
	if err != nil {
		return fatal(err)
	}
	return retentionCommand(c, argv)
}

func retentionCommand(c *client, argv []string) int {
	if len(argv) == 0 {
		argv = []string{"show"}
	}
	a := parse(argv[1:], "dry-run", "vacuum", "json")
	switch argv[0] {
	case "show":
		result, err := c.request(http.MethodGet, "/retention", nil)
		if err != nil {
			return fatal(err)
		}
		if a.bool("json") {
			printJSON(result)
			return 0
		}
		policy, _ := result["policy"].(map[string]any)
		fmt.Printf("content: %s\nrecords: %s\n", retentionDays(policy["content_days"]), retentionDays(policy["records_days"]))
		if at := stringValue(policy, "last_sweep_at"); at != "" {
			fmt.Println("last sweep:", at)
		} else {
			fmt.Println("last sweep: none since the daemon started")
		}
		printSweepCounts("next sweep", result["next_sweep"])
		return 0
	case "sweep":
		result, err := c.request(http.MethodPost, "/retention/sweep", map[string]any{"dry_run": a.bool("dry-run"), "vacuum": a.bool("vacuum")})
		if err != nil {
			return fatal(err)
		}
		if a.bool("json") {
			printJSON(result)
			return 0
		}
		label := "swept"
		if a.bool("dry-run") {
			label = "would sweep"
		}
		printSweepCounts(label, result["counts"])
		return 0
	}
	return usage(retentionUsage)
}

func retentionDays(v any) string {
	n, _ := v.(float64)
	if n == 0 {
		return "kept forever"
	}
	return fmt.Sprintf("%g days", n)
}

func printSweepCounts(label string, raw any) {
	counts, _ := raw.(map[string]any)
	if len(counts) == 0 {
		fmt.Printf("%s: nothing\n", label)
		return
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("%s:\n", label)
	for _, k := range keys {
		fmt.Printf("  %-40s %v\n", k, counts[k])
	}
}
