package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// ACPPeerSpec is the validated metadata.acp block for a brokered peer.
type ACPPeerSpec struct {
	PeerID  proto.PeerID
	Command string
	Args    []string
	CWD     string
	Env     map[string]string
}

// ACPPromptResult is the assembled assistant turn returned by session/prompt.
type ACPPromptResult struct {
	Text       string
	StopReason string
}

type acpRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type acpClient struct {
	spec         ACPPeerSpec
	mu           sync.Mutex
	writeMu      sync.Mutex
	pendingMu    sync.Mutex
	updatesMu    sync.Mutex
	permissionMu sync.RWMutex
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	pending      map[string]chan acpRPCResponse
	readerDone   chan error
	sessionID    string
	activeText   []string
	closed       bool
	nextID       atomic.Uint64
	permission   ACPPermissionHandler
}

func newACPClient(spec ACPPeerSpec) *acpClient {
	return &acpClient{spec: spec, pending: map[string]chan acpRPCResponse{}}
}

func (c *acpClient) prompt(ctx context.Context, text string) (ACPPromptResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ACPPromptResult{}, errors.New("ACP client is closed")
	}
	if err := c.ensureStartedLocked(ctx); err != nil {
		return ACPPromptResult{}, err
	}
	if c.sessionID == "" {
		cwd := c.spec.CWD
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		if resolved, err := filepath.Abs(cwd); err == nil {
			cwd = resolved
		}
		var result struct {
			SessionID string `json:"sessionId"`
		}
		if err := c.callLocked(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}, &result); err != nil {
			c.closeLocked()
			return ACPPromptResult{}, fmt.Errorf("ACP session/new: %w", err)
		}
		if result.SessionID == "" {
			c.closeLocked()
			return ACPPromptResult{}, errors.New("ACP session/new returned no sessionId")
		}
		c.sessionID = result.SessionID
	}
	c.updatesMu.Lock()
	c.activeText = nil
	c.updatesMu.Unlock()
	var result struct {
		StopReason string `json:"stopReason"`
	}
	err := c.callLocked(ctx, "session/prompt", map[string]any{
		"sessionId": c.sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": text}},
	}, &result)
	if err != nil {
		_ = c.notify("session/cancel", map[string]any{"sessionId": c.sessionID})
		c.closeLocked()
		return ACPPromptResult{}, fmt.Errorf("ACP session/prompt: %w", err)
	}
	// Updates share the stdio stream with the prompt response. The reader handles
	// each line synchronously, so all preceding chunks are already recorded.
	c.updatesMu.Lock()
	assembled := ""
	for _, chunk := range c.activeText {
		assembled += chunk
	}
	c.updatesMu.Unlock()
	return ACPPromptResult{Text: assembled, StopReason: result.StopReason}, nil
}

func (c *acpClient) ensureStartedLocked(ctx context.Context) error {
	if c.cmd != nil {
		return nil
	}
	cmd := exec.Command(c.spec.Command, c.spec.Args...)
	cmd.Dir = c.spec.CWD
	cmd.Env = os.Environ()
	for key, value := range c.spec.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd, c.stdin = cmd, stdin
	c.readerDone = make(chan error, 1)
	go c.readLoop(stdout)
	var result struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	err = c.callLocked(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"auth":     map[string]any{"terminal": false},
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}, &result)
	if err != nil {
		c.closeLocked()
		return fmt.Errorf("ACP initialize: %w", err)
	}
	return nil
}

func (c *acpClient) callLocked(ctx context.Context, method string, params any, dst any) error {
	id := fmt.Sprint(c.nextID.Add(1))
	ch := make(chan acpRPCResponse, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() { c.pendingMu.Lock(); delete(c.pending, id); c.pendingMu.Unlock() }()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": c.nextID.Load(), "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case response := <-ch:
		if response.Error != nil {
			return fmt.Errorf("rpc %d: %s", response.Error.Code, response.Error.Message)
		}
		if dst != nil && len(response.Result) > 0 {
			return json.Unmarshal(response.Result, dst)
		}
		return nil
	case err := <-c.readerDone:
		if err == nil {
			err = io.EOF
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *acpClient) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *acpClient) write(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdin == nil {
		return errors.New("ACP subprocess is not running")
	}
	bytes, err := json.Marshal(value)
	if err != nil {
		return err
	}
	bytes = append(bytes, '\n')
	_, err = c.stdin.Write(bytes)
	return err
}

func (c *acpClient) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var message acpRPCResponse
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		if message.Method != "" {
			c.handleAgentMessage(message)
			continue
		}
		id := rawID(message.ID)
		c.pendingMu.Lock()
		ch := c.pending[id]
		c.pendingMu.Unlock()
		if ch != nil {
			ch <- message
		}
	}
	c.readerDone <- scanner.Err()
}

func (c *acpClient) handleAgentMessage(message acpRPCResponse) {
	if message.Method == "session/update" {
		var params struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if json.Unmarshal(message.Params, &params) == nil && params.Update.SessionUpdate == "agent_message_chunk" && params.Update.Content.Text != "" {
			c.updatesMu.Lock()
			c.activeText = append(c.activeText, params.Update.Content.Text)
			c.updatesMu.Unlock()
		}
		return
	}
	if len(message.ID) == 0 || string(message.ID) == "null" {
		return
	}
	// The Go broker advertises no client-side filesystem/terminal capability.
	// Permission requests are denied explicitly so tools fail closed rather than
	// hanging an agent subprocess.
	if message.Method == "session/request_permission" {
		result := map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
		c.permissionMu.RLock()
		handler := c.permission
		c.permissionMu.RUnlock()
		if handler != nil {
			result = handler(c.spec.PeerID, message.Params)
		}
		_ = c.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(message.ID), "result": result})
		return
	}
	_ = c.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(message.ID), "error": map[string]any{"code": -32601, "message": "method not supported"}})
}

func (c *acpClient) setPermissionHandler(handler ACPPermissionHandler) {
	c.permissionMu.Lock()
	c.permission = handler
	c.permissionMu.Unlock()
}

func rawID(value json.RawMessage) string {
	var number json.Number
	if json.Unmarshal(value, &number) == nil {
		return number.String()
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		return text
	}
	return string(value)
}

func (c *acpClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

func (c *acpClient) closeLocked() {
	if c.cmd == nil {
		return
	}
	if c.sessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var ignored map[string]any
		_ = c.callLocked(ctx, "session/close", map[string]any{"sessionId": c.sessionID}, &ignored)
		cancel()
	}
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	c.cmd, c.stdin, c.sessionID = nil, nil, ""
}

// ACPManager owns one persistent subprocess/session per peer.
type ACPManager struct {
	mu         sync.Mutex
	clients    map[proto.PeerID]*acpClient
	closed     bool
	permission ACPPermissionHandler
}

func NewACPManager() *ACPManager { return &ACPManager{clients: map[proto.PeerID]*acpClient{}} }

func (m *ACPManager) Prompt(spec ACPPeerSpec, text string, complete func(ACPPromptResult, error)) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("ACP manager is closed")
	}
	client := m.clients[spec.PeerID]
	if client == nil {
		client = newACPClient(spec)
		client.setPermissionHandler(m.permission)
		m.clients[spec.PeerID] = client
	}
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		result, err := client.prompt(ctx, text)
		if err != nil {
			m.drop(spec.PeerID, client)
		}
		if complete != nil {
			complete(result, err)
		}
	}()
	return nil
}

func (m *ACPManager) SetPermissionHandler(handler ACPPermissionHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.permission = handler
	for _, client := range m.clients {
		client.setPermissionHandler(handler)
	}
}

func (m *ACPManager) drop(id proto.PeerID, expected *acpClient) {
	m.mu.Lock()
	if m.clients[id] == expected {
		delete(m.clients, id)
	}
	m.mu.Unlock()
	expected.close()
}

func (m *ACPManager) Drop(id proto.PeerID) {
	m.mu.Lock()
	client := m.clients[id]
	delete(m.clients, id)
	m.mu.Unlock()
	if client != nil {
		client.close()
	}
}

func (m *ACPManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	clients := make([]*acpClient, 0, len(m.clients))
	for _, client := range m.clients {
		clients = append(clients, client)
	}
	m.clients = map[proto.PeerID]*acpClient{}
	m.mu.Unlock()
	for _, client := range clients {
		client.close()
	}
}
