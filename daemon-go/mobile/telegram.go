package mobile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Telegram struct {
	token   string
	chatID  string
	apiBase string
	daemon  *DaemonPeer
	http    *http.Client

	mu       sync.Mutex
	offset   int64
	target   string
	recents  []string
	pending  *telegramPending
	options  map[string][]string
	labels   map[string]string
	targets  map[string]string
	keyboard bool
}

type telegramPending struct {
	message     string
	mode        string
	attachments []map[string]any
	expiresAt   time.Time
}

func NewTelegram(token, chatID string, daemon *DaemonPeer) *Telegram {
	return &Telegram{
		token: token, chatID: chatID, apiBase: "https://api.telegram.org", daemon: daemon,
		http: &http.Client{Timeout: 40 * time.Second}, options: map[string][]string{},
		labels: map[string]string{}, targets: map[string]string{}, keyboard: true,
	}
}

func (b *Telegram) Run(ctx context.Context) error {
	if b.token == "" || b.chatID == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID are required")
	}
	if target, label := b.daemon.DefaultTargetInfo(ctx); target != "" {
		b.setTargetLabel(target, label)
	}
	return runTogether(ctx, func(ctx context.Context) error { return b.daemon.Run(ctx, b.onDaemon) }, b.poll)
}

func (b *Telegram) endpoint(method string) string {
	return strings.TrimRight(b.apiBase, "/") + "/bot" + b.token + "/" + method
}

func (b *Telegram) call(ctx context.Context, method string, input any, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint(method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if !envelope.OK {
		return fmt.Errorf("telegram: %s", envelope.Description)
	}
	if output != nil && len(envelope.Result) > 0 {
		return json.Unmarshal(envelope.Result, output)
	}
	return nil
}

func (b *Telegram) send(ctx context.Context, text string, markup any) error {
	payload := map[string]any{"chat_id": b.chatID, "text": text}
	if markup != nil {
		payload["reply_markup"] = markup
	} else if b.keyboardEnabled() {
		payload["reply_markup"] = b.replyKeyboard()
	}
	return b.call(ctx, "sendMessage", payload, nil)
}

func (b *Telegram) onDaemon(ctx context.Context, message map[string]any) error {
	typ, from, text := textField(message, "type"), textField(message, "from_peer"), textField(message, "text")
	prefix := map[string]string{"notify": "", "query": "❓ ", "ask": "❓ ", "broadcast": "📢 "}[typ]
	if _, supported := map[string]bool{"notify": true, "query": true, "ask": true, "broadcast": true}[typ]; !supported {
		return nil
	}
	b.touchRecent(from)
	var markup any
	if typ == "ask" {
		cid := textField(message, "correlation_id")
		markup = b.askMarkup(cid, message["question"])
		if cid != "" {
			text += "\n[ask #" + shortID(cid) + "]"
		}
	}
	if err := b.send(ctx, prefix+"@"+from+"\n"+text, markup); err != nil {
		return err
	}
	if attachments, ok := message["attachments"].([]any); ok {
		for _, raw := range attachments {
			attachment, _ := raw.(map[string]any)
			if err := b.sendAttachment(ctx, attachment); err != nil {
				name := firstText(textField(attachment, "filename"), filepath.Base(textField(attachment, "path")), "attachment")
				_ = b.send(ctx, "📎 "+name+" (delivery failed: "+err.Error()+")", nil)
			}
		}
	}
	return nil
}

func (b *Telegram) askMarkup(cid string, raw any) any {
	if cid == "" {
		return nil
	}
	rows := []any{}
	question, _ := raw.(map[string]any)
	if question["kind"] == "choice" {
		options, _ := question["options"].([]any)
		ids := make([]string, 0, len(options))
		for index, rawOption := range options {
			option, _ := rawOption.(map[string]any)
			id := textField(option, "id")
			if id == "" {
				continue
			}
			ids = append(ids, id)
			title := textField(option, "title")
			if title == "" {
				title = id
			}
			rows = append(rows, []any{map[string]string{"text": title, "callback_data": fmt.Sprintf("answer:%s:%d", cid, index)}})
		}
		b.mu.Lock()
		if len(b.options) >= 100 {
			for stale := range b.options {
				delete(b.options, stale)
				break
			}
		}
		b.options[cid] = ids
		b.mu.Unlock()
		if textField(question, "scope") == "tool_permission" {
			rows = append(rows, []any{map[string]string{"text": "✕ Deny", "callback_data": "deny:" + cid}})
		}
	}
	if len(rows) == 0 {
		rows = append(rows, []any{map[string]string{"text": "✓ Ack", "callback_data": "ack:" + cid}})
	}
	return map[string]any{"inline_keyboard": rows}
}

func (b *Telegram) poll(ctx context.Context) error {
	for ctx.Err() == nil {
		b.mu.Lock()
		offset := b.offset
		b.mu.Unlock()
		var updates []map[string]any
		if err := b.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 30}, &updates); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Printf("telegram: getUpdates: %v\n", err)
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
			continue
		}
		failed := false
		for _, update := range updates {
			if err := b.handleUpdate(ctx, update); err != nil {
				fmt.Printf("telegram: update failed: %v\n", err)
				// Do not acknowledge a failed update. Telegram will return it
				// again, which makes transient daemon/API failures recoverable.
				failed = true
				break
			}
		}
		if failed {
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
	}
	return nil
}

func (b *Telegram) handleUpdate(ctx context.Context, update map[string]any) error {
	if err := b.onUpdate(ctx, update); err != nil {
		return err
	}
	if id, ok := number(update["update_id"]); ok {
		b.mu.Lock()
		b.offset = int64(id) + 1
		b.mu.Unlock()
	}
	return nil
}

func (b *Telegram) onUpdate(ctx context.Context, update map[string]any) error {
	if callback, ok := update["callback_query"].(map[string]any); ok {
		message, _ := callback["message"].(map[string]any)
		if !b.matchesChat(message) {
			return nil
		}
		_ = b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": textField(callback, "id")}, nil)
		return b.onCallback(ctx, textField(callback, "data"))
	}
	message, _ := update["message"].(map[string]any)
	if !b.matchesChat(message) {
		return nil
	}
	if photos, ok := message["photo"].([]any); ok && len(photos) > 0 {
		photo, _ := photos[len(photos)-1].(map[string]any)
		return b.onPhoto(ctx, textField(photo, "file_id"), textField(message, "caption"), int64(numberOrZero(message["message_id"])))
	}
	text := strings.TrimSpace(textField(message, "text"))
	if text != "" {
		return b.onText(ctx, text, int64(numberOrZero(message["message_id"])))
	}
	return nil
}

func (b *Telegram) matchesChat(message map[string]any) bool {
	chat, _ := message["chat"].(map[string]any)
	return fmt.Sprint(chat["id"]) == b.chatID
}

func (b *Telegram) onCallback(ctx context.Context, data string) error {
	switch {
	case strings.HasPrefix(data, "target:"):
		target := strings.TrimPrefix(data, "target:")
		b.setTarget(target)
		if pending := b.takePending(); pending != nil {
			if _, err := b.daemon.Send(ctx, target, pending.message, pending.mode, pending.attachments); err != nil {
				b.setPending(pending)
				return b.send(ctx, "✗ Retry failed: "+err.Error(), nil)
			}
			return b.send(ctx, "✓ Sent to @"+b.displayTarget(target)+".", nil)
		}
		return b.send(ctx, "Now talking to @"+b.displayTarget(target)+".", nil)
	case strings.HasPrefix(data, "ack:"):
		cid := strings.TrimPrefix(data, "ack:")
		var result map[string]any
		resp, err := b.daemon.JSON(ctx, http.MethodPost, "/ack", map[string]any{"correlation_id": cid, "from_peer": b.daemon.Name()}, &result)
		if err != nil || resp.StatusCode >= 300 {
			return fmt.Errorf("ack %s failed", shortID(cid))
		}
		return b.send(ctx, "✓ Acked #"+shortID(cid), nil)
	case strings.HasPrefix(data, "deny:"):
		return b.answer(ctx, strings.TrimPrefix(data, "deny:"), "", "denied")
	case strings.HasPrefix(data, "answer:"):
		parts := strings.Split(data, ":")
		if len(parts) != 3 {
			return nil
		}
		index, _ := strconv.Atoi(parts[2])
		b.mu.Lock()
		ids := b.options[parts[1]]
		b.mu.Unlock()
		if index < 0 || index >= len(ids) {
			return b.send(ctx, "That choice is no longer available.", nil)
		}
		return b.answer(ctx, parts[1], ids[index], "answered")
	}
	return nil
}

func (b *Telegram) answer(ctx context.Context, cid, option, outcome string) error {
	payload := map[string]any{"correlation_id": cid, "outcome": outcome}
	if option != "" {
		payload["option_id"] = option
	}
	var result map[string]any
	resp, err := b.daemon.JSON(ctx, http.MethodPost, "/answer", payload, &result)
	if err != nil || resp.StatusCode >= 300 {
		return fmt.Errorf("answer %s failed", shortID(cid))
	}
	b.mu.Lock()
	delete(b.options, cid)
	b.mu.Unlock()
	return b.send(ctx, "✓ Answered #"+shortID(cid), nil)
}

func (b *Telegram) onText(ctx context.Context, text string, messageIDs ...int64) error {
	if target, ok := keyboardTarget(text); ok {
		target = b.resolveTarget(target)
		b.setTarget(target)
		if pending := b.takePending(); pending != nil {
			if _, err := b.daemon.Send(ctx, target, pending.message, pending.mode, pending.attachments); err != nil {
				b.setPending(pending)
				return b.send(ctx, "✗ Retry failed: "+err.Error(), nil)
			}
			return b.send(ctx, "✓ Sent to @"+b.displayTarget(target)+".", nil)
		}
		return b.send(ctx, "Now talking to @"+b.displayTarget(target)+".", nil)
	}
	switch {
	case text == "/start" || text == "/peers" || text == "/list" || text == "📋 peers":
		return b.sendPeers(ctx)
	case text == "/clear" || text == "❌ clear":
		b.setTarget("")
		b.clearPending()
		return b.send(ctx, "Cleared. No active conversation.", nil)
	case text == "/keyboard off":
		b.setKeyboard(false)
		return b.call(ctx, "sendMessage", map[string]any{"chat_id": b.chatID, "text": "Keyboard hidden. Use /keyboard on to restore.", "reply_markup": map[string]bool{"remove_keyboard": true}}, nil)
	case text == "/keyboard on":
		b.setKeyboard(true)
		return b.send(ctx, "Keyboard restored.", nil)
	case strings.HasPrefix(text, "/select ") || strings.HasPrefix(text, "/switch "):
		parts := strings.SplitN(text, " ", 2)
		b.setTarget(strings.TrimPrefix(strings.TrimSpace(parts[1]), "@"))
		return b.send(ctx, "Now talking to @"+b.displayTarget(b.targetName())+".", nil)
	}
	b.clearPending()
	mode, target, message, targeted := parseTargeted(text, true)
	if !targeted {
		mode, target, message = "ask", b.targetName(), text
	}
	if target == "" {
		target = b.targetName()
	}
	if target == "" {
		var label string
		target, label = b.daemon.DefaultTargetInfo(ctx)
		b.rememberTarget(target, label)
	}
	if target == "" {
		return b.send(ctx, "No active conversation. Use /peers or @name message.", nil)
	}
	if message == "" {
		return b.send(ctx, "Message text is required.", nil)
	}
	b.setTarget(target)
	_, err := b.daemon.Send(ctx, target, message, mode, nil)
	if err != nil {
		b.setPending(&telegramPending{message: message, mode: mode, expiresAt: time.Now().Add(time.Minute)})
		return b.send(ctx, "✗ Couldn't reach @"+b.displayTarget(target)+": "+err.Error()+"\nTap a peer to retry, or type a new message to cancel.", nil)
	}
	if len(messageIDs) > 0 && messageIDs[0] != 0 {
		_ = b.react(ctx, messageIDs[0])
	}
	return nil
}

func (b *Telegram) sendPeers(ctx context.Context) error {
	peers, err := b.daemon.ActivePeers(ctx)
	if err != nil {
		return b.send(ctx, "Error listing peers: "+err.Error(), nil)
	}
	if len(peers) == 0 {
		return b.send(ctx, "No peers online.", nil)
	}
	lines := make([]string, 0, len(peers))
	rows := make([]any, 0, len(peers))
	for _, peer := range peers {
		name, target := textField(peer, "display_name"), peerTarget(peer)
		b.rememberTarget(target, name)
		lines = append(lines, "• @"+name+" — "+textField(peer, "status"))
		rows = append(rows, []any{map[string]string{"text": "💬 " + name, "callback_data": "target:" + target}})
	}
	return b.send(ctx, strings.Join(lines, "\n"), map[string]any{"inline_keyboard": rows})
}

func (b *Telegram) onPhoto(ctx context.Context, fileID, caption string, messageIDs ...int64) error {
	target := b.targetName()
	if target == "" {
		return b.send(ctx, "Select a peer first, then send the photo.", nil)
	}
	var file struct {
		FilePath string `json:"file_path"`
	}
	if err := b.call(ctx, "getFile", map[string]string{"file_id": fileID}, &file); err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(b.apiBase, "/")+"/file/bot"+b.token+"/"+file.FilePath, nil)
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	content, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	attachment, err := b.daemon.UploadAttachment(ctx, filepath.Base(file.FilePath), "image/jpeg", content)
	if err != nil {
		return err
	}
	if caption == "" {
		caption = "Photo attached"
	}
	_, err = b.daemon.Send(ctx, target, caption, "ask", []map[string]any{attachment})
	if err != nil {
		b.setPending(&telegramPending{message: caption, mode: "ask", attachments: []map[string]any{attachment}, expiresAt: time.Now().Add(time.Minute)})
		return b.send(ctx, "✗ Couldn't reach @"+target+": "+err.Error()+"\nTap a peer to retry, or send a new message to cancel.", nil)
	}
	if len(messageIDs) > 0 && messageIDs[0] != 0 {
		_ = b.react(ctx, messageIDs[0])
	}
	return nil
}

func (b *Telegram) react(ctx context.Context, messageID int64) error {
	return b.call(ctx, "setMessageReaction", map[string]any{
		"chat_id": b.chatID, "message_id": messageID,
		"reaction": []any{map[string]string{"type": "emoji", "emoji": "👍"}},
	}, nil)
}

func (b *Telegram) sendAttachment(ctx context.Context, attachment map[string]any) error {
	name := firstText(textField(attachment, "filename"), filepath.Base(textField(attachment, "path")), "attachment")
	contentType := textField(attachment, "content_type")
	var content []byte
	var err error
	if path := textField(attachment, "path"); path != "" {
		if stat, statErr := os.Stat(path); statErr == nil && stat.Size() <= 16<<20 {
			content, err = os.ReadFile(path)
		}
	}
	if len(content) == 0 && textField(attachment, "id") != "" {
		content, err = b.daemon.DownloadAttachment(ctx, textField(attachment, "id"))
	}
	if err != nil {
		return err
	}
	if len(content) == 0 {
		return fmt.Errorf("attachment content unavailable")
	}
	method, field := "sendDocument", "document"
	if strings.HasPrefix(contentType, "image/") {
		method, field = "sendPhoto", "photo"
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", b.chatID); err != nil {
		return err
	}
	part, err := writer.CreateFormFile(field, name)
	if err != nil {
		return err
	}
	if _, err := part.Write(content); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint(method), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("telegram: %s", result.Description)
	}
	return nil
}

func (b *Telegram) setTarget(target string) {
	b.mu.Lock()
	b.target = target
	b.mu.Unlock()
}

func (b *Telegram) setTargetLabel(target, label string) {
	b.rememberTarget(target, label)
	b.setTarget(target)
}

func (b *Telegram) rememberTarget(target, label string) {
	if target == "" || label == "" {
		return
	}
	b.mu.Lock()
	b.labels[target] = label
	b.targets[label] = target
	b.mu.Unlock()
}

func (b *Telegram) displayTarget(target string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if label := b.labels[target]; label != "" {
		return label
	}
	return target
}

func (b *Telegram) resolveTarget(label string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if target := b.targets[label]; target != "" {
		return target
	}
	return label
}

func (b *Telegram) targetName() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.target
}

func (b *Telegram) setKeyboard(enabled bool) {
	b.mu.Lock()
	b.keyboard = enabled
	b.mu.Unlock()
}

func (b *Telegram) keyboardEnabled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.keyboard
}

func (b *Telegram) touchRecent(peer string) {
	if peer == "" {
		return
	}
	b.mu.Lock()
	filtered := []string{peer}
	for _, recent := range b.recents {
		if recent != peer && len(filtered) < 5 {
			filtered = append(filtered, recent)
		}
	}
	b.recents = filtered
	b.mu.Unlock()
}

func (b *Telegram) replyKeyboard() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	buttons := []any{}
	seen := map[string]bool{}
	if b.target != "" {
		label := b.target
		if known := b.labels[b.target]; known != "" {
			label = known
		}
		buttons = append(buttons, map[string]string{"text": "✦ " + label})
		seen[label] = true
	}
	for _, peer := range b.recents {
		if !seen[peer] && len(buttons) < 6 {
			buttons = append(buttons, map[string]string{"text": "💬 " + peer})
			seen[peer] = true
		}
	}
	rows := []any{}
	for len(buttons) > 0 {
		n := 3
		if len(buttons) < n {
			n = len(buttons)
		}
		rows = append(rows, append([]any(nil), buttons[:n]...))
		buttons = buttons[n:]
	}
	rows = append(rows, []any{map[string]string{"text": "📋 peers"}, map[string]string{"text": "❌ clear"}})
	return map[string]any{"keyboard": rows, "resize_keyboard": true, "is_persistent": true}
}

func keyboardTarget(text string) (string, bool) {
	for _, prefix := range []string{"✦ ", "💬 "} {
		if strings.HasPrefix(text, prefix) {
			target := strings.TrimSpace(strings.TrimPrefix(text, prefix))
			return target, target != ""
		}
	}
	return "", false
}

func (b *Telegram) setPending(pending *telegramPending) {
	b.mu.Lock()
	b.pending = pending
	b.mu.Unlock()
}

func (b *Telegram) clearPending() {
	b.mu.Lock()
	b.pending = nil
	b.mu.Unlock()
}

func (b *Telegram) takePending() *telegramPending {
	b.mu.Lock()
	defer b.mu.Unlock()
	pending := b.pending
	b.pending = nil
	if pending == nil || time.Now().After(pending.expiresAt) {
		return nil
	}
	return pending
}

func number(value any) (float64, bool) {
	n, ok := value.(float64)
	return n, ok
}

func numberOrZero(value any) float64 {
	n, _ := number(value)
	return n
}

func firstText(values ...string) string {
	for _, value := range values {
		if value != "" && value != "." {
			return value
		}
	}
	return ""
}

func shortID(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}
