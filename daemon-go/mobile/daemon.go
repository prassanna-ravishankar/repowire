// Package mobile bridges Telegram and Slack to a local Repowire daemon.
package mobile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type DaemonPeer struct {
	baseURL  string
	auth     string
	path     string
	circle   string
	http     *http.Client
	identity sync.RWMutex
	name     string
	peerID   string
}

func NewDaemonPeer(baseURL, auth, name, path, circle string) *DaemonPeer {
	return &DaemonPeer{
		baseURL: strings.TrimRight(baseURL, "/"), auth: auth, name: name,
		path: path, circle: circle, http: &http.Client{Timeout: 20 * time.Second},
	}
}

func (p *DaemonPeer) Name() string {
	p.identity.RLock()
	defer p.identity.RUnlock()
	return p.name
}

func (p *DaemonPeer) PeerID() string {
	p.identity.RLock()
	defer p.identity.RUnlock()
	return p.peerID
}

func (p *DaemonPeer) setIdentity(name, peerID string) {
	p.identity.Lock()
	if name != "" {
		p.name = name
	}
	if peerID != "" {
		p.peerID = peerID
	}
	p.identity.Unlock()
}

func (p *DaemonPeer) wsURL() string {
	u, _ := url.Parse(p.baseURL)
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	return u.String()
}

func (p *DaemonPeer) Run(ctx context.Context, onMessage func(context.Context, map[string]any) error) error {
	backoff := time.Second
	for ctx.Err() == nil {
		connected, err := p.runOnce(ctx, onMessage)
		if ctx.Err() != nil {
			return nil
		}
		if connected {
			backoff = time.Second
		}
		if err != nil {
			fmt.Printf("mobile: daemon connection lost: %v; reconnecting in %s\n", err, backoff)
		}
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

func (p *DaemonPeer) runOnce(ctx context.Context, onMessage func(context.Context, map[string]any) error) (bool, error) {
	conn, _, err := websocket.Dial(ctx, p.wsURL(), nil)
	if err != nil {
		return false, err
	}
	defer conn.CloseNow()
	connect := map[string]any{
		"type": "connect", "display_name": p.Name(), "circle": p.circle,
		"backend": "claude-code", "role": "service", "path": p.path,
	}
	if p.auth != "" {
		connect["auth_token"] = p.auth
	}
	if peerID := p.PeerID(); peerID != "" {
		connect["peer_id"] = peerID
	}
	if err := wsjson.Write(ctx, conn, connect); err != nil {
		return false, err
	}
	var connected map[string]any
	if err := wsjson.Read(ctx, conn, &connected); err != nil {
		return false, err
	}
	if connected["type"] != "connected" {
		return false, fmt.Errorf("daemon rejected connection: %v", connected)
	}
	p.setIdentity(textField(connected, "display_name"), textField(connected, "session_id"))
	for {
		var message map[string]any
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			return true, err
		}
		if message["type"] == "ping" {
			if err := wsjson.Write(ctx, conn, map[string]string{"type": "pong"}); err != nil {
				return true, err
			}
			continue
		}
		if err := onMessage(ctx, message); err != nil {
			fmt.Printf("mobile: delivery failed: %v\n", err)
		}
	}
}

func (p *DaemonPeer) JSON(ctx context.Context, method, path string, input, output any) (*http.Response, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.auth != "" {
		req.Header.Set("Authorization", "Bearer "+p.auth)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if output != nil {
		if err := json.NewDecoder(resp.Body).Decode(output); err != nil && resp.StatusCode < 300 {
			return resp, err
		}
	}
	return resp, nil
}

func (p *DaemonPeer) Send(ctx context.Context, target, message, mode string, attachments []map[string]any) (map[string]any, error) {
	payload := map[string]any{"from_peer": p.Name(), "to_peer": target, "text": message}
	if len(attachments) > 0 {
		payload["attachments"] = attachments
	}
	var result map[string]any
	resp, err := p.JSON(ctx, http.MethodPost, "/"+mode, payload, &result)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return result, fmt.Errorf("daemon returned %s: %s", resp.Status, textField(result, "detail"))
	}
	return result, nil
}

func (p *DaemonPeer) UploadAttachment(ctx context.Context, filename, contentType string, content []byte) (map[string]any, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(content); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/attachments", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if p.auth != "" {
		req.Header.Set("Authorization", "Bearer "+p.auth)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upload attachment: %s", resp.Status)
	}
	if contentType != "" && result["content_type"] == nil {
		result["content_type"] = contentType
	}
	return result, nil
}

func (p *DaemonPeer) DownloadAttachment(ctx context.Context, id string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/attachments/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	if p.auth != "" {
		req.Header.Set("Authorization", "Bearer "+p.auth)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download attachment: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

func (p *DaemonPeer) Peers(ctx context.Context) ([]map[string]any, error) {
	var result struct {
		Peers []map[string]any `json:"peers"`
	}
	resp, err := p.JSON(ctx, http.MethodGet, "/peers", nil, &result)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list peers: %s", resp.Status)
	}
	return result.Peers, nil
}

func (p *DaemonPeer) ActivePeers(ctx context.Context) ([]map[string]any, error) {
	peers, err := p.Peers(ctx)
	if err != nil {
		return nil, err
	}
	selfName, selfID := p.Name(), p.PeerID()
	active := make([]map[string]any, 0, len(peers))
	for _, peer := range peers {
		status := textField(peer, "status")
		if status != "online" && status != "busy" {
			continue
		}
		if textField(peer, "peer_id") == selfID || textField(peer, "display_name") == selfName {
			continue
		}
		active = append(active, peer)
	}
	return active, nil
}

func (p *DaemonPeer) DefaultTarget(ctx context.Context) string {
	peers, err := p.ActivePeers(ctx)
	if err != nil {
		return ""
	}
	for _, peer := range peers {
		if textField(peer, "role") == "orchestrator" || strings.HasPrefix(textField(peer, "display_name"), "orchestrator-") {
			return peerTarget(peer)
		}
	}
	return ""
}

func runTogether(ctx context.Context, first, second func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- first(ctx) }()
	go func() { done <- second(ctx) }()
	err := <-done
	cancel()
	<-done
	return err
}

func textField(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return text
}

func peerTarget(peer map[string]any) string {
	if id := textField(peer, "peer_id"); id != "" {
		return id
	}
	return textField(peer, "display_name")
}

func parseTargeted(text string, slash bool) (mode, target, message string, ok bool) {
	trimmed := strings.TrimSpace(text)
	command := trimmed
	if slash {
		command = strings.TrimPrefix(command, "/")
	}
	lower := strings.ToLower(command)
	for _, prefix := range []string{"notify", "fyi"} {
		if lower == prefix || strings.HasPrefix(lower, prefix+" ") {
			body := strings.TrimSpace(command[len(prefix):])
			if strings.HasPrefix(body, "@") {
				parts := strings.SplitN(body, " ", 2)
				target = strings.TrimPrefix(parts[0], "@")
				if len(parts) == 2 {
					body = strings.TrimSpace(parts[1])
				} else {
					body = ""
				}
			}
			return "notify", target, body, true
		}
	}
	if strings.HasPrefix(trimmed, "@") {
		parts := strings.SplitN(trimmed, " ", 2)
		if len(parts) == 2 {
			return "ask", strings.TrimPrefix(parts[0], "@"), strings.TrimSpace(parts[1]), true
		}
	}
	return "", "", "", false
}
