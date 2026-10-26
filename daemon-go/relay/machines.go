package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"
)

// Cross-machine requests (rfcs/cross-machine-circles.md). Outbound, the
// daemon asks the relay to forward a /remote/ request to another of the
// owner's machines. Inbound, the relay delivers remote_request frames, which
// run the daemon's remote handler in-process on a bounded pool.

const (
	remoteWorkers = 16
	remoteQueue   = 64
)

// ErrRelayDown means no relay connection is live; nothing was sent.
var ErrRelayDown = errors.New("relay not connected")

type machineResponse struct {
	status int
	body   []byte
	err    error
}

type remoteMachineKey struct{}

// RemoteMachine is the machine a remote request came from, as named by the
// relay. It is empty for every request that did not arrive as remote_request.
func RemoteMachine(ctx context.Context) string {
	machine, _ := ctx.Value(remoteMachineKey{}).(string)
	return machine
}

// MachineRequest sends one request to another machine and waits for its
// answer. A transport error after the frame left means the outcome is
// unknown; ErrRelayDown means it was never sent.
func (c *Client) MachineRequest(ctx context.Context, machine, method, path string, body []byte) (int, []byte, error) {
	return c.machineFrame(ctx, map[string]any{"type": "machine_request", "machine": machine, "method": method, "path": path, "body": base64.StdEncoding.EncodeToString(body)})
}

// Machines lists the owner's other online machines.
func (c *Client) Machines(ctx context.Context) ([]string, error) {
	status, body, err := c.machineFrame(ctx, map[string]any{"type": "machine_list"})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("relay machine list returned %d", status)
	}
	var out struct {
		Machines []string `json:"machines"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Machines, nil
}

func (c *Client) machineFrame(ctx context.Context, frame map[string]any) (int, []byte, error) {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return 0, nil, ErrRelayDown
	}
	id := machineID()
	frame["id"] = id
	ch := make(chan machineResponse, 1)
	session.machinesMu.Lock()
	session.machines[id] = ch
	session.machinesMu.Unlock()
	defer func() {
		session.machinesMu.Lock()
		delete(session.machines, id)
		session.machinesMu.Unlock()
	}()
	if err := session.write(ctx, frame); err != nil {
		return 0, nil, fmt.Errorf("relay write: %w", err)
	}
	select {
	case r := <-ch:
		return r.status, r.body, r.err
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (s *relaySession) deliverMachine(msg map[string]any) {
	s.machinesMu.Lock()
	ch := s.machines[stringField(msg, "id")]
	s.machinesMu.Unlock()
	if ch == nil {
		return
	}
	body, _ := base64.StdEncoding.DecodeString(stringField(msg, "body"))
	status, _ := msg["status"].(float64)
	select {
	case ch <- machineResponse{status: int(status), body: body}:
	default:
	}
}

// failMachines ends every outbound request when the connection drops.
func (s *relaySession) failMachines() {
	s.machinesMu.Lock()
	defer s.machinesMu.Unlock()
	for id, ch := range s.machines {
		select {
		case ch <- machineResponse{err: errors.New("relay connection lost")}:
		default:
		}
		delete(s.machines, id)
	}
}

func (c *Client) startRemoteWorkers(ctx context.Context, session *relaySession) {
	for range remoteWorkers {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case msg := <-session.remoteWork:
					c.serveRemote(ctx, session, msg)
				}
			}
		}()
	}
}

// admitRemote queues a remote_request without blocking the read loop.
func (c *Client) admitRemote(ctx context.Context, session *relaySession, msg map[string]any) {
	select {
	case session.remoteWork <- msg:
	default:
		c.sendHTTPResponse(ctx, session, stringField(msg, "request_id"), http.StatusTooManyRequests, nil, []byte(`{"detail":"overloaded, not processed"}`))
	}
}

func (c *Client) serveRemote(ctx context.Context, session *relaySession, msg map[string]any) {
	reqID := stringField(msg, "request_id")
	deadline := time.UnixMilli(int64(numberField(msg, "deadline")))
	if c.remote == nil {
		c.sendHTTPResponse(ctx, session, reqID, http.StatusNotFound, nil, []byte(`{"detail":"remote requests not served"}`))
		return
	}
	// Work still queued past its deadline is discarded unrun: the source has
	// already given up, and its later read settles the request.
	if time.Now().After(deadline) {
		return
	}
	body, _ := base64.StdEncoding.DecodeString(stringField(msg, "body"))
	reqCtx, cancel := context.WithDeadline(context.WithValue(ctx, remoteMachineKey{}, stringField(msg, "from_machine")), deadline)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, stringField(msg, "method"), "http://remote"+requestPath(msg), bytes.NewReader(body))
	if err != nil {
		c.sendHTTPResponse(ctx, session, reqID, http.StatusBadRequest, nil, []byte(err.Error()))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c.remote.ServeHTTP(w, req)
	c.sendHTTPResponse(ctx, session, reqID, w.Code, responseHeaders(w.Header()), w.Body.Bytes())
}

func numberField(msg map[string]any, key string) float64 {
	value, _ := msg[key].(float64)
	return value
}

func machineID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "m-" + hex.EncodeToString(b[:])
}
