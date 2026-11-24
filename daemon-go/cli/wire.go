package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/repowire/repowire/daemon-go/service"
)

// runWire shows the wire view (rfcs/wire-observability.md): ask and notify
// threads across every circle, envelope only, through GET /wire/threads.
func runWire(argv []string) int {
	if len(argv) == 0 {
		argv = []string{"ls"}
	}
	c, err := newClient()
	if err != nil {
		return fatal(err)
	}
	return wireCommand(c, argv)
}

func wireCommand(c *client, argv []string) int {
	a := parse(argv[1:], "stuck", "json")
	switch argv[0] {
	case "ls":
		q := url.Values{}
		for _, key := range []string{"circle", "peer", "kind", "state", "since", "limit", "cursor"} {
			addQuery(q, key, a.string(key, ""))
		}
		if a.bool("stuck") {
			q.Set("stuck_only", "true")
		}
		result, err := c.request(http.MethodGet, pathQuery("/wire/threads", q), nil)
		if err != nil {
			return fatal(err)
		}
		if a.bool("json") {
			printJSON(result)
			return 0
		}
		var page service.WirePage
		if err := remarshal(result, &page); err != nil {
			return fatal(err)
		}
		fmt.Println(page.Text(time.Now()))
		return 0
	case "show":
		if len(a.pos) < 1 {
			return usage("wire show THREAD_ID [--json]")
		}
		result, err := c.request(http.MethodGet, "/wire/threads/"+url.PathEscape(a.pos[0]), nil)
		if err != nil {
			return fatal(err)
		}
		if a.bool("json") {
			printJSON(result)
			return 0
		}
		var thread service.WireThread
		if err := remarshal(result, &thread); err != nil {
			return fatal(err)
		}
		fmt.Println(thread.Text(0))
		return 0
	case "reassign", "nudge", "cancel":
		return wireAction(c, argv[0], a)
	}
	return usage("wire ls|show|reassign|nudge|cancel")
}

// wireAction runs an operator action on a thread (rfcs/wire-actions.md).
// The daemon requires its token for these, with no local exception.
func wireAction(c *client, action string, a args) int {
	body := map[string]any{}
	switch {
	case action == "reassign" && len(a.pos) == 2:
		body["to"], body["reason"] = a.pos[1], a.string("reason", "")
	case action == "cancel" && len(a.pos) == 1:
		body["reason"] = a.string("reason", "")
	case action == "nudge" && len(a.pos) == 1:
		body["note"] = a.string("note", "")
	default:
		return usage("wire reassign THREAD_ID PEER --reason R | wire cancel THREAD_ID --reason R | wire nudge THREAD_ID [--note N]")
	}
	result, err := c.request(http.MethodPost, "/wire/threads/"+url.PathEscape(a.pos[0])+"/"+action, body)
	if err != nil {
		return fatal(err)
	}
	printJSON(result)
	return 0
}

// remarshal decodes a generic JSON result into a typed value.
func remarshal(from any, to any) error {
	raw, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, to)
}
