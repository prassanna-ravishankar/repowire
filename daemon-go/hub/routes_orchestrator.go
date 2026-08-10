package hub

import (
	"errors"
	"net/http"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
)

// routes_orchestrator.go owns the orchestrator-role HTTP surface:
//
//	POST /peers/claim-role        (claim the singleton orchestrator role)
//
// The read counterpart, GET /circles/{name}/orchestrator, is registered by the
// peer-read group (routes_peer_read.go) and is intentionally left there. This
// file adds only the write/claim route to avoid touching the lifecycle file
// concurrently. The wire shapes match daemon/routes/peers.py exactly — the CLI
// repair surface depends on them. The claim is intentionally NOT exposed through
// MCP/Pi (include_in_schema=False in Python); it is a CLI-only repair hook.

// ClaimRoleRequest is the POST /peers/claim-role body. Mirrors the Python
// ClaimRoleRequest: peer_name + role are required, circle/force optional.
type ClaimRoleRequest struct {
	PeerName string         `json:"peer_name"`
	Role     proto.PeerRole `json:"role"`
	Circle   *string        `json:"circle"`
	Force    bool           `json:"force"`
}

// ClaimRoleResponse mirrors the Python ClaimRoleResponse. PreviousHolders is the
// list of peer_ids demoted out of the role by this claim (Registry.ClaimResult
// snapshots holders by PeerID; the Python wire carried richer dicts but the Go
// registry collapses them to ids — clients read membership, not the detail).
type ClaimRoleResponse struct {
	PeerID          string         `json:"peer_id"`
	PeerName        string         `json:"peer_name"`
	Role            proto.PeerRole `json:"role"`
	Circle          string         `json:"circle"`
	AlreadyHeld     bool           `json:"already_held"`
	PreviousHolders []string       `json:"previous_holders"`
}

// registerOrchestratorRoutes wires the claim route behind the shared bearer-token
// gate. The GET status route lives in the peer-read group.
func (h *Hub) registerOrchestratorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /peers/claim-role", h.requireAuth(h.handleClaimRole))
}

// handleClaimRole claims a singleton special role for an existing peer. Only
// role=orchestrator is claimable (anything else → 400). Maps the registry
// outcomes to HTTP per the Python route: RoleClaimConflict → 409, bad role
// (ValueError) → 400, no such peer (nil result) → 404. Mirrors
// daemon/routes/peers.py:claim_peer_role.
func (h *Hub) handleClaimRole(w http.ResponseWriter, r *http.Request) {
	var req ClaimRoleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Role != proto.RoleOrchestrator {
		writeError(w, http.StatusBadRequest, "Only role=orchestrator can be claimed")
		return
	}

	result, err := h.reg.ClaimSpecialRole(r.Context(), req.PeerName, req.Role, req.Circle, req.Force)
	if err != nil {
		var conflict *peer.RoleClaimConflictError
		if errors.As(err, &conflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		// A plain error from ClaimSpecialRole is a validation failure (bad role,
		// ambiguous identifier). Python's claim route maps ValueError → 400; an
		// ambiguous-name resolve error also surfaces loud here rather than guessing.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if result == nil {
		writeError(w, http.StatusNotFound, "Peer not found: "+req.PeerName)
		return
	}

	// Never serialize null for previous_holders: Python's wire shape is [] when
	// nothing was demoted, and clients iterate it unconditionally.
	prev := result.PreviousHolders
	if prev == nil {
		prev = []string{}
	}
	writeJSON(w, http.StatusOK, ClaimRoleResponse{
		PeerID:          string(result.Peer.PeerID),
		PeerName:        string(result.Peer.DisplayName),
		Role:            result.Peer.Role,
		Circle:          result.Peer.Circle,
		AlreadyHeld:     result.AlreadyHeld,
		PreviousHolders: prev,
	})
}
