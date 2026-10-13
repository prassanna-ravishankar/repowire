package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// runInbox reads and answers the human inbox from the terminal. Read and
// replied state is shared with every other surface (Telegram, dashboard).
func runInbox(argv []string) int {
	if len(argv) == 0 {
		argv = []string{"list"}
	}
	a := parse(argv[1:], "all", "json")
	c, err := newClient()
	if err != nil {
		return fatal(err)
	}
	switch argv[0] {
	case "list":
		q := url.Values{}
		if !a.bool("all") {
			q.Set("unread", "1")
		}
		addQuery(q, "limit", a.string("limit", ""))
		result, err := c.request(http.MethodGet, pathQuery("/inbox", q), nil)
		if err != nil {
			return fatal(err)
		}
		if a.bool("json") {
			printJSON(result)
			return 0
		}
		items := anySlice(result["items"])
		if len(items) == 0 {
			fmt.Println("inbox empty")
			return 0
		}
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			fmt.Println(inboxLine(item))
		}
		return 0
	case "show":
		if len(a.pos) < 1 {
			return usage("inbox show ITEM_ID [--json]")
		}
		item, err := c.request(http.MethodGet, "/inbox/"+url.PathEscape(a.pos[0]), nil)
		if err != nil {
			return fatal(err)
		}
		if a.bool("json") {
			printJSON(item)
			return 0
		}
		fmt.Println(inboxLine(item))
		fmt.Println(stringValue(item, "text"))
		for _, raw := range anySlice(item["pushes"]) {
			push, _ := raw.(map[string]any)
			fmt.Printf("  %s: %s %s\n", stringValue(push, "surface"), stringValue(push, "status"), stringValue(push, "error_class"))
		}
		return 0
	case "read":
		if len(a.pos) < 1 {
			return usage("inbox read ITEM_ID")
		}
		if _, err := c.request(http.MethodPost, "/inbox/"+url.PathEscape(a.pos[0])+"/read", map[string]any{"surface": "cli"}); err != nil {
			return fatal(err)
		}
		fmt.Println("read", a.pos[0])
		return 0
	case "reply":
		if len(a.pos) < 2 {
			return usage("inbox reply ITEM_ID TEXT")
		}
		result, err := c.request(http.MethodPost, "/inbox/"+url.PathEscape(a.pos[0])+"/reply",
			map[string]any{"surface": "cli", "text": strings.Join(a.pos[1:], " ")})
		if err != nil {
			return fatal(err)
		}
		fmt.Printf("%s %s\n", stringValue(result, "action"), stringValue(result, "correlation_id"))
		return 0
	}
	return usage("inbox [list|show|read|reply]")
}

func inboxLine(item map[string]any) string {
	mark := "•"
	if item["replied"] == true {
		mark = "↩"
	} else if item["read"] == true {
		mark = " "
	}
	text := strings.ReplaceAll(stringValue(item, "text"), "\n", " ")
	if len([]rune(text)) > 80 {
		text = string([]rune(text)[:79]) + "…"
	}
	return fmt.Sprintf("%s %s  %-9s @%s  %s", mark, stringValue(item, "id"), stringValue(item, "kind"), stringValue(item, "from"), text)
}
