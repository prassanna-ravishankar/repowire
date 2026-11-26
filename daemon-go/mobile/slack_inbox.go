package mobile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Slack as a surface of the human inbox (#468), on the same protocol as the
// Telegram surface (#466): the daemon pushes each item as an inbox_item
// frame, the bridge renders it and answers with a delivery_ack receipt,
// pushed (with the Slack message ts as surface_ref) or failed (with an error
// class, never the error text). Slack threads are the inbox's threads: an
// item the daemon names a reply_to_ref for is posted into that ref's thread
// (thread_ts), and a human reply in an item's thread answers that item
// through POST /inbox/reply. A choice question renders as buttons, an
// acknowledge question as one Ack button, and a tool approval adds Deny.
// The bridge also declares inbox_hold, so quiet hours (#476) apply here.
//
// Read state follows what the human does: a reply or a button tap reads the
// item it acts on. Slack has no cheap "everything above this message" horizon
// for a bot (ts values are not the integer refs the daemon's through_ref
// filter reads), so there is no read sweep as on Telegram.

// slackMessageLimit keeps one chunk inside a Block Kit section's 3000
// characters (the last chunk of a question is rendered as one), with room
// for the sender header.
const slackMessageLimit = 2900

// slackError is a Slack Web API failure with its class, for receipts. Its
// text never contains a token.
type slackError struct {
	class      string // network | rate_limited | too_long | rejected | other
	msg        string
	retryAfter time.Duration
	// unsent marks a network failure before the request reached Slack (it
	// could not connect), the only network failure safe to retry: after a
	// lost response Slack may already have posted, and a retry would post
	// the message twice.
	unsent bool
}

func (e *slackError) Error() string { return "slack: " + e.msg }

// classifySlackError maps a Slack error code (the envelope's "error") or an
// HTTP 429 to a receipt class.
func classifySlackError(status int, code string, retryAfter time.Duration) error {
	class := "other"
	switch {
	case status == http.StatusTooManyRequests || code == "ratelimited":
		class = "rate_limited"
	case code == "msg_too_long":
		class = "too_long"
	case code == "channel_not_found" || code == "not_in_channel" || code == "is_archived" || code == "invalid_auth" ||
		code == "not_authed" || code == "account_inactive" || code == "token_revoked" || code == "missing_scope" || code == "restricted_action":
		class = "rejected"
	}
	if code == "" {
		code = fmt.Sprintf("HTTP %d", status)
	}
	return &slackError{class: class, msg: code, retryAfter: retryAfter}
}

func slackErrorClass(err error) string {
	var se *slackError
	if errors.As(err, &se) {
		return se.class
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "network"
	}
	return "other"
}

func (b *Slack) enqueueInboxItem(ctx context.Context, frame map[string]any) error {
	select {
	case b.inbox <- frame:
		return nil
	default:
		// Never block the daemon read loop: a full queue is a failed push the
		// sender can see, not a silent drop.
		return b.writeReceipt(ctx, textField(frame, "delivery_id"), nil, errors.New("queue full"), "overloaded")
	}
}

func (b *Slack) inboxWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-b.inbox:
			b.handleInboxItem(ctx, frame)
		}
	}
}

// handleInboxItem renders one item and reports the receipt.
func (b *Slack) handleInboxItem(ctx context.Context, frame map[string]any) {
	pushCtx, cancel := context.WithTimeout(ctx, b.pushTimeout)
	defer cancel()
	item, _ := frame["item"].(map[string]any)
	deliveryID := textField(frame, "delivery_id")
	if item == nil || deliveryID == "" {
		return
	}
	refs, err := b.pushInboxItem(pushCtx, item, textField(frame, "reply_to_ref"))
	class := ""
	if err != nil {
		class = slackErrorClass(err)
		fmt.Printf("slack: inbox push %s failed (%s): %s\n", deliveryID, class, b.redact(err.Error()))
	}
	if werr := b.writeReceipt(ctx, deliveryID, refs, err, class); werr != nil {
		fmt.Printf("slack: inbox receipt %s not sent: %v\n", deliveryID, werr)
	}
}

// writeReceipt acks a push: surface_ref is the first message (the thread
// anchor), surface_refs every message rendered, so a reply in any of their
// threads reaches the item.
func (b *Slack) writeReceipt(ctx context.Context, deliveryID string, refs []string, err error, class string) error {
	receipt := map[string]any{"type": "delivery_ack", "delivery_id": deliveryID, "message_type": "inbox_item", "status": "pushed"}
	if err != nil {
		receipt["status"], receipt["error_class"] = "failed", class
	}
	if len(refs) > 0 {
		receipt["surface_ref"], receipt["surface_refs"] = refs[0], refs
	}
	return b.writeFrame(ctx, receipt)
}

// pushInboxItem posts the item (split when long) and returns the ts of every
// message it rendered, in order. An item the daemon threads under an earlier
// one is posted in that message's thread; a new item starts a thread of its
// own, anchored at its first message.
func (b *Slack) pushInboxItem(ctx context.Context, item map[string]any, replyToRef string) ([]string, error) {
	id := textField(item, "id")
	chunks := splitMessage(slackInboxHeader(item)+"\n"+escapeSlack(textField(item, "text")), slackMessageLimit)
	thread := replyToRef
	var refs []string
	for index, chunk := range chunks {
		payload := map[string]any{"channel": b.channel, "text": chunk}
		if thread != "" {
			payload["thread_ts"] = thread
		}
		if index == len(chunks)-1 {
			if blocks := b.inboxBlocks(id, item, chunk); blocks != nil {
				payload["blocks"] = blocks
			}
		}
		var sent struct {
			TS string `json:"ts"`
		}
		if err := b.apiWithRetry(ctx, "chat.postMessage", payload, &sent); err != nil {
			return refs, err
		}
		refs = append(refs, sent.TS)
		if thread == "" {
			thread = sent.TS // the rest of a long item stays in its thread
		}
	}
	return refs, nil
}

// apiWithRetry retries rate limits and failures to connect a bounded number
// of times. Anything Slack rejects, and a response lost after the request
// was sent, fails at once: Slack may have posted it, and a retry would post
// the message twice.
func (b *Slack) apiWithRetry(ctx context.Context, method string, payload, output any) error {
	var err error
	for attempt := 1; attempt <= pushAttempts; attempt++ {
		if err = b.botAPI(ctx, method, payload, output); err == nil {
			return nil
		}
		var se *slackError
		if !errors.As(err, &se) || !(se.class == "rate_limited" || se.unsent) || attempt == pushAttempts {
			return err
		}
		wait := b.retryDelay * time.Duration(attempt)
		if se.retryAfter > wait {
			wait = se.retryAfter
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
	return err
}

func slackInboxHeader(item map[string]any) string {
	prefix := map[string]string{"ask": ":question: ", "answer": ":leftwards_arrow_with_hook: ", "broadcast": ":loudspeaker: "}[textField(item, "kind")]
	return prefix + "*@" + escapeSlack(textField(item, "from")) + "*"
}

// inboxBlocks is the item's text plus its buttons: a choice question's
// options, one Ack for an acknowledge question, Deny for a tool approval;
// nil for anything answered by a thread reply. Button values carry the same
// ix: actions as Telegram's callbacks.
func (b *Slack) inboxBlocks(id string, item map[string]any, text string) []any {
	question, _ := item["question"].(map[string]any)
	if textField(item, "kind") != "ask" || question == nil {
		return nil
	}
	var buttons []any
	button := func(label, value string) {
		buttons = append(buttons, map[string]any{
			"type": "button", "text": map[string]string{"type": "plain_text", "text": label},
			"value": value, "action_id": fmt.Sprintf("inbox_%d", len(buttons)),
		})
	}
	switch question["kind"] {
	case "acknowledge":
		button("✓ Ack", "ix:a:"+id)
	case "choice":
		options, _ := question["options"].([]any)
		var ids []string
		for _, raw := range options {
			option, _ := raw.(map[string]any)
			optionID := textField(option, "id")
			if optionID == "" {
				continue
			}
			button(firstText(textField(option, "title"), optionID), fmt.Sprintf("ix:o:%s:%d", id, len(ids)))
			ids = append(ids, optionID)
		}
		b.rememberOptions(id, ids)
		if textField(question, "scope") == "tool_permission" {
			button("✕ Deny", "deny:"+textField(item, "correlation_id"))
		}
	}
	if len(buttons) == 0 {
		return nil
	}
	return []any{
		map[string]any{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": text}},
		map[string]any{"type": "actions", "elements": buttons},
	}
}

func (b *Slack) rememberOptions(key string, ids []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.options) >= 100 {
		for stale := range b.options {
			delete(b.options, stale)
			break
		}
	}
	b.options[key] = ids
}

// onInboxAction handles a button value: "ix:o:<id>:<n>" (choice),
// "ix:a:<id>" (acknowledge) and "deny:<cid>" (tool approval). handled is
// false for any other value.
func (b *Slack) onInboxAction(ctx context.Context, value, threadTS string) (handled bool, err error) {
	switch {
	case strings.HasPrefix(value, "deny:"):
		cid := strings.TrimPrefix(value, "deny:")
		var result map[string]any
		resp, err := b.daemon.JSON(ctx, http.MethodPost, "/answer", map[string]any{"correlation_id": cid, "outcome": "denied"}, &result)
		if err != nil || resp.StatusCode >= 300 {
			return true, b.sendThread(ctx, threadTS, ":x: Deny failed: "+escapeSlack(daemonDetail(result, resp, err)))
		}
		return true, b.sendThread(ctx, threadTS, ":no_entry_sign: Denied")
	case !strings.HasPrefix(value, "ix:"):
		return false, nil
	}
	parts := strings.Split(strings.TrimPrefix(value, "ix:"), ":")
	if len(parts) < 2 {
		return true, nil
	}
	action, id := parts[0], parts[1]
	switch action {
	case "a":
		return true, b.replyInbox(ctx, id, threadTS, map[string]any{"surface": "slack", "text": ""}, ":white_check_mark: Acked")
	case "o":
		if len(parts) != 3 {
			return true, nil
		}
		var index int
		if _, err := fmt.Sscanf(parts[2], "%d", &index); err != nil {
			return true, nil
		}
		b.mu.Lock()
		ids := b.options[id]
		b.mu.Unlock()
		if index < 0 || index >= len(ids) {
			return true, b.sendThread(ctx, threadTS, "That choice is no longer available.")
		}
		return true, b.replyInbox(ctx, id, threadTS, map[string]any{"surface": "slack", "option_id": ids[index]}, ":white_check_mark: Answered")
	}
	return true, nil
}

func (b *Slack) replyInbox(ctx context.Context, id, threadTS string, payload map[string]any, done string) error {
	var result map[string]any
	resp, err := b.daemon.JSON(ctx, http.MethodPost, "/inbox/"+id+"/reply", payload, &result)
	if err != nil || resp.StatusCode >= 300 {
		return b.sendThread(ctx, threadTS, ":x: Reply failed: "+escapeSlack(daemonDetail(result, resp, err)))
	}
	return b.sendThread(ctx, threadTS, done)
}

// onThreadReply sends a human reply in a thread the bridge rendered as a
// reply on that thread's inbox item. handled is false when the thread is not
// an inbox item's (or the daemon has no inbox): the caller then treats the
// text as an ordinary message.
func (b *Slack) onThreadReply(ctx context.Context, threadTS, ts, text string) (handled bool, err error) {
	if threadTS == "" || threadTS == ts {
		return false, nil
	}
	var result map[string]any
	resp, err := b.daemon.JSON(ctx, http.MethodPost, "/inbox/reply",
		// ref is the thread root, not this reply's ts: an agent's answer to
		// it is posted with thread_ts = ref, and Slack threads only on a root.
		map[string]any{"surface": "slack", "surface_ref": threadTS, "ref": threadTS, "text": text}, &result)
	if err != nil {
		return true, err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusServiceUnavailable {
		return false, nil
	}
	if resp.StatusCode >= 300 {
		return true, b.sendThread(ctx, threadTS, ":x: Reply failed: "+escapeSlack(daemonDetail(result, resp, nil)))
	}
	return true, nil
}

// sendThread posts a short status into a thread (the channel when none).
func (b *Slack) sendThread(ctx context.Context, threadTS, text string) error {
	payload := map[string]any{"channel": b.channel, "text": text}
	if threadTS != "" {
		payload["thread_ts"] = threadTS
	}
	return b.botAPI(ctx, "chat.postMessage", payload, nil)
}

// redact removes both tokens from an error's text.
func (b *Slack) redact(text string) string {
	for _, token := range []string{b.botToken, b.appToken} {
		if token != "" {
			text = strings.ReplaceAll(text, token, "<redacted>")
		}
	}
	return text
}
