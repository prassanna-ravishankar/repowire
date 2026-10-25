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
// Every submission is a hosted_delivery operation until the runtime reports
// it settled over the gateway (hosted_gateway.go). The runtime keeps the work
// and its report across its own restarts and resends an unacknowledged
// report on every connect, so a reply survives a daemon restart, or a run
// that finishes while the daemon is down.
// ============================================================================

const hostedDeliveryKind = "hosted_delivery"

// hostedRequestTimeout bounds one HTTP call to the runtime, so a runtime that
// accepts a connection and never answers cannot hold settlement or shutdown.
const hostedRequestTimeout = 30 * time.Second

// hostedSettleTimeout bounds one report's settlement. A settlement cut short
// is not acknowledged, so the runtime resends the report.
const hostedSettleTimeout = 2 * time.Minute

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
	CreateAgent(ctx context.Context, id, displayName string, workspace *string) error
	RetireAgent(ctx context.Context, id string) error
	Submit(ctx context.Context, id, operationID, text string, source HostedSource) (accepted bool, err error)
}

// HostedClient speaks the hosted runtime's HTTP API with its bearer token,
// naming this daemon's machine so agents it creates record their home.
type HostedClient struct {
	base    string
	token   string
	machine string
	http    *http.Client
}

func NewHostedClient(base, token, machine string) *HostedClient {
	return &HostedClient{base: strings.TrimRight(base, "/"), token: token, machine: machine, http: &http.Client{Timeout: hostedRequestTimeout}}
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
	req.Header.Set("X-Repowire-Machine", c.machine)
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

func (c *HostedClient) CreateAgent(ctx context.Context, id, displayName string, workspace *string) error {
	body := map[string]any{"id": id, "display_name": displayName}
	if workspace != nil {
		body["workspace"] = *workspace
	}
	return c.do(ctx, http.MethodPost, "/agents", body, nil)
}

// RetireAgent stops the runtime agent taking input. Its transcript remains.
func (c *HostedClient) RetireAgent(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, agentPath(id), nil, nil)
}

func (c *HostedClient) Submit(ctx context.Context, id, operationID, text string, source HostedSource) (bool, error) {
	var receipt struct {
		Accepted bool `json:"accepted"`
	}
	err := c.do(ctx, http.MethodPost, agentPath(id, "messages"),
		map[string]any{"operationId": operationID, "text": text, "source": source}, &receipt)
	return receipt.Accepted, err
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
	settled []namedObserver

	mu       sync.Mutex
	inflight map[proto.PeerID]int
	// settling serializes reports, so a resent report settles at most once.
	settling sync.Mutex
	// recording serializes finding or creating a delivery's record. It is not
	// settling: an observer may deliver (a reply to a hosted asker) while a
	// report holds settling.
	recording sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func NewHosted(reg hostedRegistry, runtime HostedRuntime, store *state.Store, machine string) *Hosted {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hosted{reg: reg, runtime: runtime, store: store, machine: machine,
		inflight: map[proto.PeerID]int{}, ctx: ctx, cancel: cancel}
}

// HostedObserver sees one settled submission: done with the final text, or
// unanswered with the runtime's reason. An error leaves the report
// unacknowledged, so the runtime resends it and the observer runs again.
type HostedObserver func(ctx context.Context, delivery HostedDelivery, result HostedResult) error

type namedObserver struct {
	name string
	run  HostedObserver
}

// OnSettled adds an observer under a name unique among the observers; a
// delivery records each name as its observer succeeds, so a replayed report
// runs only the observers that had not. Wire observers before Restore and
// Deliver.
func (h *Hosted) OnSettled(name string, observer HostedObserver) {
	h.settled = append(h.settled, namedObserver{name: name, run: observer})
}

// Close drops the gateway. Unsettled operations stay running; their reports
// arrive after the next connect.
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
	if err := h.runtime.CreateAgent(ctx, string(id), string(name), params.Workspace); err != nil {
		_, _ = h.reg.MarkOffline(ctx, id, true)
		return "", "", fmt.Errorf("create hosted agent: %w", err)
	}
	return id, name, nil
}

// Retire takes a hosted peer off the mesh for good: the runtime agent stops
// taking input first, then the peer is retired here, so a failed remote call
// leaves a usable peer rather than an agent the mesh can no longer reach. An
// agent the runtime does not know is retired here regardless. The transcript
// remains on the hosted service.
func (h *Hosted) Retire(ctx context.Context, id proto.PeerID) error {
	var refused *HostedError
	if err := h.runtime.RetireAgent(ctx, string(id)); err != nil && !(errors.As(err, &refused) && refused.Status == http.StatusNotFound) {
		return fmt.Errorf("retire hosted agent %s: %w", id, err)
	}
	_, err := h.reg.MarkOffline(ctx, id, true)
	return err
}

// Restore re-registers every hosted peer from its durable mapping and marks
// it busy for submissions a previous daemon left unsettled; their reports
// arrive once the gateway connects. Retired peers stay retired.
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
			to, _ := op.Target["to_peer_id"].(string)
			h.busy(proto.PeerID(to), +1)
		}
	}
}

// Deliver submits text to the target's runtime and returns once the runtime
// has durably accepted it. The answer arrives later as a report.
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
	opID, reused, err := h.record(ctx, record)
	if err != nil {
		return nil, err
	}
	// The peer turns busy before the runtime can report, so a report that
	// lands while Submit is still returning brings it back to idle. A resend
	// of an open delivery is already counted.
	if !reused {
		h.busy(target.PeerID, +1)
	}
	accepted, err := h.runtime.Submit(ctx, string(target.PeerID), operationID, framed,
		HostedSource{From: fromName, FromPeerID: fromID, Type: kind, CorrelationID: correlationID})
	if err != nil {
		if !reused {
			h.fail(opID, "submit_failed", err.Error())
			h.busy(target.PeerID, -1)
		}
		return nil, fmt.Errorf("hosted submit to %s: %w", target.DisplayName, err)
	}
	if !accepted && !reused && h.store != nil && opID != "" {
		// The runtime already settled this operation, against an earlier
		// record; this one settles now.
		_, _ = h.store.CompleteOperation(ctx, opID, nil, map[string]any{"outcome": "duplicate"})
		h.busy(target.PeerID, -1)
	}
	return map[string]any{"status": "accepted", "operation_id": operationID, "duplicate": !accepted}, nil
}

// record returns the open record for the delivery's agent and operation id,
// reused, or creates one, so a resend never adds a second record for the
// runtime's one report.
func (h *Hosted) record(ctx context.Context, target map[string]any) (string, bool, error) {
	if h.store == nil {
		return "", false, nil
	}
	h.recording.Lock()
	defer h.recording.Unlock()
	open, err := h.unsettled(ctx, target["agent_id"].(string), target["operation_id"].(string))
	if err != nil {
		return "", false, err
	}
	if open != nil {
		return open.OperationID, true, nil
	}
	op, err := h.store.CreateOperation(ctx, hostedDeliveryKind, target, nil)
	if err != nil {
		return "", false, fmt.Errorf("persist hosted delivery: %w", err)
	}
	strategy := "hosted_submit"
	if _, err := h.store.StartAttempt(ctx, op.OperationID, &strategy, nil); err != nil {
		return "", false, fmt.Errorf("start hosted delivery: %w", err)
	}
	return op.OperationID, false, nil
}

func (h *Hosted) fail(opID, reason, detail string) {
	if h.store != nil && opID != "" {
		_, _ = h.store.FailOperation(context.Background(), opID, "", nil, map[string]any{"reason": reason, "detail": detail})
	}
}

// Report settles the submission the runtime reports for agent and operation
// id. Each observer's success is recorded on the open operation, and the
// operation turns terminal only after all of them, so a report cut short
// (an observer failed, or the daemon stopped) is resent and resumes at the
// first observer that had not succeeded. An observer stopped partway runs
// again from its start. A nil error means the report is handled, or was already settled or is
// unknown here, and the runtime may drop it; an error means it must resend.
func (h *Hosted) Report(ctx context.Context, agentID, operationID string, result HostedResult) error {
	if h.store == nil {
		return nil
	}
	h.settling.Lock()
	defer h.settling.Unlock()
	ctx, cancel := context.WithTimeout(ctx, hostedSettleTimeout)
	defer cancel()
	op, err := h.unsettled(ctx, agentID, operationID)
	if err != nil || op == nil {
		return err
	}
	str := func(key string) string { value, _ := op.Target[key].(string); return value }
	delivery := HostedDelivery{Kind: str("kind"), Text: str("text"), CorrelationID: str("correlation_id"),
		FromPeerID: str("from_peer_id"), FromPeerName: str("from_peer_name"),
		ToPeerID: str("to_peer_id"), ToPeerName: str("to_peer_name")}
	steps, _ := op.Result["steps"].(map[string]any)
	for _, observer := range h.settled {
		if _, done := steps[observer.name]; done {
			continue
		}
		if err := observer.run(ctx, delivery, result); err != nil {
			return fmt.Errorf("settle hosted delivery %s: %s: %w", op.OperationID, observer.name, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		marked, err := h.store.MarkOperationStep(ctx, op.OperationID, observer.name)
		if err != nil || marked == nil {
			return fmt.Errorf("settle hosted delivery %s: record %s: %w", op.OperationID, observer.name, err)
		}
		op = marked
		steps, _ = op.Result["steps"].(map[string]any)
	}
	report := map[string]any{"outcome": result.Status, "reason": result.Reason, "text": result.Text, "steps": steps}
	if result.Status != "done" {
		_, err = h.store.FailOperation(ctx, op.OperationID, "", nil, report)
	} else {
		_, err = h.store.CompleteOperation(ctx, op.OperationID, nil, report)
	}
	if err != nil {
		return fmt.Errorf("settle hosted delivery %s: %w", op.OperationID, err)
	}
	h.busy(proto.PeerID(delivery.ToPeerID), -1)
	return nil
}

// unsettled finds the open operation for agent and operation id, or nil.
func (h *Hosted) unsettled(ctx context.Context, agentID, operationID string) (*state.Operation, error) {
	for _, status := range []string{"queued", "running"} {
		ops, err := h.store.ListOperations(ctx, hostedDeliveryKind, status)
		if err != nil {
			return nil, fmt.Errorf("find hosted delivery %s/%s: %w", agentID, operationID, err)
		}
		for _, op := range ops {
			if op.Target["agent_id"] == agentID && op.Target["operation_id"] == operationID {
				return op, nil
			}
		}
	}
	return nil, nil
}

// busy keeps the peer busy while any submission is in flight. The status
// change happens under the count's lock, so concurrent deliveries and reports
// apply their transitions in the order they counted.
func (h *Hosted) busy(id proto.PeerID, delta int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	before := h.inflight[id]
	after := max(before+delta, 0)
	h.inflight[id] = after
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
