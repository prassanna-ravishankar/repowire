package cli

import (
	"fmt"
	"net/http"
	"net/url"
)

// Agent peer messages and ask delegation (rfcs/message-ids-pull-delegate.md):
// `repowire peer messages|read|delivery|delegate`. Without --peer the peer is
// the one registered for this tmux pane.

func runPeerMessages(c *client, a args) int {
	peer, err := peerOrSelf(c, a)
	if err != nil {
		return fatal(err)
	}
	q := url.Values{"peer": {peer}}
	if a.bool("unread") {
		q.Set("unread", "1")
	}
	addQuery(q, "limit", a.string("limit", ""))
	result, err := c.request(http.MethodGet, pathQuery("/messages", q), nil)
	if err != nil {
		return fatal(err)
	}
	if a.bool("json") {
		printJSON(result)
		return 0
	}
	msgs := anySlice(result["messages"])
	if len(msgs) == 0 {
		fmt.Printf("no messages (delivery: %s)\n", stringValue(result, "delivery"))
		return 0
	}
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		fmt.Println(messageLine(msg))
	}
	return 0
}

func runPeerRead(c *client, a args) int {
	if len(a.pos) < 1 {
		return usage("peer read MESSAGE_ID [--peek] [--json]")
	}
	path := "/messages/" + url.PathEscape(a.pos[0])
	var msg map[string]any
	var err error
	if a.bool("peek") {
		msg, err = c.request(http.MethodGet, path, nil)
	} else {
		msg, err = c.request(http.MethodPost, path+"/read", map[string]any{})
	}
	if err != nil {
		return fatal(err)
	}
	if a.bool("json") {
		printJSON(msg)
		return 0
	}
	fmt.Println(messageLine(msg))
	fmt.Println(stringValue(msg, "text"))
	return 0
}

func runPeerDelivery(c *client, a args) int {
	if len(a.pos) < 1 || (a.pos[0] != "push" && a.pos[0] != "pull") {
		return usage("peer delivery push|pull [--peer NAME_OR_ID]")
	}
	peer, err := peerOrSelf(c, a)
	if err != nil {
		return fatal(err)
	}
	result, err := c.request(http.MethodPost, "/peers/"+url.PathEscape(peer)+"/message-delivery", map[string]any{"mode": a.pos[0]})
	if err != nil {
		return fatal(err)
	}
	fmt.Printf("%s: message delivery %s\n", stringValue(result, "peer"), stringValue(result, "delivery"))
	return 0
}

func runPeerDelegate(c *client, a args) int {
	if len(a.pos) < 2 {
		return usage("peer delegate CORR_ID PEER [--note TEXT]")
	}
	from := a.string("from-peer", "")
	if from == "" {
		var err error
		if from, err = cliPeerIdentity(c); err != nil {
			return fatal(err)
		}
	}
	body := map[string]any{"to_peer": a.pos[1], "from_peer": from}
	if note := a.string("note", ""); note != "" {
		body["note"] = note
	}
	result, err := c.request(http.MethodPost, "/asks/"+url.PathEscape(a.pos[0])+"/delegate", body)
	if err != nil {
		return fatal(err)
	}
	fmt.Printf("delegated #%s to @%s (%d hop(s))\n", a.pos[0], stringValue(result, "delegated_to"), len(anySlice(result["delegations"])))
	return 0
}

// peerOrSelf is --peer, or this pane's registered peer.
func peerOrSelf(c *client, a args) (string, error) {
	if peer := a.string("peer", ""); peer != "" {
		return peer, nil
	}
	return cliPeerIdentity(c)
}

func messageLine(msg map[string]any) string {
	mark := "•"
	if stringValue(msg, "read_at") != "" {
		mark = " "
	}
	thread := ""
	if cid := stringValue(msg, "correlation_id"); cid != "" {
		thread = " #" + cid
	}
	return fmt.Sprintf("%s %s  %-6s @%s%s  %s", mark, stringValue(msg, "message_id"), stringValue(msg, "kind"), stringValue(msg, "from"), thread, stringValue(msg, "preview"))
}
