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

const (
	defaultCircle      = "default"
	appServerReadLimit = 16 << 20
)

var invalidName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
var providerEnvKey = regexp.MustCompile(`(?m)^\s*env_key\s*=\s*["']([A-Za-z_][A-Za-z0-9_]*)["']`)

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
	bridge        *Bridge
	id            string
	cwd           string
	circle        string
	circleSrc     string
	role          string
	hintedID      string
	birthCert     map[string]any
	peerID        string
	displayName   string
	model         string
	branch        string
	gitStatus     map[string]any
	tmux          map[string]any
	activeTurn    string
	busy          bool
	lastUser      string
	lastAnswer    string
	toolCalls     map[string][]map[string]string
	seenItems     map[string]map[string]bool
	completedTurn string
	mu            sync.Mutex
	writeMu       sync.Mutex
	conn          *websocket.Conn
	cancel        context.CancelFunc
}

// Run keeps the bridge attached until the service is stopped. App Server
// connection failures are repaired here so they cannot terminate live TUIs.
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

	return b.runAppServer(ensureAppServer)
}

func (b *Bridge) runAppServer(connect func(context.Context) (*websocket.Conn, *exec.Cmd, error)) error {
	var child *exec.Cmd
	defer func() {
		if child != nil {
			_ = child.Process.Signal(os.Interrupt)
		}
	}()
	failures := 0
	for {
		conn, started, err := connect(b.ctx)
		if err != nil {
			if child == nil {
				return err
			}
			failures++
			log.Printf("codex bridge: reconnect to App Server: %v", err)
			if !wait(b.ctx, backoff(failures)) {
				return nil
			}
			continue
		}
		if started != nil {
			child = started
		}
		failures = 0
		err = b.serveApp(conn)
		b.clearApp(conn)
		if b.ctx.Err() != nil {
			return nil
		}
		failures++
		log.Printf("codex bridge: App Server connection closed, reconnecting: %v", err)
		if !wait(b.ctx, backoff(failures)) {
			return nil
		}
	}
}

func (b *Bridge) serveApp(conn *websocket.Conn) error {
	b.appWrite.Lock()
	b.app = conn
	b.appWrite.Unlock()
	readErr := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		readErr <- b.readApp(conn)
	}()
	defer func() {
		_ = conn.CloseNow()
		<-readerDone
	}()
	initCtx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
	defer cancel()
	if _, err := b.call(initCtx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "repowire", "title": "Repowire", "version": b.version},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		return fmt.Errorf("initialize Codex App Server: %w", err)
	}
	if err := b.notify(b.ctx, "initialized", map[string]any{}); err != nil {
		return fmt.Errorf("acknowledge Codex App Server initialization: %w", err)
	}
	if err := b.loadExistingThreads(b.ctx); err != nil {
		log.Printf("codex bridge: loaded-thread reconciliation: %v", err)
	}
	log.Printf("codex bridge connected to App Server at %s", appServerSocket())

	select {
	case <-b.ctx.Done():
		return nil
	case err := <-readErr:
		return err
	}
}

func (b *Bridge) clearApp(conn *websocket.Conn) {
	b.appWrite.Lock()
	defer b.appWrite.Unlock()
	if b.app == conn {
		b.app = nil
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
	cmd.Env = codexChildEnvironment(ctx)
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

func codexChildEnvironment(ctx context.Context) []string {
	env := os.Environ()
	keys := configuredProviderEnvKeys()
	var missing []string
	for _, key := range keys {
		if os.Getenv(key) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) == 0 {
		return env
	}
	snapshotCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	shell := os.Getenv("SHELL")
	if shell == "" {
		for _, candidate := range []string{"/bin/zsh", "/bin/bash"} {
			if _, err := os.Stat(candidate); err == nil {
				shell = candidate
				break
			}
		}
	}
	if shell == "" {
		log.Printf("codex bridge: provider environment unavailable for %s: no login shell", strings.Join(missing, ", "))
		return env
	}
	out, err := exec.CommandContext(snapshotCtx, shell, "-l", "-i", "-c", "env").Output()
	if err != nil {
		log.Printf("codex bridge: provider environment unavailable for %s: %v", strings.Join(missing, ", "), err)
		return env
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	for _, key := range missing {
		if value := values[key]; value != "" {
			env = append(env, key+"="+value)
		} else {
			log.Printf("codex bridge: provider environment %s is not set; rerun setup from a configured shell", key)
		}
	}
	return env
}

func configuredProviderEnvKeys() []string {
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".codex")
	}
	raw, err := os.ReadFile(filepath.Join(root, "config.toml"))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var keys []string
	for _, match := range providerEnvKey.FindAllSubmatch(raw, -1) {
		key := string(match[1])
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys
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
	if b.app == nil {
		b.appWrite.Unlock()
		return nil, errors.New("Codex App Server is reconnecting")
	}
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
	if b.app == nil {
		return errors.New("Codex App Server is reconnecting")
	}
	return wsjson.Write(ctx, b.app, map[string]any{"method": method, "params": params})
}

func (b *Bridge) readApp(conn *websocket.Conn) error {
	conn.SetReadLimit(appServerReadLimit)
	for {
		var message map[string]any
		if err := wsjson.Read(b.ctx, conn, &message); err != nil {
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
		if err := b.loadThread(ctx, id); err != nil {
			log.Printf("codex bridge: read loaded thread %s: %v", id, err)
		}
	}
	return nil
}

func (b *Bridge) loadThread(ctx context.Context, id string) error {
	if id == "" || b.thread(id) != nil {
		return nil
	}
	threadRaw, err := b.call(ctx, "thread/read", map[string]any{"threadId": id})
	if err != nil {
		return err
	}
	var result map[string]any
	if err := json.Unmarshal(threadRaw, &result); err != nil {
		return err
	}
	thread, ok := result["thread"].(map[string]any)
	if !ok {
		return errors.New("thread/read returned no thread")
	}
	b.ensureThread(thread)
	return nil
}

func (b *Bridge) handleNotification(method string, params map[string]any) {
	switch method {
	case "thread/started":
		if thread, ok := params["thread"].(map[string]any); ok {
			b.ensureThread(thread)
		}
	case "thread/status/changed":
		id := stringValue(params, "threadId")
		if p := b.thread(id); p != nil {
			active := statusType(params["status"]) == "active"
			p.setBusy(active)
			if !active {
				go b.captureCompletedTurn(p.id)
			}
		} else if id != "" {
			go func() {
				ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
				defer cancel()
				if err := b.loadThread(ctx, id); err != nil {
					log.Printf("codex bridge: discover resumed thread %s: %v", id, err)
				}
			}()
		}
	case "turn/started":
		if p := b.thread(stringValue(params, "threadId")); p != nil {
			turn, _ := params["turn"].(map[string]any)
			p.setActiveTurn(stringValue(turn, "id"))
		}
	case "turn/completed":
		if p := b.thread(stringValue(params, "threadId")); p != nil {
			turn, _ := params["turn"].(map[string]any)
			b.finishTurn(p, turn)
		}
	case "item/completed":
		b.postChatItem(params)
	case "thread/closed", "thread/deleted":
		b.closeThread(stringValue(params, "threadId"))
	}
}

func (b *Bridge) captureCompletedTurn(threadID string) {
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()
	raw, err := b.call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true})
	if err != nil {
		log.Printf("codex bridge: reconcile completed turn for %s: %v", threadID, err)
		return
	}
	var result map[string]any
	if json.Unmarshal(raw, &result) != nil {
		return
	}
	thread, _ := result["thread"].(map[string]any)
	turns := mapSlice(thread["turns"])
	for i := len(turns) - 1; i >= 0; i-- {
		if stringValue(turns[i], "status") == "completed" {
			if p := b.thread(threadID); p != nil {
				b.finishTurn(p, turns[i])
			}
			return
		}
	}
}

func (b *Bridge) finishTurn(p *threadPeer, turn map[string]any) {
	turnID := stringValue(turn, "id")
	if turnID == "" {
		return
	}
	p.mu.Lock()
	if p.completedTurn == turnID {
		p.mu.Unlock()
		return
	}
	p.completedTurn = turnID
	p.mu.Unlock()
	for _, item := range mapSlice(turn["items"]) {
		b.postChatItem(map[string]any{"threadId": p.id, "turnId": turnID, "item": item})
	}
	p.completeTurn(turnID)
	p.setActiveTurn("")
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
	cert := cachedBirthCertificate("codex", id)
	var hint map[string]any
	if cert == nil {
		b.hintMu.Lock()
		hint = hooks.ConsumeSpawnHint(cwd, "codex")
		b.hintMu.Unlock()
	}
	circle, source := defaultCircle, "fallback"
	tmux := threadTmuxPlacement(cwd, b.boundary)
	if tmux.circle != "" {
		circle, source = tmux.circle, tmux.source
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
	gitInfo, _ := thread["gitInfo"].(map[string]any)
	p := &threadPeer{
		bridge: b, id: id, cwd: cwd, circle: circle, circleSrc: source, role: role, hintedID: hintedID, birthCert: cert, cancel: cancel,
		model: stringValue(thread, "model"), branch: stringValue(gitInfo, "branch"), gitStatus: repositoryStatus(cwd),
		toolCalls: map[string][]map[string]string{}, seenItems: map[string]map[string]bool{},
	}
	if tmux.paneID != "" {
		p.tmux = map[string]any{"session": tmux.session, "window_id": tmux.windowID, "pane_id": tmux.paneID, "pane_pid": tmux.panePID}
	}
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
	if err := p.ensureContext(ctx); err != nil {
		log.Printf("codex bridge: inject mesh context for %s: %v", p.id, err)
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
	if err := p.restoreIdentity(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	claim := p.peerID
	if claim == "" {
		claim = p.hintedID
	}
	p.mu.Unlock()
	metadata := map[string]any{
		"runtime_session_id": p.id, "runtime_source_uri": "codex:" + p.id,
		"transport": "codex-app-server", "capabilities": []string{"delivery_receipts", "thread_steering"},
		"branch": p.branch, "git_status": p.gitStatus,
	}
	if p.tmux != nil {
		metadata["tmux_evidence"] = p.tmux
	}
	body := map[string]any{
		"name": safeName(filepath.Base(p.cwd)), "path": p.cwd, "circle": p.circle,
		"circle_source": p.circleSrc, "backend": "codex", "role": p.role,
		"metadata": metadata,
	}
	if p.model != "" {
		body["model"] = p.model
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
	if circle := stringValue(result, "circle"); circle != "" {
		p.circle = circle
	}
	if role := stringValue(result, "role"); role != "" {
		p.role = role
	}
	p.mu.Unlock()
	if p.peerID == "" {
		return errors.New("daemon registration returned no peer_id")
	}
	if cert, ok := result["birth_certificate"].(map[string]any); ok {
		p.mu.Lock()
		p.birthCert = cert
		p.mu.Unlock()
		if err := hooks.WriteRuntimeIdentity("codex", p.id, map[string]any{"birth_certificate": cert}); err != nil {
			return fmt.Errorf("persist runtime identity: %w", err)
		}
	}
	return nil
}

func cachedBirthCertificate(backend, sessionID string) map[string]any {
	cert, _ := hooks.ReadRuntimeIdentity(backend, sessionID)["birth_certificate"].(map[string]any)
	if cert == nil || stringValue(cert, "runtime_session_id") != sessionID {
		return nil
	}
	expires, err := time.Parse(time.RFC3339Nano, stringValue(cert, "expires_at"))
	if err != nil || !expires.After(time.Now()) {
		return nil
	}
	return cert
}

// restoreIdentity rebinds a restarted bridge from the daemon-minted proof for
// this exact Codex thread before weaker spawn/name hints are considered.
func (p *threadPeer) restoreIdentity(ctx context.Context) error {
	p.mu.Lock()
	if p.peerID != "" || p.birthCert == nil {
		p.mu.Unlock()
		return nil
	}
	cert := p.birthCert
	p.mu.Unlock()
	result, err := p.bridge.daemonRequest(ctx, http.MethodPost, "/peers/identity/validate", map[string]any{
		"birth_certificate": cert, "backend": "codex", "path": p.cwd,
	})
	if err != nil {
		return fmt.Errorf("validate runtime identity: %w", err)
	}
	peer, _ := result["peer"].(map[string]any)
	peerID, displayName := stringValue(peer, "peer_id"), stringValue(peer, "display_name")
	circle, role := stringValue(peer, "circle"), stringValue(peer, "role")
	if peerID == "" || displayName == "" || circle == "" || role == "" {
		return errors.New("runtime identity validation returned an incomplete peer")
	}
	p.mu.Lock()
	p.peerID = peerID
	p.displayName, p.circle, p.role = displayName, circle, role
	p.mu.Unlock()
	return nil
}

func (p *threadPeer) ensureContext(ctx context.Context) error {
	state := hooks.ReadRuntimeIdentity("codex", p.id)
	if injected, _ := state["mesh_context_injected"].(bool); injected {
		return nil
	}
	result, err := p.bridge.daemonRequest(ctx, http.MethodGet, "/peers", nil)
	if err != nil {
		return err
	}
	peers := mapSlice(result["peers"])
	var self map[string]any
	for _, peer := range peers {
		if stringValue(peer, "peer_id") == p.peerID {
			self = peer
			break
		}
	}
	sections := []string{hooks.FormatSelfContext(p.displayName, p.peerID, p.circle, p.circleSrc, "codex", p.role, p.cwd, p.branch, self)}
	if value := hooks.FormatPeersContext(peers, p.displayName); value != "" {
		sections = append(sections, value)
	}
	if value := hooks.LoadHandoff(p.cwd, "codex", p.id); value != "" {
		sections = append(sections, value)
	}
	item := map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": strings.Join(sections, "\n\n")}}}
	if _, err := p.bridge.call(ctx, "thread/inject_items", map[string]any{"threadId": p.id, "items": []any{item}}); err != nil {
		return err
	}
	return hooks.WriteRuntimeIdentity("codex", p.id, map[string]any{"mesh_context_injected": true})
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
	kind := stringValue(frame, "type")
	cid := stringValue(frame, "correlation_id")
	text := stringValue(frame, "text") + hooks.FormatAttachments(frame["attachments"])
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
	text = hooks.FormatInboundMessage(firstNonempty(stringValue(frame, "from_peer"), "unknown"), stringValue(frame, "to_peer"), kind, cid, text)
	input := []any{map[string]any{"type": "text", "text": text}}
	input = append(input, attachmentInputs(frame["attachments"])...)
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
		p.deliveryAck(ctx, conn, deliveryID, kind, "failed", err.Error())
		return
	}
	p.deliveryAck(ctx, conn, deliveryID, kind, "accepted", "codex thread API")
}

func attachmentInputs(value any) []any {
	var out []any
	for _, item := range mapSlice(value) {
		contentType, path := stringValue(item, "content_type"), stringValue(item, "path")
		if !strings.HasPrefix(contentType, "image/") {
			continue
		}
		if path, ok := nativeAttachmentPath(path); ok {
			out = append(out, map[string]any{"type": "localImage", "path": path})
		}
	}
	return out
}

func nativeAttachmentPath(path string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || path == "" {
		return "", false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	base := filepath.Join(home, ".repowire", "attachments")
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	info, err := os.Lstat(abs)
	return abs, err == nil && info.Mode().IsRegular()
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
	changed := p.busy != busy
	p.busy = busy
	conn := p.conn
	p.mu.Unlock()
	if conn != nil && changed {
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
	turnID := stringValue(params, "turnId")
	if itemID := stringValue(item, "id"); itemID != "" {
		p.mu.Lock()
		seen := p.seenItems[turnID]
		if seen == nil {
			seen = map[string]bool{}
			p.seenItems[turnID] = seen
		}
		if seen[itemID] {
			p.mu.Unlock()
			return
		}
		seen[itemID] = true
		p.mu.Unlock()
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
	default:
		if call := completedToolCall(item); call != nil {
			p.mu.Lock()
			p.toolCalls[turnID] = append(p.toolCalls[turnID], call)
			p.mu.Unlock()
		}
		return
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
	body := map[string]any{"peer": displayName, "peer_id": peerID, "role": role, "text": text, "session_id": p.id, "turn_id": turnID}
	p.mu.Lock()
	if role == "user" {
		p.lastUser = text
	} else {
		p.lastAnswer = text
		if calls := p.toolCalls[turnID]; len(calls) > 0 {
			body["tool_calls"] = calls
		}
	}
	p.mu.Unlock()
	postCtx, cancel := context.WithTimeout(b.ctx, 3*time.Second)
	defer cancel()
	if _, err := b.daemonRequest(postCtx, http.MethodPost, "/events/chat", body); err != nil {
		log.Printf("codex bridge: post chat item for %s: %v", p.id, err)
	}
}

func (p *threadPeer) completeTurn(turnID string) {
	p.mu.Lock()
	user, answer := p.lastUser, p.lastAnswer
	p.lastUser, p.lastAnswer = "", ""
	delete(p.toolCalls, turnID)
	delete(p.seenItems, turnID)
	p.mu.Unlock()
	if user != "" || answer != "" {
		hooks.WriteHandoff(p.cwd, "codex", p.id, user, answer)
	}
}

func completedToolCall(item map[string]any) map[string]string {
	var name, input string
	switch stringValue(item, "type") {
	case "commandExecution":
		name, input = "exec_command", stringValue(item, "command")
	case "fileChange":
		name, input = "apply_patch", changedPaths(item["changes"])
	case "mcpToolCall":
		name = strings.Trim(strings.Join([]string{stringValue(item, "server"), stringValue(item, "tool")}, "__"), "_")
		input = compactValue(item["arguments"])
	case "dynamicToolCall":
		name, input = stringValue(item, "tool"), compactValue(item["arguments"])
	case "collabAgentToolCall":
		name, input = stringValue(item, "tool"), firstNonempty(stringValue(item, "prompt"), compactValue(item["receiverThreadIds"]))
	case "webSearch":
		name, input = "web_search", stringValue(item, "query")
	case "imageView":
		name, input = "view_image", stringValue(item, "path")
	case "imageGeneration":
		name, input = "image_generation", stringValue(item, "prompt")
	default:
		return nil
	}
	if name == "" {
		return nil
	}
	if len(input) > 120 {
		input = input[:119] + "…"
	}
	return map[string]string{"name": name, "input": input}
}

func changedPaths(value any) string {
	var paths []string
	for _, change := range mapSlice(value) {
		if path := stringValue(change, "path"); path != "" {
			paths = append(paths, path)
		}
	}
	return strings.Join(paths, ", ")
}

func compactValue(value any) string {
	raw, _ := json.Marshal(value)
	if string(raw) == "null" {
		return ""
	}
	return string(raw)
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

func mapSlice(value any) []map[string]any {
	items, _ := value.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok {
			out = append(out, item)
		}
	}
	return out
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

type tmuxPlacement struct {
	circle, source, session, windowID, paneID string
	panePID                                   int
}

func threadTmuxPlacement(cwd string, boundary proto.CircleBoundary) tmuxPlacement {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_current_path}\t#{session_name}\t#{window_id}\t#{pane_id}\t#{pane_pid}").Output()
	if err != nil {
		return tmuxPlacement{}
	}
	processes, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return tmuxPlacement{}
	}
	return parseTmuxPlacement(string(out), string(processes), cwd, boundary)
}

func parseTmuxPlacement(output, processes, cwd string, boundary proto.CircleBoundary) tmuxPlacement {
	want, _ := filepath.Abs(cwd)
	var matches []tmuxPlacement
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 5 {
			continue
		}
		path, _ := filepath.Abs(fields[0])
		if filepath.Clean(path) != filepath.Clean(want) {
			continue
		}
		root, err := strconv.Atoi(fields[4])
		if err != nil || !processTreeHasCodex(processes, root) {
			continue
		}
		candidate := proto.TmuxCircle(boundary, fields[1], fields[2])
		if candidate == "" {
			continue
		}
		source := "tmux"
		if boundary == proto.CircleBoundaryWindow {
			source = "tmux_window"
		}
		matches = append(matches, tmuxPlacement{circle: candidate, source: source, session: fields[1], windowID: fields[2], paneID: fields[3], panePID: root})
	}
	if len(matches) == 0 {
		return tmuxPlacement{}
	}
	for _, match := range matches[1:] {
		if match.circle != matches[0].circle {
			return tmuxPlacement{}
		}
	}
	if len(matches) > 1 {
		return tmuxPlacement{circle: matches[0].circle, source: matches[0].source}
	}
	return matches[0]
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

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func repositoryStatus(cwd string) map[string]any {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = cwd
	raw, err := cmd.Output()
	if err != nil {
		return map[string]any{"available": false}
	}
	text := strings.TrimSpace(string(raw))
	changed := 0
	if text != "" {
		changed = len(strings.Split(text, "\n"))
	}
	return map[string]any{"clean": changed == 0, "changed_files": changed}
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
