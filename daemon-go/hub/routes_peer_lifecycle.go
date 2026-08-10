package hub

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// routes_peer_lifecycle.go owns the peer-lifecycle HTTP route group:
//
//	POST   /peer/register            (alias of POST /peers)
//	POST   /peers
//	POST   /peer/unregister          (body {name})
//	POST   /peers/{name}/offline
//	POST   /peers/{name}/touch
//	POST   /peers/{name}/description
//
// Every handler is gated by requireAuth. The wire JSON shapes match the Python
// daemon (daemon/routes/peers.py) exactly — clients depend on them. Identity-
// sensitive state canonicalizes to proto.PeerID inside the registry even though
// clients address peers by display_name: the *ByName registry wrappers resolve
// the string once (fail-loud on ambiguity → 409) and mutate keyed on PeerID.

// registerPeerLifecycleRoutes wires the write endpoints onto the mux behind the
// shared bearer-token gate. Go 1.22+ method-prefixed patterns keep these POSTs
// from colliding with the read group's GET patterns on the same paths.
func (h *Hub) registerPeerLifecycleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /peer/register", h.requireAuth(h.handleRegisterPeer))
	mux.HandleFunc("POST /peers", h.requireAuth(h.handleRegisterPeer))
	mux.HandleFunc("POST /peers/identity/validate", h.requireAuth(h.handleValidateRuntimeIdentity))
	mux.HandleFunc("POST /peer/unregister", h.requireAuth(h.handleUnregisterPeerBody))
	mux.HandleFunc("DELETE /peers/{name}", h.requireAuth(h.handleDeletePeer))
	mux.HandleFunc("POST /peers/{name}/offline", h.requireAuth(h.handleMarkOffline))
	mux.HandleFunc("POST /peers/{name}/touch", h.requireAuth(h.handleTouch))
	mux.HandleFunc("POST /peers/{name}/description", h.requireAuth(h.handleSetDescription))
}

// ---------------------------------------------------------------------------
// Request / response wire shapes (must match daemon/routes/peers.py).
// ---------------------------------------------------------------------------

// RegisterPeerRequest mirrors the Python RegisterPeerRequest. Pointer fields are
// the optional ones; Backend/Role default when absent. Name is required and must
// match ^[a-zA-Z0-9._-]+$; Circle (when present) is validated the same way.
type RegisterPeerRequest struct {
	PeerID       *string          `json:"peer_id"`
	Name         string           `json:"name"`
	Path         *string          `json:"path"`
	Machine      *string          `json:"machine"`
	TmuxSession  *string          `json:"tmux_session"`
	PaneID       *string          `json:"pane_id"`
	Backend      proto.AgentType  `json:"backend"`
	Model        *string          `json:"model"`
	Circle       *string          `json:"circle"`
	CircleSource *string          `json:"circle_source"`
	Role         proto.PeerRole   `json:"role"`
	TurnState    *proto.TurnState `json:"turn_state"`
	AgentPID     *int             `json:"agent_pid"`
	ParentPID    *int             `json:"parent_pid"`
	Metadata     map[string]any   `json:"metadata"`
}

// RegisterResponse mirrors the Python RegisterResponse. BirthCertificate is the
// cert envelope (nil when the binding store is absent or persistence was
// skipped/failed).
type RegisterResponse struct {
	OK               bool           `json:"ok"`
	PeerID           string         `json:"peer_id"`
	DisplayName      string         `json:"display_name"`
	PaneAssigned     bool           `json:"pane_assigned"`
	BirthCertificate map[string]any `json:"birth_certificate"`
}

type validateBirthCertificateRequest struct {
	BirthCertificate state.CertEnvelope `json:"birth_certificate"`
	Backend          proto.AgentType    `json:"backend"`
	Path             *string            `json:"path"`
	PaneID           *string            `json:"pane_id"`
	AgentPID         *int               `json:"agent_pid"`
}

type validateBirthCertificateResponse struct {
	OK          bool     `json:"ok"`
	PeerID      string   `json:"peer_id"`
	DisplayName string   `json:"display_name"`
	Peer        PeerInfo `json:"peer"`
}

// UnregisterPeerRequest is the POST /peer/unregister body.
type UnregisterPeerRequest struct {
	Name string `json:"name"`
}

// OfflineRequest mirrors the Python OfflineRequest. Defaults preserve the legacy
// behavior for old hooks that POST `{}` — so the zero value of an absent body is
// applied via newOfflineRequest, not Go's struct zero.
type OfflineRequest struct {
	Reason   string  `json:"reason"`
	Source   string  `json:"source"`
	Detail   *string `json:"detail"`
	Terminal bool    `json:"terminal"`
}

// OfflineResponse mirrors the Python OfflineResponse.
type OfflineResponse struct {
	OK               bool `json:"ok"`
	CancelledQueries int  `json:"cancelled_queries"`
}

// SetDescriptionRequest is the POST /peers/{name}/description body.
type SetDescriptionRequest struct {
	Description string `json:"description"`
}

// ---------------------------------------------------------------------------
// Shared impls.
// ---------------------------------------------------------------------------

// registerPeerImpl is the shared body for POST /peer/register and POST /peers.
// Pane-backed bootstrap derives circle and role from live spawn ownership; a
// pane-less CLI/admin registration supplies its own explicit circle. It then
// allocates/reclaims the identity through the registry FSM, applies the
// initial-OFFLINE rule (a pane-backed peer that already reported a runtime
// session id registers OFFLINE — its ws-hook owns the ONLINE transition), then
// persists a session-binding observation + mints a birth certificate (unless
// persistBinding is false, the pane-adoption rollback path). Returns the
// canonical peer_id, assigned display_name, pane_assigned, and the cert envelope.
func (h *Hub) registerPeerImpl(r *http.Request, req RegisterPeerRequest, persistBinding bool) (RegisterResponse, int, string) {
	ctx := r.Context()

	if !isValidIdentifier(req.Name) {
		return RegisterResponse{}, http.StatusUnprocessableEntity,
			"name must match ^[a-zA-Z0-9._-]+$ and be <= 64 chars"
	}
	if req.Circle != nil && *req.Circle != "" && !isValidIdentifier(*req.Circle) {
		return RegisterResponse{}, http.StatusUnprocessableEntity,
			"Circle must match ^[a-zA-Z0-9._-]+$ and be <= 64 chars"
	}

	backend := req.Backend
	if backend == "" {
		backend = proto.AgentClaudeCode
	}
	role, circle := req.Role, ""
	if req.PaneID != nil && *req.PaneID != "" {
		var code int
		var detail string
		circle, role, code, detail = h.verifiedPaneIdentity(*req.PaneID, backend, derefString(req.Path), derefString(req.Circle), req.Role)
		if code != http.StatusOK {
			return RegisterResponse{}, code, detail
		}
	} else {
		if req.Circle == nil || *req.Circle == "" {
			return RegisterResponse{}, http.StatusUnprocessableEntity,
				"Circle is required; choose a circle for pane-less registration"
		}
		circle = *req.Circle
		if role == "" {
			role = proto.RoleAgent
		}
	}
	if !role.Valid() {
		return RegisterResponse{}, http.StatusUnprocessableEntity, "Invalid role"
	}
	machine := "unknown"
	if req.Machine != nil && *req.Machine != "" {
		machine = *req.Machine
	} else if hn, err := os.Hostname(); err == nil && hn != "" {
		machine = hn
	}
	runtimeSessionID := runtimeSessionIDFromMetadata(req.Metadata)

	var claimed *proto.PeerID
	if req.PeerID != nil && *req.PeerID != "" {
		id := proto.PeerID(*req.PeerID)
		claimed = &id
	}

	params := peer.AllocateParams{
		Circle:        circle,
		Backend:       backend,
		Model:         req.Model,
		Path:          req.Path,
		PaneID:        req.PaneID,
		TmuxSession:   req.TmuxSession,
		Machine:       machine,
		Role:          role,
		ClaimedPeerID: claimed,
		Metadata:      req.Metadata,
		AgentPID:      req.AgentPID,
		ParentPID:     req.ParentPID,
		TurnState:     req.TurnState,
	}
	peerID, displayName, err := h.reg.AllocateAndRegister(ctx, params)
	if err != nil {
		// PaneHijack / PeerRetired guards are 409s (orphan ws-hook reclaim).
		return RegisterResponse{}, http.StatusConflict, err.Error()
	}

	// Initial-OFFLINE rule: a pane-backed peer that already reported a runtime
	// session id is mid-handoff — its ws-hook, not this HTTP registration, owns
	// the ONLINE transition. Drive it OFFLINE through the FSM (non-terminal) so
	// the wire status is truthful rather than papering an unverified ONLINE.
	if req.PaneID != nil && *req.PaneID != "" && runtimeSessionID != nil {
		_, _ = h.reg.MarkOffline(ctx, peerID, false)
	}

	var birthCert map[string]any
	if h.store != nil && persistBinding {
		birthCert = h.persistBinding(r, req, peerID, displayName, backend, circle, role, runtimeSessionID)
	}

	// pane_assigned: vacuously true when no pane was requested; otherwise true
	// iff the registered peer actually ended up owning the requested pane.
	paneAssigned := true
	if req.PaneID != nil && *req.PaneID != "" {
		if p, ok := h.reg.GetPeer(peerID); ok {
			paneAssigned = p.PaneID != nil && *p.PaneID == *req.PaneID
		} else {
			paneAssigned = false
		}
	}

	return RegisterResponse{
		OK:               true,
		PeerID:           string(peerID),
		DisplayName:      string(displayName),
		PaneAssigned:     paneAssigned,
		BirthCertificate: birthCert,
	}, http.StatusOK, ""
}

func (h *Hub) verifiedPaneIdentity(paneID string, backend proto.AgentType, path, requestedCircle string, requestedRole proto.PeerRole) (string, proto.PeerRole, int, string) {
	if h.spawn == nil || h.spawn.svc == nil {
		return "", "", http.StatusForbidden, "pane-backed runtime registration requires live tmux evidence"
	}
	proof := h.spawn.svc.Ownership().ValidateBootstrap(paneID)
	if !proof.OK || proof.Evidence == nil {
		return "", "", http.StatusForbidden, "pane-backed runtime registration rejected: " + proof.Error
	}
	circle := tmuxEvidenceCircle(h.spawn.boundary, proof.Evidence)
	role := proto.RoleAgent
	if proof.Record != nil {
		if proof.Record.Backend != string(backend) || service.NormPath(proof.Record.Path) != service.NormPath(path) {
			return "", "", http.StatusForbidden, "pane-backed runtime registration does not match spawn ownership"
		}
		if proof.Record.Circle != circle {
			return "", "", http.StatusForbidden, "pane-backed runtime registration circle contradicts live tmux evidence"
		}
		role = proto.PeerRole(proof.Record.Role)
	}
	if !isValidIdentifier(circle) || !role.Valid() {
		return "", "", http.StatusConflict, "pane evidence has invalid circle or role"
	}
	if service.NormPath(proof.Evidence.CurrentPath) != service.NormPath(path) {
		return "", "", http.StatusForbidden, "pane-backed runtime path contradicts live tmux evidence"
	}
	if requestedCircle != "" && requestedCircle != circle {
		return "", "", http.StatusForbidden, "pane-backed runtime registration circle contradicts pane evidence"
	}
	if requestedRole != "" && requestedRole != role {
		return "", "", http.StatusForbidden, "pane-backed runtime registration role contradicts pane evidence"
	}
	return circle, role, http.StatusOK, ""
}

func tmuxEvidenceCircle(boundary proto.CircleBoundary, evidence *service.TmuxPaneEvidence) string {
	if evidence == nil {
		return ""
	}
	if boundary == "" {
		boundary = proto.CircleBoundarySession
	}
	session := evidence.SessionName
	if session == "" {
		session, _, _ = strings.Cut(evidence.TmuxSession, ":")
	}
	return proto.TmuxCircle(boundary, session, evidence.WindowID)
}

// persistBinding records the session-binding observation and mints a birth
// certificate. Best-effort, mirroring Python: any store error is logged-and-
// swallowed (registration still succeeds; the cert is simply absent). Returns
// the cert envelope, or nil when minting failed.
func (h *Hub) persistBinding(
	r *http.Request,
	req RegisterPeerRequest,
	peerID proto.PeerID,
	displayName proto.DisplayName,
	backend proto.AgentType,
	circle string,
	role proto.PeerRole,
	runtimeSessionID *string,
) map[string]any {
	ctx := r.Context()
	pid := string(peerID)

	provenance := map[string]any{
		"source_kind":         "runtime_hook",
		"backend":             string(backend),
		"runtime_session_id":  derefOrNil(runtimeSessionID),
		"observed_by_peer_id": pid,
	}
	bindingMeta := bindingMetadata(req.Metadata)
	bindingMeta["circle"] = circle
	bindingMeta["role"] = string(role)
	if _, err := h.store.UpsertObservation(ctx, state.Observation{
		PeerID:           &pid,
		Backend:          string(backend),
		ProjectPath:      req.Path,
		RuntimeSessionID: runtimeSessionID,
		RuntimeSourceURI: metadataSourceURI(req.Metadata),
		Provenance:       provenance,
		ResumeCapability: service.ResumeCapabilityForRegistration(backend, derefString(runtimeSessionID)),
		Status:           state.BindingActive,
		Metadata:         bindingMeta,
	}); err != nil {
		// Fail loud in the journal, not on the request: registration is durable
		// in the registry regardless; the binding is observability/resume sugar.
		h.reg.AddEvent(r.Context(), "session_binding_persist_failed", map[string]any{
			"peer_id": pid, "detail": err.Error(),
		})
		return nil
	}

	cert, err := h.store.MintBirthCertificate(
		ctx, pid, string(displayName), string(backend),
		req.Path, runtimeSessionID, req.PaneID, req.AgentPID, req.ParentPID,
		bindingMeta, 0, // ttl 0 → DefaultCertificateTTL
		time.Time{}, // zero issuedAt → MintBirthCertificate uses time.Now()
	)
	if err != nil {
		h.reg.AddEvent(r.Context(), "birth_certificate_mint_failed", map[string]any{
			"peer_id": pid, "detail": err.Error(),
		})
		return nil
	}
	return certEnvelope(cert)
}

// unregisterPeerImpl is the shared body for POST /peer/unregister and DELETE
// /peers/{name}: 404 if the peer is unknown (matching Python's get_peer
// pre-check), else remove it from the registry + durable mapping. An ambiguous
// display_name resolves to a 409 (fail-loud), never a silent guess.
func (h *Hub) unregisterPeerImpl(r *http.Request, name string, circle *string) (int, string) {
	ctx := r.Context()
	p, err := h.reg.ResolvePeer(name, circle)
	if err != nil {
		return http.StatusConflict, err.Error()
	}
	if p == nil {
		return http.StatusNotFound, "Peer not found: " + name
	}
	// Delete by the RESOLVED peer_id, not the raw name: the id is already
	// unambiguous here, so UnregisterPeer's own ambiguity error can't fire.
	_, _ = h.reg.UnregisterPeer(ctx, string(p.PeerID), nil)
	return http.StatusOK, ""
}

// ---------------------------------------------------------------------------
// Handlers.
// ---------------------------------------------------------------------------

func (h *Hub) handleRegisterPeer(w http.ResponseWriter, r *http.Request) {
	var req RegisterPeerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	resp, code, detail := h.registerPeerImpl(r, req, true)
	if code != http.StatusOK {
		writeError(w, code, detail)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Hub) handleValidateRuntimeIdentity(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeError(w, http.StatusNotFound, "Runtime identity certificate store is unavailable")
		return
	}
	var req validateBirthCertificateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BirthCertificate.Nonce == "" {
		writeError(w, http.StatusUnprocessableEntity, "birth_certificate is required")
		return
	}
	cert, err := h.store.ValidateBirthCertificate(r.Context(), req.BirthCertificate, string(req.Backend), req.Path, req.PaneID, req.AgentPID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cert == nil {
		writeError(w, http.StatusConflict, "Runtime identity certificate is invalid or expired")
		return
	}
	peerID := proto.PeerID(cert.PeerID)
	p, ok := h.reg.GetPeer(peerID)
	if !ok {
		circle, _ := cert.Metadata["circle"].(string)
		if circle == "" {
			writeError(w, http.StatusConflict, "Runtime identity certificate has no circle")
			return
		}
		role := proto.RoleAgent
		if value, ok := cert.Metadata["role"].(string); ok && value != "" {
			role = proto.PeerRole(value)
		}
		machine, _ := os.Hostname()
		metadata := map[string]any{"birth_certificate_nonce": cert.Nonce}
		if cert.RuntimeSessionID != nil {
			metadata["hook_session_id"] = *cert.RuntimeSessionID
		}
		id, _, allocErr := h.reg.AllocateAndRegister(r.Context(), peer.AllocateParams{
			Circle: circle, Backend: proto.AgentType(cert.Backend), Path: &cert.ProjectPath,
			PaneID: cert.PaneID, Machine: machine, Role: role, ClaimedPeerID: &peerID,
			Metadata: metadata, AgentPID: cert.AgentPID, ParentPID: cert.ParentPID,
		})
		if allocErr != nil {
			writeError(w, http.StatusConflict, "Runtime identity certificate could not rehydrate peer: "+allocErr.Error())
			return
		}
		p, ok = h.reg.GetPeer(id)
	}
	if !ok || p == nil {
		writeError(w, http.StatusNotFound, "Peer not found after runtime identity validation")
		return
	}
	writeJSON(w, http.StatusOK, validateBirthCertificateResponse{OK: true, PeerID: string(p.PeerID), DisplayName: string(p.DisplayName), Peer: peerToInfo(p)})
}

func (h *Hub) handleUnregisterPeerBody(w http.ResponseWriter, r *http.Request) {
	var req UnregisterPeerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if code, detail := h.unregisterPeerImpl(r, req.Name, nil); code != http.StatusOK {
		writeError(w, code, detail)
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

func (h *Hub) handleDeletePeer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var circle *string
	if c := r.URL.Query().Get("circle"); c != "" {
		circle = &c
	}
	if code, detail := h.unregisterPeerImpl(r, name, circle); code != http.StatusOK {
		writeError(w, code, detail)
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

func (h *Hub) handleMarkOffline(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	body := newOfflineRequest()
	// An absent/empty body is allowed (old hooks POST nothing or `{}`).
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	found, cancelled, err := h.reg.MarkOfflineByNameWithReason(r.Context(), name, body.Terminal, body.Reason)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	// Python returns 200 with cancelled=0 even for an unknown id (mark_offline is
	// a no-op there); a terminal offline of an evicted peer_id still retires it.
	_ = found
	writeJSON(w, http.StatusOK, OfflineResponse{OK: true, CancelledQueries: cancelled})
}

func (h *Hub) handleTouch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var circle *string
	if c := r.URL.Query().Get("circle"); c != "" {
		circle = &c
	}
	found, err := h.reg.TouchLastSeen(r.Context(), name, circle)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "Peer not found: "+name)
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

func (h *Hub) handleSetDescription(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req SetDescriptionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var circle *string
	if c := r.URL.Query().Get("circle"); c != "" {
		circle = &c
	}
	found, err := h.reg.UpdateDescription(r.Context(), name, req.Description, circle)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "Peer not found: "+name)
		return
	}
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// newOfflineRequest returns the legacy-preserving defaults for an absent body.
func newOfflineRequest() OfflineRequest {
	detail := "Peer was explicitly marked offline through the HTTP route."
	return OfflineRequest{
		Reason:   "offline_route",
		Source:   "peers_route",
		Detail:   &detail,
		Terminal: false,
	}
}

// decodeJSON (the request-body decoder shared across route groups) lives in
// routes_ask_lifecycle.go; this group reuses it rather than redeclaring it.

// runtimeSessionIDFromMetadata extracts the runtime (hook) session id from peer
// metadata. Mirrors _runtime_session_id_from_metadata: first non-empty string of
// hook_session_id, runtime_session_id, session_id.
func runtimeSessionIDFromMetadata(m map[string]any) *string {
	for _, k := range []string{"hook_session_id", "runtime_session_id", "session_id"} {
		if v, ok := m[k].(string); ok && v != "" {
			return &v
		}
	}
	return nil
}

// metadataSourceURI mirrors _metadata_source_uri: first non-empty of
// runtime_source_uri, source_uri, transcript_source_uri.
func metadataSourceURI(m map[string]any) *string {
	for _, k := range []string{"runtime_source_uri", "source_uri", "transcript_source_uri"} {
		if v, ok := m[k].(string); ok && v != "" {
			return &v
		}
	}
	return nil
}

// bindingMetadata mirrors _binding_metadata: the allow-listed subset of peer
// metadata persisted on the binding (identifiers + source locators only).
func bindingMetadata(m map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{
		"hook_session_id", "runtime_session_id", "session_id",
		"runtime_source_uri", "source_uri", "transcript_source_uri",
	} {
		if v, ok := m[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

// certEnvelope projects a minted certificate into the wire envelope shape Python
// clients consume (RuntimeIdentityCertificate.as_envelope).
func certEnvelope(c *state.RuntimeIdentityCertificate) map[string]any {
	return map[string]any{
		"nonce":              c.Nonce,
		"peer_id":            c.PeerID,
		"display_name":       c.DisplayName,
		"backend":            c.Backend,
		"project_path":       c.ProjectPath,
		"runtime_session_id": c.RuntimeSessionID,
		"pane_id":            c.PaneID,
		"agent_pid":          c.AgentPID,
		"parent_pid":         c.ParentPID,
		"issued_at":          c.IssuedAt,
		"expires_at":         c.ExpiresAt,
		"metadata":           c.Metadata,
	}
}

func derefOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
