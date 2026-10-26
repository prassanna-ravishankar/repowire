package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/relay"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// Cross-machine circles (rfcs/cross-machine-circles.md). This file is the
// target side: the /remote API another machine reaches through the relay,
// served in-process from the relay client and never on the HTTP mux.
// remote_source.go is the asking side.

const (
	remoteRetention     = 7 * 24 * time.Hour
	remoteCreateHorizon = 6 * 24 * time.Hour
	remoteFutureSkew    = 5 * time.Minute
	remoteWaitCap       = 20 * time.Second
)

// machineLink is the relay client's cross-machine surface.
type machineLink interface {
	MachineRequest(ctx context.Context, machine, method, path string, body []byte) (int, []byte, error)
	Machines(ctx context.Context) ([]string, error)
}

type remoteMesh struct {
	link  machineLink
	store *state.Store
	self  string // this machine's daemon_id
	epoch string

	checking sync.Map // outbound ids being read back, single-flight
	pruneMu  sync.Mutex
	pruned   time.Time
}

// WithRemote joins this daemon to its owner's other machines. It needs the
// ask lifecycle and the store, rehydrates records a restart left open, and
// guards every close of a remote-origin ask behind its durable result.
func (h *Hub) WithRemote(ctx context.Context, link machineLink, self string) error {
	if h.ask == nil || h.store == nil {
		return errors.New("remote: ask lifecycle and store are required")
	}
	epoch, err := h.store.StoreEpoch(ctx)
	if err != nil {
		return err
	}
	h.remote = &remoteMesh{link: link, store: h.store, self: self, epoch: epoch}
	h.ask.asks.SetCloseGuard(h.guardRemoteClose)
	h.remote.pruneLazily(ctx)
	return h.recoverRemote(ctx)
}

// RemoteHandler is the /remote API, for relay.Client.WithRemoteHandler.
func (h *Hub) RemoteHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /remote/peers", h.remotePeers)
	mux.HandleFunc("POST /remote/ask", h.remoteCreate("ask"))
	mux.HandleFunc("POST /remote/notify", h.remoteCreate("notify"))
	mux.HandleFunc("POST /remote/asks/{id}", h.remoteRead)
	mux.HandleFunc("POST /remote/settled", h.remoteSettled)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.remote == nil || relay.RemoteMachine(r.Context()) == "" {
			writeJSONError(w, http.StatusForbidden, "remote requests come only through the relay")
			return
		}
		h.remote.pruneLazily(r.Context())
		mux.ServeHTTP(w, r)
	})
}

// remoteReceipt is a record's state as the target reports it.
type remoteReceipt struct {
	ID          string  `json:"id"`
	State       string  `json:"state"`
	CloseReason string  `json:"close_reason,omitempty"`
	Reply       *string `json:"reply,omitempty"`
	Responder   string  `json:"responder,omitempty"`
	StoreEpoch  string  `json:"store_epoch"`
}

type remotePeer struct {
	PeerID      string `json:"peer_id"`
	DisplayName string `json:"display_name"`
	Circle      string `json:"circle"`
	Status      string `json:"status"`
	Backend     string `json:"backend"`
	Description string `json:"description,omitempty"`
	Project     string `json:"project,omitempty"`
}

type remotePeersRequest struct {
	From state.RemotePrincipal `json:"from"`
	Name string                `json:"name,omitempty"`
}

type remoteCreateRequest struct {
	ID            string                `json:"id"`
	From          state.RemotePrincipal `json:"from"`
	ToPeer        string                `json:"to_peer"`
	Text          string                `json:"text"`
	ExpectedEpoch string                `json:"expected_epoch"`
}

type remoteReadRequest struct {
	From state.RemotePrincipal `json:"from"`
	Wait float64               `json:"wait"`
}

type remoteSettledRequest struct {
	ID          string  `json:"id"`
	CloseReason string  `json:"close_reason"`
	Reply       *string `json:"reply,omitempty"`
	StoreEpoch  string  `json:"store_epoch"`
}

func (m *remoteMesh) receipt(r state.RemoteInbound, responder string) remoteReceipt {
	return remoteReceipt{ID: r.ID, State: r.State, CloseReason: r.CloseReason, Reply: r.Reply, Responder: responder, StoreEpoch: m.epoch}
}

// remoteTarget is a local peer a remote principal may reach: in the
// principal's shared circle, addressable, and with no circle-bypassing role.
func (h *Hub) remoteTarget(from state.RemotePrincipal, p *proto.Peer) bool {
	return p != nil && p.Circle == from.Circle && !p.Role.BypassesCircles() &&
		proto.RequireAddressable(nil, p) == nil && h.listedByDefault(p) &&
		(p.Status == proto.StatusOnline || p.Status == proto.StatusBusy)
}

func validPrincipal(from state.RemotePrincipal) error {
	if from.PeerID == "" || from.DisplayName == "" {
		return errors.New("from must name the sending peer")
	}
	if !proto.SharedCircle(from.Circle) {
		return fmt.Errorf("circle %q is local to its machine", from.Circle)
	}
	return nil
}

func (h *Hub) remotePeers(w http.ResponseWriter, r *http.Request) {
	var req remotePeersRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := validPrincipal(req.From); err != nil {
		writeJSONError(w, http.StatusForbidden, err.Error())
		return
	}
	peers := []remotePeer{}
	for _, p := range h.reg.GetAllPeers() {
		if !h.remoteTarget(req.From, p) || (req.Name != "" && string(p.DisplayName) != req.Name && string(p.PeerID) != req.Name) {
			continue
		}
		project, _ := p.Metadata["project"].(string)
		peers = append(peers, remotePeer{PeerID: string(p.PeerID), DisplayName: string(p.DisplayName), Circle: p.Circle, Status: string(p.Status), Backend: string(p.Backend), Description: p.Description, Project: project})
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": peers, "store_epoch": h.remote.epoch})
}

// remoteIDTime reads the creation time an id carries: <kind>-<unix>-<32 hex>.
func remoteIDTime(kind, id string) (int64, bool) {
	rest, ok := strings.CutPrefix(id, kind+"-")
	if !ok {
		return 0, false
	}
	stamp, token, ok := strings.Cut(rest, "-")
	if !ok || len(token) != 32 || strings.Trim(token, "0123456789abcdef") != "" {
		return 0, false
	}
	unix, err := strconv.ParseInt(stamp, 10, 64)
	return unix, err == nil && unix > 0
}

func newRemoteID(kind string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%s", kind, time.Now().Unix(), hex.EncodeToString(b[:]))
}

func remoteIDKind(id string) string {
	if strings.HasPrefix(id, "notif-") {
		return "notify"
	}
	return "ask"
}

func idPrefix(kind string) string {
	if kind == "notify" {
		return "notif"
	}
	return "ask"
}

// remoteCreate records a request before dispatching it, then dispatches at
// most once. A known id returns its receipt and dispatches nothing.
func (h *Hub) remoteCreate(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req remoteCreateRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		ctx, fromMachine, now := r.Context(), relay.RemoteMachine(r.Context()), time.Now()
		created, ok := remoteIDTime(idPrefix(kind), req.ID)
		switch {
		case !ok:
			writeJSONError(w, http.StatusBadRequest, "malformed id")
			return
		case created < now.Add(-remoteCreateHorizon).Unix() || created > now.Add(remoteFutureSkew).Unix():
			writeJSONError(w, http.StatusBadRequest, "stale id")
			return
		case req.ExpectedEpoch != h.remote.epoch:
			writeJSONError(w, http.StatusConflict, map[string]any{"error": "epoch_changed", "store_epoch": h.remote.epoch})
			return
		}
		if err := validPrincipal(req.From); err != nil {
			writeJSONError(w, http.StatusForbidden, err.Error())
			return
		}
		target, found := h.reg.GetPeer(proto.PeerID(req.ToPeer))
		if !found || !h.remoteTarget(req.From, target) {
			writeJSONError(w, http.StatusNotFound, "no reachable peer "+req.ToPeer+" in circle "+req.From.Circle)
			return
		}
		record, fresh, err := h.store.CreateInbound(ctx, state.RemoteInbound{
			ID: req.ID, Kind: kind, FromMachine: fromMachine, From: req.From,
			ToPeerID: string(target.PeerID), Text: req.Text, CreatedAt: created,
		})
		if errors.Is(err, state.ErrRemoteConflict) {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if fresh {
			// Dispatch is not cut short by the caller going away: a send
			// abandoned halfway is exactly the ambiguity to avoid.
			if record, err = h.dispatchRemote(context.WithoutCancel(ctx), record, target); err != nil {
				writeJSONError(w, http.StatusInternalServerError, "outcome not recorded: "+err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, h.remote.receipt(record, string(target.DisplayName)))
	}
}

// remoteSender is how a remote principal is named to a local agent.
func remoteSender(from state.RemotePrincipal, machine string) string {
	return from.DisplayName + "@" + machine
}

// dispatchRemote hands a freshly recorded request to the local transport,
// once, and returns the record as committed afterwards. A definite refusal
// settles it not_delivered; a send that may have landed never does: an ask
// stays open for its recipient (the TTL ends it if nothing came through) and
// a notify settles uncertain. A failed write is an error, so the source
// treats the outcome as unknown rather than trusting a receipt nobody kept.
func (h *Hub) dispatchRemote(ctx context.Context, record state.RemoteInbound, target *proto.Peer) (state.RemoteInbound, error) {
	// Inside the relay's own deadline, so the source hears the outcome.
	ctx, cancel := context.WithTimeout(ctx, remoteWaitCap)
	defer cancel()
	sender := remoteSender(record.From, record.FromMachine)
	if record.Kind == "notify" {
		res, err := h.ask.delivery.Notify(ctx, service.NotifyParams{FromPeer: sender, ToPeer: string(target.PeerID), Text: record.Text, BypassCircle: true, DeliveryID: record.ID})
		switch {
		case err == nil && (res.Delivered() || res.Queued()):
			if _, err := h.store.AdvanceInbound(ctx, record.ID, state.RemoteDispatching, state.RemoteDelivered); err != nil {
				return record, err
			}
		case err != nil && ambiguousSend(err):
			if _, err := h.store.SettleInbound(ctx, record.ID, "uncertain", nil, time.Now().Unix()); err != nil {
				return record, err
			}
		default:
			if _, err := h.store.SettleInbound(ctx, record.ID, "not_delivered", nil, time.Now().Unix()); err != nil {
				return record, err
			}
		}
		return h.store.GetInbound(ctx, record.ID)
	}
	if _, err := h.registerRemoteAsk(ctx, record, target.DisplayName); err != nil {
		if _, serr := h.store.SettleInbound(ctx, record.ID, "not_delivered", nil, time.Now().Unix()); serr != nil {
			return record, serr
		}
		return h.store.GetInbound(ctx, record.ID)
	}
	_, err := h.ask.delivery.DeliverAsk(ctx, service.DeliverAskParams{
		FromPeer: sender, ToPeer: string(target.PeerID), Text: record.Text,
		CorrelationID: record.ID, BypassCircle: true,
	})
	if err != nil && !ambiguousSend(err) {
		// The guard commits not_delivered as the ask closes.
		if _, closed := h.ask.asks.Close(ctx, record.ID, "not_delivered"); !closed {
			return record, errors.New("could not record not_delivered")
		}
		return h.store.GetInbound(ctx, record.ID)
	}
	if _, err := h.store.AdvanceInbound(ctx, record.ID, state.RemoteDispatching, state.RemoteOpen); err != nil {
		return record, err
	}
	return h.store.GetInbound(ctx, record.ID)
}

// ambiguousSend is a transport error after the frame may have left: a
// timeout or cancellation waiting for the delivery ack. Everything else
// (unknown or unreachable target, no socket, an injection the pane refused)
// is a definite refusal.
func ambiguousSend(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// registerRemoteAsk puts a remote-origin ask in the tracker as a pull ask:
// the reply stays on the ask for the source to read.
func (h *Hub) registerRemoteAsk(ctx context.Context, record state.RemoteInbound, toName proto.DisplayName) (string, error) {
	return h.ask.asks.Register(ctx, service.RegisterAskParams{
		CorrelationID: record.ID,
		FromPeerID:    proto.RemotePeerID(record.FromMachine, record.From.PeerID),
		FromPeerName:  proto.DisplayName(remoteSender(record.From, record.FromMachine)),
		ToPeerID:      proto.PeerID(record.ToPeerID),
		ToPeerName:    toName,
		Text:          record.Text,
		ReplyDelivery: "pull",
		CreatedAt:     time.Unix(record.CreatedAt, 0),
	})
}

// guardRemoteClose commits a remote-origin ask's result before the tracker
// closes it; a failed commit keeps the ask open. After the commit the source
// is told once, off the tracker's lock.
func (h *Hub) guardRemoteClose(ask *service.Ask, reason string) error {
	machine, _, ok := proto.ParseRemotePeerID(ask.FromPeerID)
	if !ok || h.remote == nil {
		return nil
	}
	ctx := context.Background()
	won, err := h.store.SettleInbound(ctx, ask.CorrelationID, reason, ask.ReplyText, time.Now().Unix())
	if err != nil {
		return err
	}
	if won {
		go h.pushSettled(machine, remoteSettledRequest{ID: ask.CorrelationID, CloseReason: reason, Reply: ask.ReplyText, StoreEpoch: h.remote.epoch})
	}
	return nil
}

// pushSettled tells the source machine one ask settled. A lost push is
// recovered by the source reading the record back.
func (h *Hub) pushSettled(machine string, settled remoteSettledRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(settled)
	if status, _, err := h.remote.link.MachineRequest(ctx, machine, http.MethodPost, "/remote/settled", body); err != nil || status >= 300 {
		log.Printf("remote %s: settlement push to %s: status %d err %v (source reads it back)", settled.ID, machine, status, err)
	}
}

// remoteRead is the recorded origin reading its request, optionally waiting
// for it to settle. An unknown id inside the create horizon is fenced, so a
// late create can no longer dispatch it.
func (h *Hub) remoteRead(w http.ResponseWriter, r *http.Request) {
	var req remoteReadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx, id, fromMachine := r.Context(), r.PathValue("id"), relay.RemoteMachine(r.Context())
	created, ok := remoteIDTime(idPrefix(remoteIDKind(id)), id)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "malformed id")
		return
	}
	horizon := time.Now().Add(-remoteCreateHorizon).Unix()
	record, err := h.store.ReadInbound(ctx, id, fromMachine, req.From.PeerID, created, horizon)
	if err == nil && record.State != state.RemoteSettled && record.Kind == "ask" && req.Wait > 0 {
		wait := min(time.Duration(req.Wait*float64(time.Second)), remoteWaitCap)
		_, _ = h.ask.asks.WaitForResolution(ctx, id, wait, true)
		record, err = h.store.ReadInbound(ctx, id, fromMachine, req.From.PeerID, created, horizon)
	}
	switch {
	case errors.Is(err, state.ErrRemoteNotFound):
		writeJSONError(w, http.StatusForbidden, "not this origin's request")
	case errors.Is(err, state.ErrRemoteExpired):
		writeJSONError(w, http.StatusGone, map[string]any{"error": "expired", "store_epoch": h.remote.epoch})
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
	default:
		responder := record.ToPeerID
		if p, ok := h.reg.GetPeer(proto.PeerID(record.ToPeerID)); ok {
			responder = string(p.DisplayName)
		}
		writeJSON(w, http.StatusOK, h.remote.receipt(record, responder))
	}
}

// recoverRemote runs once at startup. Notifies caught mid-dispatch are
// settled uncertain and never dispatched again; asks that were dispatching
// or open go back in the tracker with their original age, so an ack still
// settles them and the TTL still ends them. Outbound asks get their tracker
// projection back from their envelope.
func (h *Hub) recoverRemote(ctx context.Context) error {
	inbound, err := h.store.InboundInStates(ctx, state.RemoteDispatching, state.RemoteOpen)
	if err != nil {
		return err
	}
	for _, record := range inbound {
		if record.Kind == "notify" {
			_, _ = h.store.SettleInbound(ctx, record.ID, "uncertain", nil, time.Now().Unix())
			continue
		}
		name := proto.DisplayName(record.ToPeerID)
		if p, ok := h.reg.GetPeer(proto.PeerID(record.ToPeerID)); ok {
			name = p.DisplayName
		}
		if _, err := h.registerRemoteAsk(ctx, record, name); err != nil {
			log.Printf("remote %s: rehydrate: %v", record.ID, err)
		}
	}
	outbound, err := h.store.UnsettledOutbound(ctx)
	if err != nil {
		return err
	}
	for _, row := range outbound {
		h.registerOutboundAsk(ctx, row)
	}
	return nil
}

// pruneLazily deletes records past retention, at most hourly, on a request.
func (m *remoteMesh) pruneLazily(ctx context.Context) {
	m.pruneMu.Lock()
	defer m.pruneMu.Unlock()
	if time.Since(m.pruned) < time.Hour {
		return
	}
	m.pruned = time.Now()
	if err := m.store.PruneRemote(ctx, time.Now().Add(-remoteRetention).Unix()); err != nil {
		log.Printf("remote: prune: %v", err)
	}
}
