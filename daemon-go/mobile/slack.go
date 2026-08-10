package mobile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type Slack struct {
	botToken string
	appToken string
	channel  string
	apiBase  string
	daemon   *DaemonPeer
	http     *http.Client

	mu     sync.Mutex
	target string
}

func NewSlack(botToken, appToken, channel string, daemon *DaemonPeer) *Slack {
	return &Slack{
		botToken: botToken, appToken: appToken, channel: channel,
		apiBase: "https://slack.com", daemon: daemon, http: &http.Client{Timeout: 20 * time.Second},
	}
}

func (b *Slack) Run(ctx context.Context) error {
	if b.botToken == "" || b.appToken == "" || b.channel == "" {
		return fmt.Errorf("SLACK_BOT_TOKEN, SLACK_APP_TOKEN, and SLACK_CHANNEL_ID are required")
	}
	if target := b.daemon.DefaultTarget(ctx); target != "" {
		b.setTarget(target)
	}
	return runTogether(ctx, func(ctx context.Context) error { return b.daemon.Run(ctx, b.onDaemon) }, b.socketLoop)
}

func (b *Slack) api(ctx context.Context, method, token string, input any, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(b.apiBase, "/")+"/api/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var envelope map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if ok, _ := envelope["ok"].(bool); !ok {
		return fmt.Errorf("slack %s: %s", method, textField(envelope, "error"))
	}
	if output != nil {
		encoded, _ := json.Marshal(envelope)
		return json.Unmarshal(encoded, output)
	}
	return nil
}

func (b *Slack) send(ctx context.Context, text string, blocks any) error {
	payload := map[string]any{"channel": b.channel, "text": text}
	if blocks != nil {
		payload["blocks"] = blocks
	}
	return b.api(ctx, "chat.postMessage", b.botToken, payload, nil)
}

func (b *Slack) onDaemon(ctx context.Context, message map[string]any) error {
	typ, from, text := textField(message, "type"), textField(message, "from_peer"), textField(message, "text")
	prefix, ok := map[string]string{"notify": "", "query": ":question: ", "ask": ":question: ", "broadcast": ":loudspeaker: "}[typ]
	if !ok {
		return nil
	}
	if typ == "ask" {
		if cid := textField(message, "correlation_id"); cid != "" {
			text += "\n[ask #" + shortID(cid) + "]"
		}
	}
	return b.send(ctx, prefix+"*@"+escapeSlack(from)+"*\n"+escapeSlack(text), nil)
}

func (b *Slack) socketLoop(ctx context.Context) error {
	backoff := time.Second
	for ctx.Err() == nil {
		connected, err := b.socketOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if connected {
			backoff = time.Second
		}
		fmt.Printf("slack: socket lost: %v; reconnecting in %s\n", err, backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return nil
}

func (b *Slack) socketOnce(ctx context.Context) (bool, error) {
	var opened struct {
		URL string `json:"url"`
	}
	if err := b.api(ctx, "apps.connections.open", b.appToken, map[string]any{}, &opened); err != nil {
		return false, err
	}
	conn, _, err := websocket.Dial(ctx, opened.URL, nil)
	if err != nil {
		return false, err
	}
	defer conn.CloseNow()
	for {
		var envelope map[string]any
		if err := wsjson.Read(ctx, conn, &envelope); err != nil {
			return true, err
		}
		if id := textField(envelope, "envelope_id"); id != "" {
			if err := wsjson.Write(ctx, conn, map[string]string{"envelope_id": id}); err != nil {
				return true, err
			}
		}
		switch textField(envelope, "type") {
		case "events_api":
			payload, _ := envelope["payload"].(map[string]any)
			event, _ := payload["event"].(map[string]any)
			if err := b.onEvent(ctx, event); err != nil {
				fmt.Printf("slack: event failed: %v\n", err)
			}
		case "interactive":
			payload, _ := envelope["payload"].(map[string]any)
			if err := b.onInteraction(ctx, payload); err != nil {
				fmt.Printf("slack: interaction failed: %v\n", err)
			}
		case "disconnect":
			return true, fmt.Errorf("Slack requested reconnect")
		}
	}
}

func (b *Slack) onEvent(ctx context.Context, event map[string]any) error {
	if textField(event, "type") != "message" || textField(event, "channel") != b.channel || textField(event, "bot_id") != "" || event["subtype"] != nil {
		return nil
	}
	text := strings.TrimSpace(textField(event, "text"))
	if text == "" {
		return nil
	}
	return b.onText(ctx, text)
}

func (b *Slack) onInteraction(ctx context.Context, payload map[string]any) error {
	actions, _ := payload["actions"].([]any)
	if len(actions) == 0 {
		return nil
	}
	action, _ := actions[0].(map[string]any)
	value := textField(action, "value")
	if strings.HasPrefix(value, "target:") {
		b.setTarget(strings.TrimPrefix(value, "target:"))
		return b.send(ctx, "Now talking to *@"+escapeSlack(b.targetName())+"*.", nil)
	}
	if value == "clear" {
		b.setTarget("")
		return b.send(ctx, "Cleared. No active conversation.", nil)
	}
	if value == "peers" {
		return b.sendPeers(ctx)
	}
	return nil
}

func (b *Slack) onText(ctx context.Context, text string) error {
	lower := strings.ToLower(text)
	switch {
	case lower == "peers" || lower == "list":
		return b.sendPeers(ctx)
	case lower == "clear":
		b.setTarget("")
		return b.send(ctx, "Cleared. No active conversation.", nil)
	case strings.HasPrefix(lower, "select ") || strings.HasPrefix(lower, "switch "):
		parts := strings.SplitN(text, " ", 2)
		b.setTarget(strings.TrimPrefix(strings.TrimSpace(parts[1]), "@"))
		return b.send(ctx, "Now talking to *@"+escapeSlack(b.targetName())+"*.", nil)
	}
	mode, target, message, targeted := parseTargeted(text, false)
	if !targeted {
		mode, target, message = "ask", b.targetName(), text
	}
	if target == "" {
		target = b.targetName()
	}
	if target == "" {
		target = b.daemon.DefaultTarget(ctx)
	}
	if target == "" {
		return b.send(ctx, "No active conversation. Use `peers` or `@name message`.", nil)
	}
	if message == "" {
		return b.send(ctx, "Message text is required.", nil)
	}
	b.setTarget(target)
	result, err := b.daemon.Send(ctx, target, message, mode, nil)
	if err != nil {
		return b.send(ctx, ":x: "+escapeSlack(err.Error()), nil)
	}
	if mode == "ask" {
		cid := textField(result, "correlation_id")
		suffix := ""
		if cid != "" {
			suffix = " `#" + shortID(cid) + "`"
		}
		return b.send(ctx, ":question: → *@"+escapeSlack(target)+"*"+suffix, nil)
	}
	return b.send(ctx, ":white_check_mark: → *@"+escapeSlack(target)+"*", nil)
}

func (b *Slack) sendPeers(ctx context.Context) error {
	peers, err := b.daemon.ActivePeers(ctx)
	if err != nil {
		return b.send(ctx, "Error listing peers: "+escapeSlack(err.Error()), nil)
	}
	if len(peers) == 0 {
		return b.send(ctx, "No peers online.", nil)
	}
	blocks := make([]any, 0, len(peers))
	names := make([]string, 0, len(peers))
	for _, peer := range peers {
		name, target := textField(peer, "display_name"), peerTarget(peer)
		names = append(names, name)
		line := ":large_green_circle: *" + escapeSlack(name) + "*"
		if description := textField(peer, "description"); description != "" {
			line += "\n_" + escapeSlack(description) + "_"
		}
		blocks = append(blocks, map[string]any{
			"type": "section", "text": map[string]string{"type": "mrkdwn", "text": line},
			"accessory": map[string]string{"type": "button", "text": "💬 " + name, "value": "target:" + target, "action_id": "select_" + name},
		})
	}
	return b.send(ctx, "Online: "+strings.Join(names, ", "), blocks)
}

func (b *Slack) setTarget(target string) {
	b.mu.Lock()
	b.target = target
	b.mu.Unlock()
}

func (b *Slack) targetName() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.target
}

func escapeSlack(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	return strings.ReplaceAll(text, ">", "&gt;")
}
