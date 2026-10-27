package relayserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
)

// A machine answers the relay's request API for one owner: a daemon over its
// tunnel, or the managed service, which is always online. The relay MCP tools
// speak only this; each machine decides identity, access and delivery for its
// own agents.
type machine interface {
	agents(ctx context.Context, circle string) ([]map[string]any, error)
	// checkAgent fails unless peerID is an exact, addressable agent id.
	checkAgent(ctx context.Context, peerID string) error
	ask(ctx context.Context, sender, peerID, text string) (string, error)
	notify(ctx context.Context, sender, peerID, text string) (map[string]any, error)
	wait(ctx context.Context, sender, requestID string, seconds int) (map[string]any, error)
	pending(ctx context.Context, sender string) ([]map[string]any, error)
}

const managedMachineID = "managed"

// errUncertain marks a request the machine may have taken: the caller keeps
// the request id and reads it back, and never resends.
var errUncertain = errors.New("delivery is uncertain")

func (s *Server) machine(userID, id string) (machine, error) {
	if id == "" {
		return nil, fmt.Errorf("daemon_id is required; use list_machines")
	}
	if id == managedMachineID && s.worker != nil {
		return managedMachine{worker: s.worker, owner: userID}, nil
	}
	conn := s.daemon(userID, id)
	if conn == nil {
		return nil, fmt.Errorf("machine %q is offline or unavailable to this connection", id)
	}
	return daemonMachine{s: s, conn: conn}, nil
}

// ---- daemon -----------------------------------------------------------------

type daemonMachine struct {
	s    *Server
	conn *daemonConn
}

func (d daemonMachine) agents(ctx context.Context, circle string) ([]map[string]any, error) {
	q := url.Values{"listed": {"true"}, "status": {"online"}}
	if circle != "" {
		q.Set("circle", circle)
	}
	data, err := d.call(ctx, http.MethodGet, "/peers", q, nil)
	if err != nil {
		return nil, err
	}
	peers, _ := data["peers"].([]any)
	rows := []map[string]any{}
	for _, raw := range peers {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		row := map[string]any{}
		for _, k := range []string{"peer_id", "display_name", "circle", "backend", "status", "description", "model", "path", "addressable", "source", "turn_state"} {
			if v, ok := p[k]; ok {
				row[k] = v
			}
		}
		if metadata, ok := p["metadata"].(map[string]any); ok {
			if project, ok := metadata["project"]; ok {
				row["project"] = project
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (d daemonMachine) checkAgent(ctx context.Context, peerID string) error {
	p, err := d.call(ctx, http.MethodGet, "/peers/"+url.PathEscape(peerID), nil, nil)
	if err != nil {
		return err
	}
	if p["peer_id"] != peerID {
		return fmt.Errorf("use the exact peer_id from list_agents, not its display name")
	}
	return nil
}

func (d daemonMachine) ask(ctx context.Context, sender, peerID, text string) (string, error) {
	health, err := d.call(ctx, http.MethodGet, "/health", nil, nil)
	if err != nil {
		return "", err
	}
	caps, _ := health["capabilities"].(map[string]any)
	if caps["ask_pull_delivery"] != true {
		return "", fmt.Errorf("update the local daemon: it does not support pull-delivery asks; no task was sent")
	}
	data, err := d.call(ctx, http.MethodPost, "/ask", nil, map[string]any{"from_peer": sender, "to_peer": peerID, "text": text, "reply_delivery": "pull"})
	if err != nil {
		return "", err
	}
	requestID, ok := data["correlation_id"].(string)
	if !ok || requestID == "" {
		return "", fmt.Errorf("daemon returned no request ID; delivery may have happened, so do not automatically resend")
	}
	return requestID, nil
}

func (d daemonMachine) notify(ctx context.Context, sender, peerID, text string) (map[string]any, error) {
	data, err := d.call(ctx, http.MethodPost, "/notify", nil, map[string]any{"from_peer": sender, "to_peer": peerID, "text": text})
	if err != nil {
		return nil, err
	}
	return map[string]any{"delivery_id": data["delivery_id"], "delivery_state": data["delivery_state"], "delivered": data["delivered"], "queued": data["queued"]}, nil
}

func (d daemonMachine) wait(ctx context.Context, sender, requestID string, seconds int) (map[string]any, error) {
	return d.call(ctx, http.MethodPost, "/asks/"+url.PathEscape(requestID)+"/wait", nil, map[string]any{"peer_id": sender, "timeout_seconds": seconds})
}

func (d daemonMachine) pending(ctx context.Context, sender string) ([]map[string]any, error) {
	data, err := d.call(ctx, http.MethodGet, "/asks/pending", url.Values{"peer_id": {sender}, "direction": {"outbound"}}, nil)
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	if asks, ok := data["asks"].([]any); ok {
		for _, raw := range asks {
			if ask, ok := raw.(map[string]any); ok {
				rows = append(rows, map[string]any{"request_id": ask["correlation_id"], "to_agent": ask["to_peer"], "message": ask["text"], "created_at": ask["created_at"]})
			}
		}
	}
	return rows, nil
}

// The relay is only an adapter. No routing logic or local HTTP credentials live
// here: the existing authenticated outbound tunnel and daemon own delivery.
func (d daemonMachine) call(ctx context.Context, method, path string, query url.Values, body any) (map[string]any, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, method, "http://relay-internal"+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	r.URL.RawQuery = query.Encode()
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	d.s.tunnel(w, r, d.conn, path)
	if w.Code < 200 || w.Code >= 300 {
		return nil, fmt.Errorf("daemon request returned HTTP %d: %s. Delivery may be uncertain after a timeout; do not automatically resend", w.Code, w.Body.String())
	}
	var data map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		return nil, fmt.Errorf("invalid daemon response: %w", err)
	}
	return data, nil
}

// ---- managed ----------------------------------------------------------------

// managedMachine is the owner's agents on the managed service that no daemon
// owns. Daemon-owned agents stay with their daemon, so each agent has one
// place that answers for it.
type managedMachine struct {
	worker *managedWorker
	owner  string
}

type managedAgent struct {
	ID          string  `json:"id"`
	DisplayName *string `json:"display_name"`
	Description *string `json:"description"`
	State       string  `json:"state"`
	Workspace   *string `json:"workspace"`
	HomeMachine *string `json:"home_machine"`
	CreatedBy   string  `json:"created_by"`
	RetiredAt   *string `json:"retired_at"`
	Circle      string  `json:"circle"`
}

// circle is the agent's own circle; a Worker from before agent circles
// reports none, and then every agent is in managed.
func (a managedAgent) circle() string {
	if a.Circle == "" {
		return managedMachineID
	}
	return a.Circle
}

// Agents a daemon created stay with it, including ones recorded without a
// home; only agents created through the relay are on the managed machine.
func (a managedAgent) onMachine() bool {
	return a.CreatedBy != "daemon" && a.HomeMachine == nil && a.RetiredAt == nil
}

func (m managedMachine) agents(ctx context.Context, circle string) ([]map[string]any, error) {
	var list struct {
		Items []managedAgent `json:"items"`
	}
	if _, err := m.do(ctx, http.MethodGet, "/agents", nil, nil, &list); err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, a := range list.Items {
		if !a.onMachine() || (circle != "" && a.circle() != circle) {
			continue
		}
		status := "online"
		if a.State == "working" {
			status = "busy"
		}
		name := a.ID
		if a.DisplayName != nil && *a.DisplayName != "" {
			name = *a.DisplayName
		}
		rows = append(rows, map[string]any{
			"peer_id": a.ID, "display_name": name, "circle": a.circle(), "backend": "hosted", "status": status,
			"state": a.State, "description": a.Description, "workspace": a.Workspace, "addressable": true,
		})
	}
	return rows, nil
}

func (m managedMachine) checkAgent(ctx context.Context, peerID string) error {
	var a managedAgent
	status, err := m.do(ctx, http.MethodGet, "/agents/"+url.PathEscape(peerID), nil, nil, &a)
	if status == http.StatusNotFound || (err == nil && !a.onMachine()) {
		return fmt.Errorf("no managed agent %q; use the exact peer_id from list_agents", peerID)
	}
	return err
}

func (m managedMachine) submit(ctx context.Context, kind, sender, peerID, text string) (string, error) {
	id, err := requestID()
	if err != nil {
		return "", err
	}
	status, err := m.do(ctx, http.MethodPost, "/requests", nil, map[string]any{"id": id, "agent": peerID, "kind": kind, "requester": sender, "text": text}, nil)
	switch {
	case err == nil:
		return id, nil
	case status >= 400 && status < 500:
		return "", fmt.Errorf("not sent: %w", err)
	default:
		return id, fmt.Errorf("%w: %v", errUncertain, err)
	}
}

func (m managedMachine) ask(ctx context.Context, sender, peerID, text string) (string, error) {
	return m.submit(ctx, "ask", sender, peerID, text)
}

func (m managedMachine) notify(ctx context.Context, sender, peerID, text string) (map[string]any, error) {
	id, err := m.submit(ctx, "notify", sender, peerID, text)
	if errors.Is(err, errUncertain) {
		return map[string]any{"delivery_id": id, "delivery_state": "uncertain", "delivered": false, "queued": false}, err
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"delivery_id": id, "delivery_state": "accepted", "delivered": true, "queued": false}, nil
}

func (m managedMachine) wait(ctx context.Context, sender, requestID string, seconds int) (map[string]any, error) {
	var out map[string]any
	q := url.Values{"requester": {sender}, "wait": {fmt.Sprint(seconds)}}
	if _, err := m.do(ctx, http.MethodGet, "/requests/"+url.PathEscape(requestID), q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (m managedMachine) pending(ctx context.Context, sender string) ([]map[string]any, error) {
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if _, err := m.do(ctx, http.MethodGet, "/requests", url.Values{"requester": {sender}}, nil, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// do sends one owner request and decodes a 2xx body into out. Any other
// status is an error carrying the service's {error} text; the status is
// returned so callers can tell a refusal from an outage.
func (m managedMachine) do(ctx context.Context, method, path string, query url.Values, body, out any) (int, error) {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return 0, err
		}
	}
	resp, err := m.worker.call(ctx, method, path, query.Encode(), m.owner, encoded)
	if err != nil {
		return 0, fmt.Errorf("managed service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&failure)
		return resp.StatusCode, fmt.Errorf("managed service returned %d: %s", resp.StatusCode, failure.Error)
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

func requestID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "rq-" + hex.EncodeToString(b), nil
}
