// Package codexbridge connects live Codex App Server threads to the Repowire
// mesh. Codex owns thread execution; this process only translates lifecycle and
// delivery frames between the two local protocols.
package codexbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/proto"
)

const defaultCircle = "default"

var invalidName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

type rpcReply struct {
	result json.RawMessage
	err    error
}

type Bridge struct {
	ctx        context.Context
	version    string
	app        *websocket.Conn
	appWrite   sync.Mutex
	nextID     atomic.Int64
	rpcMu      sync.Mutex
	pending    map[int64]chan rpcReply
	threadsMu  sync.Mutex
	threads    map[string]*threadPeer
	hintMu     sync.Mutex
	daemonHTTP string
	daemonWS   string
	token      string
	boundary   proto.CircleBoundary
}

type threadPeer struct {
	bridge      *Bridge
	id          string
	cwd         string
	circle      string
	circleSrc   string
	role        string
	hintedID    string
	peerID      string
	displayName string
	activeTurn  string
	busy        bool
	mu          sync.Mutex
	writeMu     sync.Mutex
	conn        *websocket.Conn
	cancel      context.CancelFunc
}

// Run keeps the bridge attached until the service is stopped or App Server
// exits. The service manager owns restart/backoff for process-level failures.
func Run(ctx context.Context, version string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	host := cfg.Daemon.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	address := net.JoinHostPort(host, fmt.Sprint(cfg.Daemon.Port))
	b := &Bridge{
		ctx: ctx, version: version, pending: map[int64]chan rpcReply{},
		threads: map[string]*threadPeer{}, daemonHTTP: "http://" + address,
		daemonWS: "ws://" + address + "/ws", token: cfg.Daemon.AuthToken, boundary: cfg.Daemon.CircleBoundary,
	}

	conn, child, err := ensureAppServer(ctx)
	if err != nil {
		return err
	}
	b.app = conn
	defer conn.CloseNow()
	if child != nil {
		defer func() { _ = child.Process.Signal(os.Interrupt) }()
	}

	readErr := make(chan error, 1)
	go func() { readErr <- b.readApp() }()
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := b.call(initCtx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "repowire", "title": "Repowire", "version": version},
	}); err != nil {
		return fmt.Errorf("initialize Codex App Server: %w", err)
	}
	if err := b.notify(ctx, "initialized", map[string]any{}); err != nil {
		return fmt.Errorf("acknowledge Codex App Server initialization: %w", err)
	}
	if err := b.loadExistingThreads(ctx); err != nil {
		log.Printf("codex bridge: loaded-thread reconciliation: %v", err)
	}
	log.Printf("codex bridge connected to App Server at %s", appServerSocket())

	select {
	case <-ctx.Done():
		return nil
	case err := <-readErr:
		return fmt.Errorf("Codex App Server connection closed: %w", err)
	}
}

func ensureAppServer(ctx context.Context) (*websocket.Conn, *exec.Cmd, error) {
	if conn, err := dialAppServer(ctx); err == nil {
		return conn, nil, nil
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		return nil, nil, errors.New("codex executable not found")
	}
	if err := os.MkdirAll(filepath.Dir(appServerSocket()), 0o700); err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(codex, "app-server", "--listen", "unix://")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start Codex App Server: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if conn, err := dialAppServer(ctx); err == nil {
			return conn, cmd, nil
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return nil, nil, ctx.Err()
		case <-deadline.C:
			_ = cmd.Process.Kill()
			return nil, nil, errors.New("Codex App Server did not create its control socket within 10s")
		case <-tick.C:
		}
	}
}

func appServerSocket() string {
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".codex")
	}
	return filepath.Join(root, "app-server-control", "app-server-control.sock")
}

func dialAppServer(ctx context.Context) (*websocket.Conn, error) {
	dialer := &net.Dialer{Timeout: time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", appServerSocket())
	}}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: client})
	return conn, err
}

func (b *Bridge) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := b.nextID.Add(1)
	response := make(chan rpcReply, 1)
	b.rpcMu.Lock()
	b.pending[id] = response
	b.rpcMu.Unlock()
	defer func() {
		b.rpcMu.Lock()
		delete(b.pending, id)
		b.rpcMu.Unlock()
	}()
	b.appWrite.Lock()
	err := wsjson.Write(ctx, b.app, map[string]any{"id": id, "method": method, "params": params})
	b.appWrite.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case reply := <-response:
		return reply.result, reply.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *Bridge) notify(ctx context.Context, method string, params any) error {
	b.appWrite.Lock()
	defer b.appWrite.Unlock()
	return wsjson.Write(ctx, b.app, map[string]any{"method": method, "params": params})
}

func (b *Bridge) readApp() error {
	for {
		var message map[string]any
		if err := wsjson.Read(b.ctx, b.app, &message); err != nil {
			b.failPending(err)
			return err
		}
		if id, ok := number(message["id"]); ok && (message["result"] != nil || message["error"] != nil) {
			b.rpcMu.Lock()
			ch := b.pending[id]
			b.rpcMu.Unlock()
			if ch != nil {
				if value := message["error"]; value != nil {
					ch <- rpcReply{err: fmt.Errorf("%v", value)}
				} else {
					raw, _ := json.Marshal(message["result"])
					ch <- rpcReply{result: raw}
				}
			}
			continue
		}
		if method, _ := message["method"].(string); method != "" {
			params, _ := message["params"].(map[string]any)
			b.handleNotification(method, params)
		}
	}
}

func (b *Bridge) failPending(err error) {
	b.rpcMu.Lock()
	defer b.rpcMu.Unlock()
	for _, ch := range b.pending {
		select {
		case ch <- rpcReply{err: err}:
		default:
		}
	}
}

func (b *Bridge) loadExistingThreads(ctx context.Context) error {
	raw, err := b.call(ctx, "thread/loaded/list", map[string]any{})
	if err != nil {
		return err
	}
	var list struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	for _, id := range list.Data {
		threadRaw, err := b.call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": true})
		if err != nil {
			log.Printf("codex bridge: read loaded thread %s: %v", id, err)
			continue
		}
		var result map[string]any
		_ = json.Unmarshal(threadRaw, &result)
		if thread, ok := result["thread"].(map[string]any); ok {
			b.ensureThread(thread)
		}
	}
	return nil
}

func (b *Bridge) handleNotification(method string, params map[string]any) {
	switch method {
	case "thread/started":
		if thread, ok := params["thread"].(map[string]any); ok {
			b.ensureThread(thread)
		}
	case "thread/status/changed":
		if p := b.thread(stringValue(params, "threadId")); p != nil {
			p.setBusy(statusType(params["status"]) == "active")
		}
	case "turn/started":
		if p := b.thread(stringValue(params, "threadId")); p != nil {
			turn, _ := params["turn"].(map[string]any)
			p.setActiveTurn(stringValue(turn, "id"))
		}
	case "turn/completed":
		if p := b.thread(stringValue(params, "threadId")); p != nil {
			p.setActiveTurn("")
		}
	case "item/completed":
		b.postChatItem(params)
	case "thread/closed", "thread/deleted":
		b.closeThread(stringValue(params, "threadId"))
	}
}

func (b *Bridge) ensureThread(thread map[string]any) {
	id, cwd := stringValue(thread, "id"), stringValue(thread, "cwd")
	if id == "" || cwd == "" {
		return
	}
	b.threadsMu.Lock()
	if existing := b.threads[id]; existing != nil {
		b.threadsMu.Unlock()
		return
	}
	b.hintMu.Lock()
	hint := hooks.ConsumeSpawnHint(cwd, "codex")
	b.hintMu.Unlock()
	circle, source := defaultCircle, "fallback"
	if tmuxCircle, tmuxSource := threadTmuxPlacement(cwd, b.boundary); tmuxCircle != "" {
		circle, source = tmuxCircle, tmuxSource
	}
	if configured := os.Getenv("REPOWIRE_CODEX_CIRCLE"); configured != "" {
		circle, source = configured, "fallback"
	}
	role, hintedID := "agent", ""
	if hint != nil {
		if value := stringValue(hint, "circle"); value != "" {
			circle, source = value, "spawn_hint"
		}
		if value := stringValue(hint, "role"); value != "" {
			role = value
		}
		hintedID = stringValue(hint, "peer_id")
	}
	peerCtx, cancel := context.WithCancel(b.ctx)
	p := &threadPeer{bridge: b, id: id, cwd: cwd, circle: circle, circleSrc: source, role: role, hintedID: hintedID, cancel: cancel}
	p.busy = statusType(thread["status"]) == "active"
	p.activeTurn = activeTurn(thread)
	b.threads[id] = p
	b.threadsMu.Unlock()
	go p.runMesh(peerCtx)
}

func activeTurn(thread map[string]any) string {
	turns, _ := thread["turns"].([]any)
	for i := len(turns) - 1; i >= 0; i-- {
		turn, _ := turns[i].(map[string]any)
		if stringValue(turn, "status") == "inProgress" {
			return stringValue(turn, "id")
		}
	}
	return ""
}

func (b *Bridge) thread(id string) *threadPeer {
	b.threadsMu.Lock()
	defer b.threadsMu.Unlock()
	return b.threads[id]
}

func (b *Bridge) closeThread(id string) {
	b.threadsMu.Lock()
	p := b.threads[id]
	delete(b.threads, id)
	b.threadsMu.Unlock()
	if p != nil {
		p.cancel()
	}
}

func (p *threadPeer) runMesh(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		conn, err := p.connectMesh(ctx)
		if err != nil {
			failures++
			log.Printf("codex bridge: connect thread %s: %v", p.id, err)
			if !wait(ctx, backoff(failures)) {
				return
			}
			continue
		}
		failures = 0
		p.mu.Lock()
		p.conn = conn
		p.mu.Unlock()
		err = p.readMesh(ctx, conn)
		p.mu.Lock()
		if p.conn == conn {
			p.conn = nil
		}
		p.mu.Unlock()
		_ = conn.CloseNow()
		if ctx.Err() == nil {
			log.Printf("codex bridge: mesh connection for %s closed: %v", p.id, err)
			if !wait(ctx, time.Second) {
				return
			}
		}
	}
}

func (p *threadPeer) connectMesh(ctx context.Context) (*websocket.Conn, error) {
	if err := p.register(ctx); err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, p.bridge.daemonWS, nil)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	peerID, displayName, busy := p.peerID, p.displayName, p.busy
	p.mu.Unlock()
	connect := map[string]any{
		"type": "connect", "display_name": displayName, "circle": p.circle,
		"circle_source": p.circleSrc, "backend": "codex", "path": p.cwd, "role": p.role,
		"peer_id": peerID, "capabilities": []string{"delivery_receipts", "thread_steering"},
	}
	if p.bridge.token != "" {
		connect["auth_token"] = p.bridge.token
	}
	if err := wsjson.Write(ctx, conn, connect); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	var response map[string]any
	if err := wsjson.Read(ctx, conn, &response); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	if stringValue(response, "type") != "connected" {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("mesh rejected thread: %v", response)
	}
	p.mu.Lock()
	p.peerID, p.displayName = stringValue(response, "session_id"), stringValue(response, "display_name")
	p.mu.Unlock()
	status := "online"
	turnState := "idle"
	if busy {
		status, turnState = "busy", "working"
	}
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "status", "status": status, "turn_state": turnState}); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	log.Printf("codex bridge: registered %s as %s", p.id, p.displayName)
	return conn, nil
}

func (p *threadPeer) register(ctx context.Context) error {
	p.mu.Lock()
	claim := p.peerID
	if claim == "" {
		claim = p.hintedID
	}
	p.mu.Unlock()
	body := map[string]any{
		"name": safeName(filepath.Base(p.cwd)), "path": p.cwd, "circle": p.circle,
		"circle_source": p.circleSrc, "backend": "codex", "role": p.role,
		"metadata": map[string]any{
			"runtime_session_id": p.id, "runtime_source_uri": "codex:" + p.id,
			"transport": "codex-app-server", "capabilities": []string{"delivery_receipts", "thread_steering"},
		},
	}
	if claim != "" {
		body["peer_id"] = claim
	}
	result, err := p.bridge.daemonRequest(ctx, http.MethodPost, "/peers", body)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.peerID, p.displayName = stringValue(result, "peer_id"), stringValue(result, "display_name")
	p.mu.Unlock()
	if p.peerID == "" {
		return errors.New("daemon registration returned no peer_id")
	}
	return nil
}

func (p *threadPeer) readMesh(ctx context.Context, conn *websocket.Conn) error {
	for {
		var frame map[string]any
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			return err
		}
		switch stringValue(frame, "type") {
		case "ping":
			p.writeMesh(ctx, conn, map[string]any{"type": "pong", "ping_id": frame["ping_id"]})
		case "ask", "notify", "broadcast":
			p.inject(ctx, conn, frame)
		}
	}
}

func (p *threadPeer) inject(ctx context.Context, conn *websocket.Conn, frame map[string]any) {
	text := stringValue(frame, "text")
	deliveryID := stringValue(frame, "delivery_id")
	if text == "" {
		p.deliveryAck(ctx, conn, deliveryID, stringValue(frame, "type"), "failed", "empty delivery")
		return
	}
	p.mu.Lock()
	activeTurn := p.activeTurn
	p.mu.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	input := []any{map[string]any{"type": "text", "text": text}}
	var err error
	if activeTurn != "" {
		_, err = p.bridge.call(callCtx, "turn/steer", map[string]any{"threadId": p.id, "expectedTurnId": activeTurn, "input": input})
		if err != nil {
			_, err = p.bridge.call(callCtx, "turn/start", map[string]any{"threadId": p.id, "input": input})
		}
	} else {
		_, err = p.bridge.call(callCtx, "turn/start", map[string]any{"threadId": p.id, "input": input})
	}
	if err != nil {
		p.deliveryAck(ctx, conn, deliveryID, stringValue(frame, "type"), "failed", err.Error())
		return
	}
	p.deliveryAck(ctx, conn, deliveryID, stringValue(frame, "type"), "accepted", "codex thread API")
}

func (p *threadPeer) deliveryAck(ctx context.Context, conn *websocket.Conn, deliveryID, kind, status, detail string) {
	if deliveryID == "" {
		return
	}
	p.writeMesh(ctx, conn, map[string]any{"type": "delivery_ack", "delivery_id": deliveryID, "message_type": kind, "status": status, "detail": detail})
}

func (p *threadPeer) writeMesh(ctx context.Context, conn *websocket.Conn, value any) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := wsjson.Write(ctx, conn, value); err != nil {
		log.Printf("codex bridge: write mesh frame for %s: %v", p.id, err)
	}
}

func (p *threadPeer) setBusy(busy bool) {
	p.mu.Lock()
	p.busy = busy
	conn := p.conn
	p.mu.Unlock()
	if conn != nil {
		status, turnState := "online", "idle"
		if busy {
			status, turnState = "busy", "working"
		}
		p.writeMesh(p.bridge.ctx, conn, map[string]any{"type": "status", "status": status, "turn_state": turnState})
	}
}

func (p *threadPeer) setActiveTurn(turnID string) {
	p.mu.Lock()
	p.activeTurn = turnID
	p.mu.Unlock()
	p.setBusy(turnID != "")
}

func (b *Bridge) postChatItem(params map[string]any) {
	p := b.thread(stringValue(params, "threadId"))
	item, _ := params["item"].(map[string]any)
	if p == nil || item == nil {
		return
	}
	role, text := "", ""
	switch stringValue(item, "type") {
	case "userMessage":
		role, text = "user", userText(item["content"])
	case "agentMessage":
		phase := stringValue(item, "phase")
		if phase == "commentary" {
			return
		}
		role, text = "assistant", stringValue(item, "text")
	}
	if strings.TrimSpace(text) == "" {
		return
	}
	p.mu.Lock()
	peerID, displayName := p.peerID, p.displayName
	p.mu.Unlock()
	if peerID == "" {
		return
	}
	body := map[string]any{"peer": displayName, "peer_id": peerID, "role": role, "text": text, "session_id": p.id, "turn_id": stringValue(params, "turnId")}
	postCtx, cancel := context.WithTimeout(b.ctx, 3*time.Second)
	defer cancel()
	if _, err := b.daemonRequest(postCtx, http.MethodPost, "/events/chat", body); err != nil {
		log.Printf("codex bridge: post chat item for %s: %v", p.id, err)
	}
}

func userText(value any) string {
	items, _ := value.([]any)
	var text []string
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if stringValue(item, "type") == "text" && stringValue(item, "text") != "" {
			text = append(text, stringValue(item, "text"))
		}
	}
	return strings.Join(text, "\n")
}

func (b *Bridge) daemonRequest(ctx context.Context, method, path string, body any) (map[string]any, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, b.daemonHTTP+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var result map[string]any
	_ = json.Unmarshal(payload, &result)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	return result, nil
}

func safeName(value string) string {
	value = strings.Trim(invalidName.ReplaceAllString(value, "-"), "-.")
	if value == "" {
		value = "codex"
	}
	if len(value) > 64 {
		value = value[:64]
	}
	return value
}

func threadTmuxPlacement(cwd string, boundary proto.CircleBoundary) (string, string) {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_current_path}\t#{session_name}\t#{window_id}\t#{pane_pid}").Output()
	if err != nil {
		return "", ""
	}
	processes, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return "", ""
	}
	return parseTmuxPlacement(string(out), string(processes), cwd, boundary)
}

func parseTmuxPlacement(output, processes, cwd string, boundary proto.CircleBoundary) (string, string) {
	want, _ := filepath.Abs(cwd)
	circles := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			continue
		}
		path, _ := filepath.Abs(fields[0])
		if filepath.Clean(path) != filepath.Clean(want) {
			continue
		}
		root, err := strconv.Atoi(fields[3])
		if err != nil || !processTreeHasCodex(processes, root) {
			continue
		}
		candidate := proto.TmuxCircle(boundary, fields[1], fields[2])
		if candidate == "" {
			continue
		}
		circles[candidate] = true
	}
	if len(circles) != 1 {
		return "", ""
	}
	var circle string
	for value := range circles {
		circle = value
	}
	if boundary == proto.CircleBoundaryWindow {
		return circle, "tmux_window"
	}
	return circle, "tmux"
}

func processTreeHasCodex(output string, root int) bool {
	children := map[int][]int{}
	commands := map[int]string{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		if pidErr != nil || parentErr != nil {
			continue
		}
		children[parent] = append(children[parent], pid)
		commands[pid] = strings.ToLower(strings.Join(fields[2:], " "))
	}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if strings.Contains(commands[pid], "/codex") {
			return true
		}
		queue = append(queue, children[pid]...)
	}
	return false
}

func stringValue(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	value, _ := m[key].(string)
	return value
}

func statusType(value any) string {
	status, _ := value.(map[string]any)
	return stringValue(status, "type")
}

func number(value any) (int64, bool) {
	switch value := value.(type) {
	case float64:
		return int64(value), true
	case int64:
		return value, true
	case json.Number:
		n, err := value.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

func backoff(failures int) time.Duration {
	if failures > 5 {
		failures = 5
	}
	return time.Duration(1<<failures) * 100 * time.Millisecond
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
