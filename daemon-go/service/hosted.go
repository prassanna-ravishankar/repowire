package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// ============================================================================
// Hosted agents: managed peers whose runtime is a durable agent on the hosted
// service (hosted/ in this repo). The daemon owns their peer identity and
// routing, exactly like the human inbox: a hosted peer never holds a socket,
// stays addressable while its runtime sleeps, and receives deliveries as
// durable submissions keyed by an operation id. The hosted agent id is the
// peer_id, so there is one identity end to end.
//
// Every submission is a hosted_delivery operation until the runtime settles
// it. The runtime keeps the work across its own restarts; the operation lets
// the daemon resume waiting across a daemon restart instead of losing the
// reply.
// ============================================================================

const hostedDeliveryKind = "hosted_delivery"

// hostedWaitAttempt bounds one held wait request; a slow answer reconnects
// with the same operation id. hostedWaitBackoffMax caps the reconnect delay
// after a network failure. Both run only while a submission is in flight.
const (
	hostedWaitAttempt    = 10 * time.Minute
	hostedWaitBackoffMax = 30 * time.Second
)

// HostedError is a non-2xx answer from the hosted runtime.
type HostedError struct {
	Status  int
	Message string
}

func (e *HostedError) Error() string {
	return fmt.Sprintf("hosted runtime %d: %s", e.Status, e.Message)
}

// HostedSource is the provenance recorded with a submission in the runtime's
// transcript. It is a label for UIs, not an authorization.
type HostedSource struct {
	From          string `json:"from"`
	FromPeerID    string `json:"fromPeerId,omitempty"`
	Type          string `json:"type"`
	CorrelationID string `json:"correlationId,omitempty"`
}

// HostedResult is how one submission settled: done with the final text, or
// unanswered with the runtime's reason.
type HostedResult struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Text   string `json:"text,omitempty"`
}

// HostedRuntime is the hosted service's API. *HostedClient implements it.
type HostedRuntime interface {
	CreateAgent(ctx context.Context, id string, workspace *string) error
	Submit(ctx context.Context, id, operationID, text string, source HostedSource) (accepted bool, err error)
	Wait(ctx context.Context, id, operationID string) (HostedResult, error)
}

// HostedClient speaks the hosted runtime's HTTP API with its bearer token.
type HostedClient struct {
	base  string
	token string
	http  *http.Client
}

func NewHostedClient(base, token string) *HostedClient {
	return &HostedClient{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{}}
}

func (c *HostedClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		if failure.Error == "" {
			failure.Error = http.StatusText(resp.StatusCode)
		}
		return &HostedError{Status: resp.StatusCode, Message: failure.Error}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func agentPath(id string, rest ...string) string {
	parts := append([]string{"/agents", url.PathEscape(id)}, rest...)
	return strings.Join(parts, "/")
}

func (c *HostedClient) CreateAgent(ctx context.Context, id string, workspace *string) error {
	body := map[string]any{"id": id}
	if workspace != nil {
		body["workspace"] = *workspace
	}
	return c.do(ctx, http.MethodPost, "/agents", body, nil)
}

func (c *HostedClient) Submit(ctx context.Context, id, operationID, text string, source HostedSource) (bool, error) {
	var receipt struct {
		Accepted bool `json:"accepted"`
	}
	err := c.do(ctx, http.MethodPost, agentPath(id, "messages"),
		map[string]any{"operationId": operationID, "text": text, "source": source}, &receipt)
	return receipt.Accepted, err
}

func (c *HostedClient) Wait(ctx context.Context, id, operationID string) (HostedResult, error) {
	var result HostedResult
	err := c.do(ctx, http.MethodGet, agentPath(id, "operations", url.PathEscape(operationID)), nil, &result)
	return result, err
}

// hostedRegistry is the subset of *peer.Registry the hosted service calls.
type hostedRegistry interface {
	AllocateAndRegister(ctx context.Context, params peer.AllocateParams) (proto.PeerID, proto.DisplayName, error)
	UpdateStatus(ctx context.Context, id proto.PeerID, status proto.PeerStatus) error
	UpdateTurnState(ctx context.Context, id proto.PeerID, ts proto.TurnState)
	MarkOffline(ctx context.Context, id proto.PeerID, terminal bool) (int, error)
}

// HostedDelivery is one settled submission, handed to observers: the
// delivery layer closes an ask with the runtime's answer, and job completion
// and the chat feed record the run.
type HostedDelivery struct {
	Kind          string // "ask" | "notify" | "broadcast"
	Text          string // the framed text the runtime received
	CorrelationID string
	FromPeerID    string
	FromPeerName  string
	ToPeerID      string
	ToPeerName    string
}

// Hosted registers managed peers and carries deliveries to their runtime.
type Hosted struct {
	reg     hostedRegistry
	runtime HostedRuntime
	store   *state.Store
	machine string

	// settled observers run in order once per submission the runtime settles.
	settled []HostedObserver

	// retry is the first reconnect delay after a failed wait.
	retry time.Duration

	mu       sync.Mutex
	inflight map[proto.PeerID]int
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func NewHosted(reg hostedRegistry, runtime HostedRuntime, store *state.Store, machine string) *Hosted {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hosted{reg: reg, runtime: runtime, store: store, machine: machine,
		retry: time.Second, inflight: map[proto.PeerID]int{}, ctx: ctx, cancel: cancel}
}

// HostedObserver sees one settled submission: done with the final text,
// unanswered, or refused with err.
type HostedObserver func(ctx context.Context, delivery HostedDelivery, result HostedResult, err error)

// OnSettled adds an observer. Wire observers before Restore and Deliver.
func (h *Hosted) OnSettled(observer HostedObserver) {
	h.settled = append(h.settled, observer)
}

// Close stops waiting. Unsettled operations stay running and resume on the
// next start.
func (h *Hosted) Close() {
	if h == nil {
		return
	}
	h.cancel()
	h.wg.Wait()
}

// Owns reports whether p is a hosted peer this service delivers to.
func (h *Hosted) Owns(p *proto.Peer) bool {
	return h != nil && p != nil && p.Backend == proto.AgentHosted
}

func hostedAllocateParams(circle, path string, role proto.PeerRole, machine string) peer.AllocateParams {
	idle := proto.TurnIdle
	return peer.AllocateParams{
		Circle:       circle,
		CircleSource: "spawn_hint",
		Backend:      proto.AgentHosted,
		Role:         role,
		Path:         &path,
		Machine:      machine,
		TurnState:    &idle,
		Metadata: map[string]any{
			"in_process":   true,
			"transport":    "hosted",
			"capabilities": []any{proto.CapRuntimeInbox},
		},
		Provenance: &proto.Provenance{Source: proto.SourceHosted, Initiator: proto.InitiatorAgent, Addressable: true},
	}
}

// HostedSpawnParams describes a new managed agent. Path is a label that names
// the peer; nothing runs locally. Workspace attaches a shared sandbox.
type HostedSpawnParams struct {
	Path      string
	Circle    string
	Role      proto.PeerRole
	Workspace *string
}

// Spawn registers a hosted peer and creates its runtime agent under the
// peer_id. A peer reclaimed by the same circle, path and role reattaches to
// its existing agent and conversation. A runtime refusal retires the peer.
func (h *Hosted) Spawn(ctx context.Context, params HostedSpawnParams) (proto.PeerID, proto.DisplayName, error) {
	if params.Role == "" {
		params.Role = proto.RoleAgent
	}
	id, name, err := h.reg.AllocateAndRegister(ctx, hostedAllocateParams(params.Circle, params.Path, params.Role, h.machine))
	if err != nil {
		return "", "", err
	}
	if err := h.runtime.CreateAgent(ctx, string(id), params.Workspace); err != nil {
		_, _ = h.reg.MarkOffline(ctx, id, true)
		return "", "", fmt.Errorf("create hosted agent: %w", err)
	}
	return id, name, nil
}

// Retire takes a hosted peer off the mesh for good. Its runtime agent and
// transcript remain on the hosted service.
func (h *Hosted) Retire(ctx context.Context, id proto.PeerID) error {
	_, err := h.reg.MarkOffline(ctx, id, true)
	return err
}

// Restore re-registers every hosted peer from its durable mapping and resumes
// waiting on submissions a previous daemon left unsettled. Retired peers stay
// retired.
func (h *Hosted) Restore(ctx context.Context) {
	if h.store == nil {
		return
	}
	mappings, err := h.store.LoadMappings(ctx)
	if err != nil {
		log.Printf("hosted: load mappings: %v", err)
		return
	}
	for _, m := range mappings {
		if m.Backend != proto.AgentHosted || m.Path == nil {
			continue
		}
		params := hostedAllocateParams(m.Circle, *m.Path, m.Role, h.machine)
		claim := m.SessionID
		params.ClaimedPeerID = &claim
		if _, _, err := h.reg.AllocateAndRegister(ctx, params); err != nil && !errors.Is(err, peer.ErrPeerRetired) {
			log.Printf("hosted: restore %s: %v", m.SessionID, err)
		}
	}
	for _, status := range []string{"queued", "running"} {
		ops, err := h.store.ListOperations(ctx, hostedDeliveryKind, status)
		if err != nil {
			continue
		}
		for _, op := range ops {
			h.await(op.OperationID, op.Target)
		}
	}
}

// Deliver submits text to the target's runtime and returns once the runtime
// has durably accepted it. The answer arrives later through settled.
func (h *Hosted) Deliver(ctx context.Context, kind, correlationID string, from *proto.Peer, fromName string, target *proto.Peer, text string) (map[string]any, error) {
	operationID := correlationID
	if operationID == "" {
		operationID = kind + "-" + uuid.NewString()[:8]
	}
	fromID, fromRole := "", ""
	if from != nil {
		fromID, fromRole = string(from.PeerID), string(from.Role)
	}
	record := map[string]any{
		"agent_id": string(target.PeerID), "operation_id": operationID, "kind": kind,
		"correlation_id": correlationID, "from_peer_id": fromID, "from_peer_name": fromName,
		"to_peer_id": string(target.PeerID), "to_peer_name": string(target.DisplayName),
	}
	framed := hooks.FormatInboundMessage(fromName, fromRole, string(target.DisplayName), kind, correlationID, text)
	record["text"] = framed
	opID, err := h.record(ctx, record)
	if err != nil {
		return nil, err
	}
	accepted, err := h.runtime.Submit(ctx, string(target.PeerID), operationID, framed,
		HostedSource{From: fromName, FromPeerID: fromID, Type: kind, CorrelationID: correlationID})
	if err != nil {
		h.fail(opID, "submit_failed", err.Error())
		return nil, fmt.Errorf("hosted submit to %s: %w", target.DisplayName, err)
	}
	h.await(opID, record)
	return map[string]any{"status": "accepted", "operation_id": operationID, "duplicate": !accepted}, nil
}

func (h *Hosted) record(ctx context.Context, target map[string]any) (string, error) {
	if h.store == nil {
		return "", nil
	}
	op, err := h.store.CreateOperation(ctx, hostedDeliveryKind, target, nil)
	if err != nil {
		return "", fmt.Errorf("persist hosted delivery: %w", err)
	}
	strategy := "hosted_submit"
	if _, err := h.store.StartAttempt(ctx, op.OperationID, &strategy, nil); err != nil {
		return "", fmt.Errorf("start hosted delivery: %w", err)
	}
	return op.OperationID, nil
}

func (h *Hosted) fail(opID, reason, detail string) {
	if h.store != nil && opID != "" {
		_, _ = h.store.FailOperation(context.Background(), opID, "", nil, map[string]any{"reason": reason, "detail": detail})
	}
}

// await waits for one submission to settle. It reconnects a dropped or
// expired wait with the same operation id, and stops without settling when
// the daemon closes so the next start resumes it.
func (h *Hosted) await(opID string, target map[string]any) {
	str := func(key string) string { value, _ := target[key].(string); return value }
	delivery := HostedDelivery{Kind: str("kind"), Text: str("text"), CorrelationID: str("correlation_id"),
		FromPeerID: str("from_peer_id"), FromPeerName: str("from_peer_name"),
		ToPeerID: str("to_peer_id"), ToPeerName: str("to_peer_name")}
	agentID, operationID := str("agent_id"), str("operation_id")
	peerID := proto.PeerID(delivery.ToPeerID)
	h.busy(peerID, +1)
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		backoff := h.retry
		for {
			attempt, cancel := context.WithTimeout(h.ctx, hostedWaitAttempt)
			result, err := h.runtime.Wait(attempt, agentID, operationID)
			cancel()
			if h.ctx.Err() != nil {
				return
			}
			var refused *HostedError
			if err != nil && !errors.As(err, &refused) {
				select {
				case <-h.ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, hostedWaitBackoffMax)
				continue
			}
			h.settle(opID, delivery, result, err)
			h.busy(peerID, -1)
			return
		}
	}()
}

func (h *Hosted) settle(opID string, delivery HostedDelivery, result HostedResult, err error) {
	ctx := context.Background()
	switch {
	case err != nil:
		h.fail(opID, "runtime_refused", err.Error())
	case result.Status != "done":
		h.fail(opID, "unanswered", result.Reason)
	case h.store != nil && opID != "":
		_, _ = h.store.CompleteOperation(ctx, opID, nil, map[string]any{"outcome": "done"})
	}
	for _, observer := range h.settled {
		observer(ctx, delivery, result, err)
	}
}

// busy keeps the peer busy while any submission is in flight.
func (h *Hosted) busy(id proto.PeerID, delta int) {
	h.mu.Lock()
	before := h.inflight[id]
	after := max(before+delta, 0)
	h.inflight[id] = after
	h.mu.Unlock()
	ctx := context.Background()
	switch {
	case before == 0 && after > 0:
		_ = h.reg.UpdateStatus(ctx, id, proto.StatusBusy)
		h.reg.UpdateTurnState(ctx, id, proto.TurnWorking)
	case before > 0 && after == 0:
		_ = h.reg.UpdateStatus(ctx, id, proto.StatusOnline)
		h.reg.UpdateTurnState(ctx, id, proto.TurnIdle)
	}
}
