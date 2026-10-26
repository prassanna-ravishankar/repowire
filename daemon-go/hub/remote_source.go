package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/relay"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// The asking side of cross-machine circles (rfcs/cross-machine-circles.md).
// remote_outbound is authoritative; the tracker entry is a projection so
// /asks/pending, reminders and wait_on_ack keep working by correlation id.

const (
	remoteCheckEvery   = 30 * time.Second
	remoteCheckBatch   = 8
	remoteCheckBudget  = 2 * time.Second
	remoteDeliverBatch = 8
	remoteFanout       = 16
	remoteListBudget   = 2 * time.Second
	remoteSendTimeout  = 30 * time.Second
)

// errNotRemote means an address names no other machine: route it locally.
var errNotRemote = errors.New("not a remote address")

// remoteAddress splits name@machine when machine is another online machine of
// this owner. A local peer with the same literal name makes it ambiguous.
func (h *Hub) remoteAddress(ctx context.Context, address string) (name, machine string, err error) {
	at := strings.LastIndex(address, "@")
	if h.remote == nil || at <= 0 || at == len(address)-1 {
		return "", "", errNotRemote
	}
	name, machine = address[:at], address[at+1:]
	if machine == h.remote.self {
		return "", "", errNotRemote
	}
	listCtx, cancel := context.WithTimeout(ctx, remoteListBudget)
	defer cancel()
	machines, err := h.remote.link.Machines(listCtx)
	if err != nil || !contains(machines, machine) {
		return "", "", errNotRemote
	}
	if local, _ := h.reg.GetPeerByName(address, nil); local != nil {
		return "", "", routeErr(http.StatusConflict, fmt.Sprintf("%q names both a local peer and %s on %s", address, name, machine))
	}
	return name, machine, nil
}

// remoteCaller is the vouched principal for a local caller: its registry
// entry, never tool arguments.
func (h *Hub) remoteCaller(caller string) (*proto.Peer, state.RemotePrincipal, error) {
	from, _ := h.reg.GetPeerByName(caller, nil)
	if from == nil {
		return nil, state.RemotePrincipal{}, routeErr(http.StatusForbidden, "cross-machine messages need a registered caller")
	}
	if !proto.SharedCircle(from.Circle) || from.Role.BypassesCircles() {
		return nil, state.RemotePrincipal{}, routeErr(http.StatusForbidden, fmt.Sprintf("circle %q is local to this machine; name the circle to share it", from.Circle))
	}
	return from, state.RemotePrincipal{PeerID: string(from.PeerID), DisplayName: string(from.DisplayName), Circle: from.Circle}, nil
}

// machineCall sends one /remote request and decodes a 2xx body into out.
func (m *remoteMesh) machineCall(ctx context.Context, machine, path string, body, out any) (int, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	status, raw, err := m.link.MachineRequest(ctx, machine, http.MethodPost, path, encoded)
	if err != nil {
		return 0, err
	}
	if status >= 200 && status < 300 && out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return status, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	if status >= 300 {
		return status, fmt.Errorf("%s on %s: HTTP %d: %s", path, machine, status, strings.TrimSpace(string(raw)))
	}
	return status, nil
}

type remotePeersResponse struct {
	Peers      []remotePeer `json:"peers"`
	StoreEpoch string       `json:"store_epoch"`
}

// resolveRemote asks the target machine itself for one peer, recording its
// store epoch before anything is sent.
func (h *Hub) resolveRemote(ctx context.Context, from state.RemotePrincipal, name, machine string) (remotePeer, string, error) {
	var out remotePeersResponse
	status, err := h.remote.machineCall(ctx, machine, "/remote/peers", remotePeersRequest{From: from, Name: name}, &out)
	if err != nil {
		if status >= 400 && status < 500 {
			return remotePeer{}, "", routeErr(status, err.Error())
		}
		return remotePeer{}, "", routeErr(http.StatusBadGateway, err.Error())
	}
	switch len(out.Peers) {
	case 0:
		return remotePeer{}, "", routeErr(http.StatusNotFound, fmt.Sprintf("no peer %s in circle %s on %s", name, from.Circle, machine))
	case 1:
		return out.Peers[0], out.StoreEpoch, nil
	}
	return remotePeer{}, "", routeErr(http.StatusConflict, fmt.Sprintf("%s is ambiguous on %s; use its peer_id", name, machine))
}

// remoteName is how a peer on another machine is shown here.
func remoteName(machine string, target remotePeer) string {
	return target.DisplayName + "@" + machine
}

// openRemoteAsk sends an ask to a peer on another machine. The envelope is
// committed before the send, and an unknown outcome is never resent.
func (h *Hub) openRemoteAsk(ctx context.Context, req AskRequest, name, machine string) (AskResponse, error) {
	if req.ReplyTo != nil || len(req.Attachments) > 0 || req.Question != nil {
		return AskResponse{}, routeErr(http.StatusUnprocessableEntity, "cross-machine asks carry text only (no reply_to, attachments or questions)")
	}
	_, from, err := h.remoteCaller(req.FromPeer)
	if err != nil {
		return AskResponse{}, err
	}
	target, epoch, err := h.resolveRemote(ctx, from, name, machine)
	if err != nil {
		return AskResponse{}, err
	}
	toName := remoteName(machine, target)
	row := state.RemoteOutbound{
		ID: newRemoteID("ask"), Machine: machine, StoreEpoch: epoch, ToPeerID: target.PeerID, ToName: toName,
		From: from, Text: req.Text, CreatedAt: time.Now().Unix(),
	}
	if err := h.store.InsertOutbound(ctx, row); err != nil {
		return AskResponse{}, routeErr(http.StatusInternalServerError, err.Error())
	}
	h.registerOutboundAsk(ctx, row)
	h.traceAsk(ctx, row.ID, "created", "", nil, map[string]any{"to_peer": toName, "from_peer": from.PeerID, "machine": machine})

	sendCtx, cancel := context.WithTimeout(ctx, remoteSendTimeout)
	defer cancel()
	var receipt remoteReceipt
	status, err := h.remote.machineCall(sendCtx, machine, "/remote/ask", remoteCreateRequest{ID: row.ID, From: from, ToPeer: target.PeerID, Text: req.Text, ExpectedEpoch: epoch}, &receipt)
	switch {
	case err == nil:
		h.applyReceipt(ctx, row, receipt, true)
		return AskResponse{CorrelationID: row.ID}, nil
	case errors.Is(err, relay.ErrRelayDown) || (status >= 400 && status < 500 && status != http.StatusTooManyRequests):
		// Definite: nothing was created on the target.
		h.settleOutbound(ctx, row, "refused", nil)
		_, _ = h.ask.asks.Close(ctx, row.ID, "send_failed")
		_ = h.store.MarkOutboundDelivered(ctx, row.ID)
		if status == 0 {
			status = http.StatusServiceUnavailable
		}
		return AskResponse{}, routeErr(status, err.Error())
	case status == http.StatusTooManyRequests:
		// The target refused at admission: also definite, but worth a retry.
		h.settleOutbound(ctx, row, "refused", nil)
		_, _ = h.ask.asks.Close(ctx, row.ID, "send_failed")
		_ = h.store.MarkOutboundDelivered(ctx, row.ID)
		return AskResponse{}, routeErr(status, err.Error())
	default:
		_, _ = h.store.MarkOutboundUncertain(ctx, row.ID)
		detail := fmt.Sprintf("delivery to %s is uncertain (%v); do not resend, wait_on_ack or the next check settles it", toName, err)
		return AskResponse{CorrelationID: row.ID, Error: &detail}, nil
	}
}

// registerOutboundAsk (re)builds the tracker projection of an outbound row.
func (h *Hub) registerOutboundAsk(ctx context.Context, row state.RemoteOutbound) {
	if _, err := h.ask.asks.Register(ctx, service.RegisterAskParams{
		CorrelationID: row.ID,
		FromPeerID:    proto.PeerID(row.From.PeerID),
		FromPeerName:  proto.DisplayName(row.From.DisplayName),
		ToPeerID:      proto.RemotePeerID(row.Machine, row.ToPeerID),
		ToPeerName:    proto.DisplayName(row.ToName),
		Text:          row.Text,
		CreatedAt:     time.Unix(row.CreatedAt, 0),
	}); err != nil {
		log.Printf("remote %s: register projection: %v", row.ID, err)
	}
}

// applyReceipt takes a create response or a read: a settled receipt goes
// through the one terminal transition, anything else leaves the row as is.
// push is false when the caller is the asker taking the reply itself.
func (h *Hub) applyReceipt(ctx context.Context, row state.RemoteOutbound, receipt remoteReceipt, push bool) {
	if receipt.StoreEpoch != "" && receipt.StoreEpoch != row.StoreEpoch {
		h.settleOutboundPush(ctx, row, "result_lost", nil, push)
		return
	}
	if receipt.State == state.RemoteSettled {
		h.settleOutboundPush(ctx, row, receipt.CloseReason, receipt.Reply, push)
	}
}

// settleOutbound writes the first result and, when this call wins, tries to
// deliver the reply to the asker.
func (h *Hub) settleOutbound(ctx context.Context, row state.RemoteOutbound, reason string, reply *string) {
	h.settleOutboundPush(ctx, row, reason, reply, true)
}

func (h *Hub) settleOutboundPush(ctx context.Context, row state.RemoteOutbound, reason string, reply *string, push bool) {
	won, err := h.store.SettleOutbound(ctx, row.ID, reason, reply)
	if err != nil {
		log.Printf("remote %s: settle: %v", row.ID, err)
		return
	}
	if won && push {
		row.State, row.CloseReason, row.Reply = state.RemoteSettled, reason, reply
		h.deliverOutbound(ctx, row)
	}
}

// deliverOutbound pushes a settled result to the asker and marks it
// delivered. The push and the mark are separate writes, so a crash between
// them repeats the reply (at least once, keyed by correlation id).
func (h *Hub) deliverOutbound(ctx context.Context, row state.RemoteOutbound) {
	reason := row.CloseReason
	if reason == "ack" && row.Reply == nil {
		_, _ = h.ask.asks.Close(ctx, row.ID, "ack")
		_ = h.store.MarkOutboundDelivered(ctx, row.ID)
		return
	}
	body := derefOr(row.Reply, "")
	var framed string
	switch reason {
	case "ack", "ack_with_msg":
		framed = fmt.Sprintf("[ack #%s from @%s] %s", row.ID, row.ToName, body)
	case "declined":
		framed = fmt.Sprintf("[declined #%s from @%s] %s", row.ID, row.ToName, body)
	default:
		framed = fmt.Sprintf("[closed #%s @%s: %s] %s", row.ID, row.ToName, reason, body)
	}
	res, err := h.ask.delivery.Notify(ctx, service.NotifyParams{
		FromPeer: row.ToName, ToPeer: row.From.PeerID, Text: framed, BypassCircle: true,
		CorrelationID: row.ID, ThreadText: body,
	})
	if err != nil || (!res.Delivered() && !res.Queued()) {
		return // stays undelivered; the asker's next hook request retries
	}
	if row.Reply != nil {
		h.ask.asks.CaptureReply(ctx, row.ID, *row.Reply, nil)
	}
	_, _ = h.ask.asks.Close(ctx, row.ID, reason)
	_ = h.store.MarkOutboundDelivered(ctx, row.ID)
}

// remoteSettled is the target pushing a result. Only the machine the row
// names may settle it, with the epoch the row recorded.
func (h *Hub) remoteSettled(w http.ResponseWriter, r *http.Request) {
	var req remoteSettledRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	row, err := h.store.GetOutbound(ctx, req.ID)
	if err != nil || row.Machine != relay.RemoteMachine(ctx) {
		writeJSONError(w, http.StatusNotFound, "no such request from this machine")
		return
	}
	if req.CloseReason == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "close_reason is required")
		return
	}
	if req.StoreEpoch != row.StoreEpoch {
		h.settleOutbound(ctx, row, "result_lost", nil)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	h.settleOutbound(ctx, row, req.CloseReason, req.Reply)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// readOutbound reads a row back from its target, waiting up to wait. It
// applies whatever the target answers for and leaves the row alone when the
// target cannot be reached.
func (h *Hub) readOutbound(ctx context.Context, row state.RemoteOutbound, wait time.Duration, push bool) {
	if _, busy := h.remote.checking.LoadOrStore(row.ID, true); busy {
		return
	}
	defer h.remote.checking.Delete(row.ID)
	var receipt remoteReceipt
	status, err := h.remote.machineCall(ctx, row.Machine, "/remote/asks/"+row.ID, remoteReadRequest{From: row.From, Wait: wait.Seconds()}, &receipt)
	switch {
	case err == nil:
		h.applyReceipt(ctx, row, receipt, push)
	case status == http.StatusGone:
		h.settleOutboundPush(ctx, row, "result_lost", nil, push)
	}
}

// reconcileRemote runs on the asker's own hook requests: it reads back a
// fair, bounded batch of the asker's unsettled rows and retries delivering
// settled replies, all inside one short budget.
func (h *Hub) reconcileRemote(ctx context.Context, asker proto.PeerID) {
	if h.remote == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, remoteCheckBudget)
	defer cancel()
	now := time.Now()
	rows, err := h.store.ClaimOutboundChecks(ctx, string(asker), now.Add(-remoteCheckEvery).Unix(), now.Unix(), remoteCheckBatch)
	if err != nil {
		log.Printf("remote: claim checks: %v", err)
	}
	var wg sync.WaitGroup
	for _, row := range rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.readOutbound(ctx, row, 0, true)
		}()
	}
	wg.Wait()
	undelivered, _ := h.store.UndeliveredOutbound(ctx, string(asker), remoteDeliverBatch)
	for _, row := range undelivered {
		h.deliverOutbound(ctx, row)
	}
}

// waitRemote is wait_on_ack for an outbound row: answer from the row when it
// is settled, otherwise long-poll the target until the caller's timeout.
func (h *Hub) waitRemote(ctx context.Context, row state.RemoteOutbound, caller string, timeout time.Duration) (AskWaitResponse, error) {
	if caller != row.From.PeerID && caller != row.From.DisplayName {
		return AskWaitResponse{}, routeErr(http.StatusForbidden, map[string]any{"error": "not_the_asker", "correlation_id": row.ID, "asker": row.From.DisplayName})
	}
	deadline := time.Now().Add(timeout)
	for row.State != state.RemoteSettled {
		remaining := time.Until(deadline)
		if remaining <= 0 || ctx.Err() != nil {
			return AskWaitResponse{CorrelationID: row.ID, Status: "pending"}, nil
		}
		callCtx, cancel := context.WithTimeout(ctx, min(remaining, remoteWaitCap)+5*time.Second)
		h.readOutbound(callCtx, row, min(remaining, remoteWaitCap), false)
		cancel()
		next, err := h.store.GetOutbound(ctx, row.ID)
		if err != nil {
			return AskWaitResponse{}, routeErr(http.StatusNotFound, "No ask with correlation_id: "+row.ID)
		}
		if next.State != state.RemoteSettled && time.Until(deadline) > 0 {
			// Unreachable target or still pending: do not spin on an offline machine.
			select {
			case <-ctx.Done():
			case <-time.After(min(time.Second, time.Until(deadline))):
			}
		}
		row = next
	}
	// The waiter is the asker taking the reply: like a local pull, nothing is
	// pushed afterwards.
	_ = h.store.MarkOutboundDelivered(ctx, row.ID)
	if row.Reply != nil {
		h.ask.asks.CaptureReply(ctx, row.ID, *row.Reply, nil)
	}
	_, _ = h.ask.asks.Close(ctx, row.ID, row.CloseReason)
	return AskWaitResponse{CorrelationID: row.ID, Status: "resolved", Reply: row.Reply, CloseReason: strPtr(row.CloseReason), Responder: strPtr(row.ToName)}, nil
}

// remoteNotify sends one notify to a peer on another machine.
func (h *Hub) remoteNotify(ctx context.Context, caller, name, machine, text string) (string, string, error) {
	_, from, err := h.remoteCaller(caller)
	if err != nil {
		return "", "", err
	}
	target, epoch, err := h.resolveRemote(ctx, from, name, machine)
	if err != nil {
		return "", "", err
	}
	return h.sendRemoteNotify(ctx, from, machine, target, epoch, text)
}

func (h *Hub) sendRemoteNotify(ctx context.Context, from state.RemotePrincipal, machine string, target remotePeer, epoch, text string) (string, string, error) {
	id := newRemoteID("notif")
	var receipt remoteReceipt
	status, err := h.remote.machineCall(ctx, machine, "/remote/notify", remoteCreateRequest{ID: id, From: from, ToPeer: target.PeerID, Text: text, ExpectedEpoch: epoch}, &receipt)
	switch {
	case err == nil && receipt.State == state.RemoteDelivered:
		return id, "delivered", nil
	case err == nil:
		return id, receipt.CloseReason, nil
	case errors.Is(err, relay.ErrRelayDown) || (status >= 400 && status < 500):
		return id, "", routeErr(max(status, http.StatusServiceUnavailable), err.Error())
	default:
		return id, "uncertain", nil
	}
}

// remoteListing is discovery's result: peers by machine and the machines it
// could not include.
type remoteListing struct {
	rows    []remoteRow
	partial []string
}

type remoteRow struct {
	machine string
	epoch   string
	peer    remotePeer
}

// discoverRemote lists peers in the caller's shared circle on up to
// remoteFanout other machines, within one budget for enumeration and fan-out.
func (h *Hub) discoverRemote(ctx context.Context, from state.RemotePrincipal) remoteListing {
	ctx, cancel := context.WithTimeout(ctx, remoteListBudget)
	defer cancel()
	machines, err := h.remote.link.Machines(ctx)
	if err != nil {
		return remoteListing{partial: []string{"(machine list unavailable)"}}
	}
	sort.Strings(machines)
	var out remoteListing
	if len(machines) > remoteFanout {
		out.partial = append(out.partial, machines[remoteFanout:]...)
		machines = machines[:remoteFanout]
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, machine := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var resp remotePeersResponse
			_, err := h.remote.machineCall(ctx, machine, "/remote/peers", remotePeersRequest{From: from}, &resp)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				out.partial = append(out.partial, machine)
				return
			}
			for _, p := range resp.Peers {
				out.rows = append(out.rows, remoteRow{machine: machine, epoch: resp.StoreEpoch, peer: p})
			}
		}()
	}
	wg.Wait()
	sort.Strings(out.partial)
	sort.Slice(out.rows, func(i, j int) bool {
		if out.rows[i].machine != out.rows[j].machine {
			return out.rows[i].machine < out.rows[j].machine
		}
		return out.rows[i].peer.DisplayName < out.rows[j].peer.DisplayName
	})
	return out
}

// broadcastRemote notifies every discovered peer in the caller's shared
// circle on other machines and reports the outcome per target.
func (h *Hub) broadcastRemote(ctx context.Context, from state.RemotePrincipal, text string) string {
	listing := h.discoverRemote(ctx, from)
	var mu sync.Mutex
	var sent, failed, uncertain []string
	var wg sync.WaitGroup
	slots := make(chan struct{}, remoteCheckBatch)
	for _, row := range listing.rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			callCtx, cancel := context.WithTimeout(ctx, remoteSendTimeout)
			defer cancel()
			_, outcome, err := h.sendRemoteNotify(callCtx, from, row.machine, row.peer, row.epoch, text)
			name := remoteName(row.machine, row.peer)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				failed = append(failed, name)
			case outcome == "delivered":
				sent = append(sent, name)
			case outcome == "uncertain":
				uncertain = append(uncertain, name)
			default:
				failed = append(failed, name)
			}
		}()
	}
	wg.Wait()
	out := fmt.Sprintf(" remote_sent=%d remote_failed=%d remote_uncertain=%d", len(sent), len(failed), len(uncertain))
	for _, group := range []struct {
		label string
		names []string
	}{{"failed", failed}, {"uncertain", uncertain}} {
		if len(group.names) > 0 {
			sort.Strings(group.names)
			out += fmt.Sprintf(" %s_targets=%s", group.label, strings.Join(group.names, ","))
		}
	}
	if len(listing.partial) > 0 {
		out += " partial=" + strings.Join(listing.partial, ",")
	}
	return out
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
