package mobile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Telegram as a surface of the human inbox (#466). The daemon pushes each
// inbox item as an inbox_item frame; the bridge renders it, threads it under
// the earlier message on the same ask when the daemon names one, and answers
// with a delivery_ack receipt: pushed (with the Telegram message_id) or failed
// (with an error class, never the error text). Replies, ack buttons and the
// seen button go back through the daemon's /inbox routes, so whatever the
// human does here is visible on every other surface.

// inboxSurfaceCapability matches proto.CapInboxSurface.
const inboxSurfaceCapability = "inbox_surface"

// telegramMessageLimit leaves headroom under Telegram's 4096-character cap
// for the sender header.
const telegramMessageLimit = 3900

// pushAttempts bounds retries of one Telegram call within a push.
const pushAttempts = 3

// telegramError is a Telegram failure with its class, for receipts. Its text
// never contains the bot token.
type telegramError struct {
	class      string // network | rate_limited | too_long | rejected | other
	msg        string
	retryAfter time.Duration
}

func (e *telegramError) Error() string { return "telegram: " + e.msg }

func classifyTelegramError(code int, description string, retryAfter int) error {
	class := "other"
	switch {
	case code == http.StatusTooManyRequests:
		class = "rate_limited"
	case strings.Contains(strings.ToLower(description), "too long"):
		class = "too_long"
	case code == http.StatusBadRequest || code == http.StatusForbidden:
		class = "rejected"
	}
	return &telegramError{class: class, msg: description, retryAfter: time.Duration(retryAfter) * time.Second}
}

func errorClass(err error) string {
	var te *telegramError
	if errors.As(err, &te) {
		return te.class
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "network"
	}
	return "other"
}

// redact removes the bot token, which Go's HTTP errors carry inside the URL.
func (b *Telegram) redact(text string) string {
	if b.token == "" {
		return text
	}
	return strings.ReplaceAll(text, b.token, "<redacted>")
}

func (b *Telegram) enqueueInboxItem(ctx context.Context, frame map[string]any) error {
	select {
	case b.inbox <- frame:
		return nil
	default:
		// Never block the read loop. A full queue is a failed push the sender
		// can see, not a silent drop.
		return b.writeReceipt(ctx, textField(frame, "delivery_id"), "", errors.New("queue full"), "overloaded")
	}
}

func (b *Telegram) inboxWorker(ctx context.Context) {
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
func (b *Telegram) handleInboxItem(ctx context.Context, frame map[string]any) {
	pushCtx, cancel := context.WithTimeout(ctx, b.pushTimeout)
	defer cancel()
	item, _ := frame["item"].(map[string]any)
	deliveryID := textField(frame, "delivery_id")
	if item == nil || deliveryID == "" {
		return
	}
	b.touchRecent(textField(item, "from"))
	ref, err := b.pushInboxItem(pushCtx, item, textField(frame, "reply_to_ref"))
	class := ""
	if err != nil {
		class = errorClass(err)
		fmt.Printf("telegram: inbox push %s failed (%s): %s\n", deliveryID, class, b.redact(err.Error()))
	}
	if werr := b.writeReceipt(ctx, deliveryID, ref, err, class); werr != nil {
		fmt.Printf("telegram: inbox receipt %s not sent: %v\n", deliveryID, werr)
	}
}

func (b *Telegram) writeReceipt(ctx context.Context, deliveryID, ref string, err error, class string) error {
	receipt := map[string]any{"type": "delivery_ack", "delivery_id": deliveryID, "message_type": "inbox_item", "status": "pushed"}
	if err != nil {
		receipt["status"], receipt["error_class"] = "failed", class
	} else if ref != "" {
		receipt["surface_ref"] = ref
	}
	return b.writeFrame(ctx, receipt)
}

// pushInboxItem sends the item (split when long) and returns the first
// message's id, which is what a later reply-to or thread names.
func (b *Telegram) pushInboxItem(ctx context.Context, item map[string]any, replyToRef string) (string, error) {
	id := textField(item, "id")
	chunks := splitMessage(inboxHeader(item)+"\n"+textField(item, "text"), telegramMessageLimit)
	first := ""
	for index, chunk := range chunks {
		payload := map[string]any{"chat_id": b.chatID, "text": chunk}
		if index == 0 && replyToRef != "" {
			if refID, err := strconv.ParseInt(replyToRef, 10, 64); err == nil {
				payload["reply_parameters"] = map[string]any{"message_id": refID, "allow_sending_without_reply": true}
			}
		}
		if index == len(chunks)-1 {
			payload["reply_markup"] = b.inboxMarkup(id, item)
		}
		var sent struct {
			MessageID int64 `json:"message_id"`
		}
		if err := b.callWithRetry(ctx, "sendMessage", payload, &sent); err != nil {
			return first, err
		}
		if index == 0 {
			first = strconv.FormatInt(sent.MessageID, 10)
		}
	}
	if attachments, ok := item["attachments"].([]any); ok {
		for _, raw := range attachments {
			attachment, _ := raw.(map[string]any)
			if err := b.sendAttachment(ctx, attachment); err != nil {
				// The message itself landed; a failed attachment is told to
				// the human in-line rather than failing the whole item.
				_ = b.send(ctx, "📎 attachment delivery failed: "+errorClass(err), nil)
			}
		}
	}
	return first, nil
}

// callWithRetry retries network failures and rate limits a bounded number of
// times; anything Telegram rejects outright fails at once.
func (b *Telegram) callWithRetry(ctx context.Context, method string, payload, output any) error {
	var err error
	for attempt := 1; attempt <= pushAttempts; attempt++ {
		if err = b.call(ctx, method, payload, output); err == nil {
			return nil
		}
		var te *telegramError
		if !errors.As(err, &te) || (te.class != "network" && te.class != "rate_limited") || attempt == pushAttempts {
			return err
		}
		wait := b.retryDelay * time.Duration(attempt)
		if te.retryAfter > wait {
			wait = te.retryAfter
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

func inboxHeader(item map[string]any) string {
	prefix := map[string]string{"ask": "❓ ", "answer": "↩ ", "broadcast": "📢 "}[textField(item, "kind")]
	return prefix + "@" + textField(item, "from")
}

// inboxMarkup is the reply affordance: choices or an ack for an ask, a seen
// button for everything else. Callback data stays well under Telegram's 64
// bytes ("ix:o:inb-xxxxxxxx:N").
func (b *Telegram) inboxMarkup(id string, item map[string]any) any {
	if textField(item, "kind") != "ask" {
		return map[string]any{"inline_keyboard": []any{[]any{map[string]string{"text": "✓ Seen", "callback_data": "ix:s:" + id}}}}
	}
	rows := []any{}
	question, _ := item["question"].(map[string]any)
	if question["kind"] == "choice" {
		options, _ := question["options"].([]any)
		ids := []string{}
		for _, raw := range options {
			option, _ := raw.(map[string]any)
			optionID := textField(option, "id")
			if optionID == "" {
				continue
			}
			title := firstText(textField(option, "title"), optionID)
			rows = append(rows, []any{map[string]string{"text": title, "callback_data": fmt.Sprintf("ix:o:%s:%d", id, len(ids))}})
			ids = append(ids, optionID)
		}
		b.rememberOptions(id, ids)
		if textField(question, "scope") == "tool_permission" {
			rows = append(rows, []any{map[string]string{"text": "✕ Deny", "callback_data": "deny:" + textField(item, "correlation_id")}})
		}
	}
	if len(rows) == 0 {
		rows = append(rows, []any{map[string]string{"text": "✓ Ack", "callback_data": "ix:a:" + id}})
	}
	return map[string]any{"inline_keyboard": rows}
}

func (b *Telegram) rememberOptions(key string, ids []string) {
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

// onInboxCallback handles "a:<id>" (ack), "o:<id>:<n>" (choice), "s:<id>" (seen).
func (b *Telegram) onInboxCallback(ctx context.Context, data string) error {
	parts := strings.Split(data, ":")
	if len(parts) < 2 {
		return nil
	}
	action, id := parts[0], parts[1]
	switch action {
	case "s":
		var result map[string]any
		resp, err := b.daemon.JSON(ctx, http.MethodPost, "/inbox/"+id+"/read", map[string]any{"surface": "telegram"}, &result)
		if err != nil || resp.StatusCode >= 300 {
			return b.send(ctx, "✗ Couldn't mark read: "+daemonDetail(result, resp, err), nil)
		}
		return nil
	case "a":
		return b.replyInbox(ctx, id, map[string]any{"surface": "telegram", "text": ""}, "✓ Acked")
	case "o":
		if len(parts) != 3 {
			return nil
		}
		index, _ := strconv.Atoi(parts[2])
		b.mu.Lock()
		ids := b.options[id]
		b.mu.Unlock()
		if index < 0 || index >= len(ids) {
			return b.send(ctx, "That choice is no longer available.", nil)
		}
		return b.replyInbox(ctx, id, map[string]any{"surface": "telegram", "option_id": ids[index]}, "✓ Answered")
	}
	return nil
}

func (b *Telegram) replyInbox(ctx context.Context, id string, payload map[string]any, done string) error {
	var result map[string]any
	resp, err := b.daemon.JSON(ctx, http.MethodPost, "/inbox/"+id+"/reply", payload, &result)
	if err != nil || resp.StatusCode >= 300 {
		return b.send(ctx, "✗ Reply failed: "+daemonDetail(result, resp, err), nil)
	}
	return b.send(ctx, done, nil)
}

// onReply sends a Telegram reply-to as a reply on the inbox item that message
// rendered. handled is false when the message is not an inbox item (or the
// daemon has no inbox), so the caller treats the text as an ordinary message.
func (b *Telegram) onReply(ctx context.Context, replyTo int64, text string, messageID int64) (handled bool, err error) {
	if replyTo == 0 {
		return false, nil
	}
	payload := map[string]any{
		"surface": "telegram", "surface_ref": strconv.FormatInt(replyTo, 10),
		"ref": strconv.FormatInt(messageID, 10), "text": text,
	}
	var result map[string]any
	resp, err := b.daemon.JSON(ctx, http.MethodPost, "/inbox/reply", payload, &result)
	if err != nil {
		return true, err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusServiceUnavailable {
		return false, nil
	}
	if resp.StatusCode >= 300 {
		return true, b.send(ctx, "✗ Reply failed: "+daemonDetail(result, resp, nil), nil)
	}
	if messageID != 0 {
		_ = b.react(ctx, messageID)
	}
	return true, nil
}

func daemonDetail(result map[string]any, resp *http.Response, err error) string {
	if err != nil {
		return err.Error()
	}
	if detail := textField(result, "detail"); detail != "" {
		return detail
	}
	if resp != nil {
		return resp.Status
	}
	return "unknown error"
}

// splitMessage cuts text into chunks of at most limit runes, preferring a
// newline boundary in the second half of each chunk.
func splitMessage(text string, limit int) []string {
	var chunks []string
	for utf8.RuneCountInString(text) > limit {
		runes := []rune(text)
		cut := limit
		for i := limit; i > limit/2; i-- {
			if runes[i-1] == '\n' {
				cut = i
				break
			}
		}
		chunks = append(chunks, string(runes[:cut]))
		text = string(runes[cut:])
	}
	return append(chunks, text)
}
