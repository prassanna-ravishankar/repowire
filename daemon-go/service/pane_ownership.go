package service

// pane_ownership.go is the destructive-action proof layer for spawn/kill/restart,
// ported from repowire/spawn_ownership.py (durable JSON proof + in-memory
// spawned-pane set) plus repowire/spawn_hints.py write_hint (the codex
// circle-discovery bridge). The peer/session mapping is identity state, not
// ownership proof: a kill needs separate, revocable proof that Repowire created
// the pane. Records are written only after spawn creates a pane and removed when
// the pane dies or is killed.
//
// Identity discipline: every proof keys on peer_id, never display_name/path. The
// path is deliberately NOT sufficient for a destructive action — many peers share
// one repo path. Fail loud over silent degrade: an ambiguous or stale proof
// refuses the kill rather than guessing which pane to terminate.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// ownershipPath returns ~/.repowire/spawn_ownership.json, matching the Python
// config_paths.get_config_dir() layout. Honors $REPOWIRE_CONFIG_DIR for tests.
func ownershipPath() string {
	if dir := os.Getenv("REPOWIRE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "spawn_ownership.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "spawn_ownership.json"
	}
	return filepath.Join(home, ".repowire", "spawn_ownership.json")
}

// OwnershipRecord is a revocable pane-ownership record written by daemon spawn.
// Keyed by pane id, but validation also checks the identity tuple and live tmux
// evidence so a stale/reused pane id is not enough to authorize a kill. JSON
// field names match SpawnOwnershipRecord (snake_case) so the file round-trips
// with the Python daemon.
type OwnershipRecord struct {
	PaneID      string  `json:"pane_id"`
	Path        string  `json:"path"`
	Backend     string  `json:"backend"`
	Circle      string  `json:"circle"`
	Role        string  `json:"role"`
	DisplayName string  `json:"display_name"`
	TmuxSession string  `json:"tmux_session"`
	Machine     string  `json:"machine"`
	PeerID      *string `json:"peer_id"`
	CreatedAt   float64 `json:"created_at"`
}

// TmuxPaneEvidence is live tmux evidence for a pane id.
type TmuxPaneEvidence struct {
	PaneID      string
	SessionName string
	WindowID    string
	WindowPanes int
	TmuxSession string
	CurrentPath string
	PanePID     string
}

// OwnershipValidation is the result of validating a durable ownership record.
// Error ∈ {missing_ownership, ambiguous_ownership, ownership_machine_mismatch,
// ownership_peer_mismatch, ownership_identity_mismatch, pane_not_live,
// pane_identity_mismatch}.
type OwnershipValidation struct {
	OK       bool
	Record   *OwnershipRecord
	Evidence *TmuxPaneEvidence
	Error    string
	Hint     string
}

// PaneOwnership is the durable spawn-proof store plus the in-memory spawned-pane
// set. Backed by ~/.repowire/spawn_ownership.json (atomic temp+rename) and an
// in-process set of pane ids spawned this daemon life. Mirrors the module-level
// functions + _SPAWNED_PANE_IDS in Python.
type PaneOwnership interface {
	Record(rec OwnershipRecord)
	Forget(paneID string)
	UpdatePlacement(paneID, tmuxSession, circle string)
	MarkSpawned(paneID string)
	IsSpawned(paneID string) bool
	ValidateBootstrap(paneID string) OwnershipValidation
	ValidateForPeer(p *proto.Peer) OwnershipValidation
	BackfillPeerID(p *proto.Peer, v OwnershipValidation)
	PruneDead() int
}

// fileOwnership is the production PaneOwnership: a JSON-file-backed record store
// guarded by a mutex, plus the in-memory spawned set. The tmux seam is injected
// so tests can fake pane probing without a real tmux server.
type fileOwnership struct {
	mu          sync.Mutex
	path        string
	selfMachine string
	probe       func(paneID string) *TmuxPaneEvidence
	spawned     map[string]struct{}
}

// NewFileOwnership constructs the file-backed ownership store. selfMachine is the
// daemon's hostname (records written on another host are rejected). probe is the
// tmux pane-evidence seam (the real TmuxController.ProbePane in main; a fake in
// tests). A nil probe defaults to the real `tmux display-message` shell-out.
func NewFileOwnership(selfMachine string, probe func(paneID string) *TmuxPaneEvidence) *fileOwnership {
	if probe == nil {
		probe = realProbeTmuxPane
	}
	return &fileOwnership{
		path:        ownershipPath(),
		selfMachine: selfMachine,
		probe:       probe,
		spawned:     make(map[string]struct{}),
	}
}

// MarkSpawned adds a pane id to the in-process spawned set (the strongest, but
// daemon-lifetime-only, destructive proof). Idempotent.
func (o *fileOwnership) MarkSpawned(paneID string) {
	if paneID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.spawned[paneID] = struct{}{}
}

// IsSpawned reports whether the pane is in the in-process spawned set.
func (o *fileOwnership) IsSpawned(paneID string) bool {
	if paneID == "" {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.spawned[paneID]
	return ok
}

// Record persists proof that Repowire spawned rec.PaneID. Path is normalized and
// CreatedAt defaulted (matching SpawnOwnershipRecord.__post_init__). No-op on an
// empty pane id.
func (o *fileOwnership) Record(rec OwnershipRecord) {
	if rec.PaneID == "" {
		return
	}
	rec.Path = NormPath(rec.Path)
	if rec.CreatedAt == 0 {
		rec.CreatedAt = float64(time.Now().UnixNano()) / 1e9
	}
	if rec.Machine == "" {
		rec.Machine = o.selfMachine
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	records := o.loadLocked()
	records[rec.PaneID] = rec
	o.saveLocked(records)
}

// Forget removes any durable proof for paneID AND discards it from the spawned
// set (mirrors forget_spawn_ownership + the _SPAWNED_PANE_IDS.discard in
// forget_spawned_pane). Idempotent.
func (o *fileOwnership) Forget(paneID string) {
	if paneID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.spawned, paneID)
	records := o.loadLocked()
	if _, ok := records[paneID]; ok {
		delete(records, paneID)
		o.saveLocked(records)
	}
}

// UpdatePlacement keeps durable proof aligned with tmux renames.
func (o *fileOwnership) UpdatePlacement(paneID, tmuxSession, circle string) {
	if paneID == "" || tmuxSession == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	records := o.loadLocked()
	record, ok := records[paneID]
	if !ok || (record.TmuxSession == tmuxSession && (circle == "" || record.Circle == circle)) {
		return
	}
	record.TmuxSession = tmuxSession
	if circle != "" {
		record.Circle = circle
	}
	records[paneID] = record
	o.saveLocked(records)
}

// ValidateBootstrap returns live tmux evidence for initial registration. A
// durable ownership record, when present, additionally proves the spawn role.
func (o *fileOwnership) ValidateBootstrap(paneID string) OwnershipValidation {
	if paneID == "" {
		return OwnershipValidation{Error: "missing_ownership", Hint: "Pane-backed registration requires a pane id."}
	}
	ev := o.probe(paneID)
	if ev == nil {
		return OwnershipValidation{Error: "pane_not_live", Hint: "The pane is not visible in tmux."}
	}
	o.mu.Lock()
	rec, ok := o.loadLocked()[paneID]
	o.mu.Unlock()
	if !ok {
		return OwnershipValidation{OK: true, Evidence: ev}
	}
	if rec.Machine != "" && rec.Machine != o.selfMachine {
		return OwnershipValidation{Record: &rec, Evidence: ev, Error: "ownership_machine_mismatch", Hint: "Ownership proof was written on a different host."}
	}
	if ev.TmuxSession != rec.TmuxSession || NormPath(ev.CurrentPath) != NormPath(rec.Path) {
		return OwnershipValidation{Record: &rec, Evidence: ev, Error: "pane_identity_mismatch", Hint: "Live tmux pane evidence does not match the ownership proof."}
	}
	return OwnershipValidation{OK: true, Record: &rec, Evidence: ev}
}

// ValidateForPeer ports _effective_ownership_validation: a direct valid pane
// proof wins; a failed direct validation falls back to the unique
// identity-correlated proof (find_spawn_ownership_for_peer). This is the seam the
// routes call.
func (o *fileOwnership) ValidateForPeer(p *proto.Peer) OwnershipValidation {
	var direct *OwnershipValidation
	if p.PaneID != nil && *p.PaneID != "" {
		v := o.validateDirect(p)
		if v.OK {
			return v
		}
		direct = &v
	}
	adopted := o.findForPeer(p)
	if adopted.OK {
		o.BackfillPeerID(p, adopted)
	}
	if adopted.OK || direct == nil || adopted.Error == "ambiguous_ownership" {
		return adopted
	}
	return *direct
}

// validateDirect ports validate_spawn_ownership: the peer's pane_id must have a
// durable record on this machine for this peer, matching backend/path/circle/role
// AND backed by live tmux evidence.
func (o *fileOwnership) validateDirect(p *proto.Peer) OwnershipValidation {
	paneID := ""
	if p.PaneID != nil {
		paneID = *p.PaneID
	}
	if paneID == "" {
		return OwnershipValidation{Error: "missing_ownership",
			Hint: "Peer has no pane id, so Repowire cannot prove a pane to kill."}
	}
	o.mu.Lock()
	rec, ok := o.loadLocked()[paneID]
	o.mu.Unlock()
	if !ok {
		return OwnershipValidation{Error: "missing_ownership",
			Hint: "No durable Repowire spawn ownership proof exists for this pane."}
	}
	if rec.Machine != "" && rec.Machine != o.selfMachine {
		return OwnershipValidation{Record: &rec, Error: "ownership_machine_mismatch",
			Hint: "Ownership proof was written on a different host."}
	}
	if rec.PeerID != nil && *rec.PeerID != string(p.PeerID) {
		return OwnershipValidation{Record: &rec, Error: "ownership_peer_mismatch",
			Hint: "Ownership proof belongs to a different peer id."}
	}
	if !o.recordMatchesIdentity(rec, p) {
		return OwnershipValidation{Record: &rec, Error: "ownership_identity_mismatch",
			Hint: "Ownership proof no longer matches the peer backend/path/circle/role."}
	}
	ev := o.probe(paneID)
	if ev == nil {
		return OwnershipValidation{Record: &rec, Error: "pane_not_live",
			Hint: "The recorded pane is not visible in tmux; refusing to use stale proof."}
	}
	if ev.TmuxSession != rec.TmuxSession || NormPath(ev.CurrentPath) != NormPath(rec.Path) {
		return OwnershipValidation{Record: &rec, Evidence: ev, Error: "pane_identity_mismatch",
			Hint: "Live tmux pane evidence does not match the ownership proof."}
	}
	return OwnershipValidation{OK: true, Record: &rec, Evidence: ev}
}

// findForPeer ports find_spawn_ownership_for_peer: after a daemon restart the
// rehydrated peer may have no/stale pane fields, so correlate by identity tuple
// + live tmux evidence. Refuses to guess when more than one record matches.
func (o *fileOwnership) findForPeer(p *proto.Peer) OwnershipValidation {
	o.mu.Lock()
	records := o.loadLocked()
	o.mu.Unlock()

	var matches []OwnershipValidation
	sawIdentity, sawDead, sawMismatch := false, false, false
	for _, rec := range records {
		if rec.Machine != "" && rec.Machine != o.selfMachine {
			continue
		}
		if rec.PeerID != nil && *rec.PeerID != string(p.PeerID) {
			continue
		}
		if !o.recordMatchesIdentity(rec, p) {
			continue
		}
		sawIdentity = true
		ev := o.probe(rec.PaneID)
		if ev == nil {
			sawDead = true
			continue
		}
		if ev.TmuxSession != rec.TmuxSession || NormPath(ev.CurrentPath) != NormPath(rec.Path) {
			sawMismatch = true
			continue
		}
		r := rec
		e := ev
		matches = append(matches, OwnershipValidation{OK: true, Record: &r, Evidence: e})
	}

	switch {
	case len(matches) == 1:
		return matches[0]
	case len(matches) > 1:
		return OwnershipValidation{Error: "ambiguous_ownership",
			Hint: "Multiple durable Repowire spawn ownership proofs match this peer identity; refusing to guess which pane to kill."}
	case sawDead:
		return OwnershipValidation{Error: "pane_not_live",
			Hint: "The recorded pane is not visible in tmux; refusing to use stale proof."}
	case sawMismatch:
		return OwnershipValidation{Error: "pane_identity_mismatch",
			Hint: "Live tmux pane evidence does not match the ownership proof."}
	case sawIdentity:
		return OwnershipValidation{Error: "missing_ownership",
			Hint: "No live durable Repowire spawn ownership proof matches this peer."}
	default:
		return OwnershipValidation{Error: "missing_ownership",
			Hint: "No durable Repowire spawn ownership proof exists for this peer identity."}
	}
}

// BackfillPeerID ports backfill_ownership_peer_id: persist peer_id onto a record
// that lost it across restart, once uniquely correlated. No-op unless the peer
// has an id, the validation is OK, and the record currently lacks a peer_id.
func (o *fileOwnership) BackfillPeerID(p *proto.Peer, v OwnershipValidation) {
	if p.PeerID == "" || !v.OK || v.Record == nil || v.Record.PeerID != nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	records := o.loadLocked()
	rec, ok := records[v.Record.PaneID]
	if !ok || rec.PeerID != nil {
		return
	}
	id := string(p.PeerID)
	rec.PeerID = &id
	records[v.Record.PaneID] = rec
	o.saveLocked(records)
}

// PruneDead drops records whose pane is no longer live in tmux (lazy-repair
// hook). Returns the number removed. Mirrors prune_dead_ownership.
func (o *fileOwnership) PruneDead() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	records := o.loadLocked()
	var dead []string
	for paneID, rec := range records {
		if o.probe(rec.PaneID) == nil {
			dead = append(dead, paneID)
		}
	}
	if len(dead) == 0 {
		return 0
	}
	for _, paneID := range dead {
		delete(records, paneID)
	}
	o.saveLocked(records)
	return len(dead)
}

// recordMatchesIdentity ports _record_matches_peer_identity: backend, circle,
// role, and normalized path must all match.
func (o *fileOwnership) recordMatchesIdentity(rec OwnershipRecord, p *proto.Peer) bool {
	return rec.Backend == string(p.Backend) &&
		rec.Circle == p.Circle &&
		rec.Role == string(p.Role) &&
		NormPath(rec.Path) == NormPath(p.Path)
}

// loadLocked reads the JSON record map (caller holds mu). A missing/corrupt file
// yields an empty map (treated as "no proof", never an error). Records whose
// embedded pane_id disagrees with the map key are dropped.
func (o *fileOwnership) loadLocked() map[string]OwnershipRecord {
	out := make(map[string]OwnershipRecord)
	raw, err := os.ReadFile(o.path)
	if err != nil {
		return out
	}
	var data map[string]OwnershipRecord
	if json.Unmarshal(raw, &data) != nil {
		return out
	}
	for paneID, rec := range data {
		if rec.PaneID == paneID {
			out[paneID] = rec
		}
	}
	return out
}

// saveLocked atomically writes the record map via temp+rename (caller holds mu).
// Keys are sorted for stable diffs, matching the Python sort_keys=True dump. A
// write failure is swallowed: ownership proof is best-effort durability, and the
// in-memory spawned set still backstops destructive proof for this daemon life.
func (o *fileOwnership) saveLocked(records map[string]OwnershipRecord) {
	if err := os.MkdirAll(filepath.Dir(o.path), 0o755); err != nil {
		return
	}
	// Marshal with sorted keys: build an ordered map equivalent by encoding a
	// plain map (encoding/json already sorts string keys) so the on-disk shape
	// matches Python's json.dumps(sort_keys=True).
	blob, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return
	}
	tmp := o.path + ".tmp"
	if os.WriteFile(tmp, blob, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, o.path)
}

// NormPath ports _norm_path: realpath of the expanded path, "" for empty. Falls
// back to the cleaned absolute path when the target doesn't resolve (e.g. a peer
// whose pane already died) so identity comparison stays stable.
func NormPath(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

// realProbeTmuxPane ports probe_tmux_pane: `tmux display-message` for one pane,
// returning nil when tmux is unavailable, the pane is gone, or the fields are
// incomplete. This is the default probe seam for NewFileOwnership and the real
// TmuxController.ProbePane.
func realProbeTmuxPane(paneID string) *TmuxPaneEvidence {
	if paneID == "" {
		return nil
	}
	out, err := exec.Command("tmux", "display-message", "-t", paneID, "-p",
		"#{session_name}\t#{window_id}\t#{window_name}\t#{pane_current_path}\t#{pane_pid}\t#{window_panes}").Output()
	if err != nil {
		return nil
	}
	parts := strings.Split(strings.TrimRight(string(out), "\n"), "\t")
	if len(parts) != 6 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[4] == "" {
		return nil
	}
	windowPanes, err := strconv.Atoi(parts[5])
	if err != nil || windowPanes < 1 {
		return nil
	}
	return &TmuxPaneEvidence{
		PaneID:      paneID,
		SessionName: parts[0],
		WindowID:    parts[1],
		WindowPanes: windowPanes,
		TmuxSession: parts[0] + ":" + parts[2],
		CurrentPath: parts[3],
		PanePID:     parts[4],
	}
}
