package hub

// swarm_restore.go is the daemon's half of swarm restore (#505 part 2): it
// plans what a restore does with each pane of a snapshot, records the panes
// the CLI placed as spawn records them, and reports whether each agent came
// back under its own identity. It never touches tmux: the CLI owns tmux,
// because a tmux server inherits the environment of the client that starts
// it, and the daemon's is launchd's. Identity is never written here either:
// a restored agent comes back only through its session binding, and a twin is
// reported, never rebound.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// Restore actions.
const (
	SwarmLaunch = "launch" // start the agent's resume command in the pane
	SwarmShell  = "shell"  // a plain shell: a shell pane, or an agent left alone (Skip says why)
)

// SwarmPlan is a snapshot's restore: every pane in its place, so the window
// layout applies, each one either launched or a shell.
type SwarmPlan struct {
	Schema int `json:"schema"`
	// PlanID names the plan for /swarm/placed, which records only panes this
	// plan placed (an ownership record is kill and restart proof).
	PlanID string          `json:"plan_id"`
	Panes  []SwarmPlanPane `json:"panes"`
}

// SwarmPlanPane is one pane of the plan. CWD is where the pane starts: the
// agent's registered path for a launch, the recorded cwd for a shell.
type SwarmPlanPane struct {
	service.PaneGeometry
	Action  string       `json:"action"`
	CWD     string       `json:"cwd"`
	PeerID  proto.PeerID `json:"peer_id,omitempty"`
	Backend string       `json:"backend,omitempty"`
	Command string       `json:"command,omitempty"`
	// AwaitsFirstTurn: Codex registers on its first turn, so a resumed Codex
	// pane is not on the mesh until someone uses it; restore never prompts it.
	AwaitsFirstTurn bool   `json:"awaits_first_turn,omitempty"`
	Skip            string `json:"skip,omitempty"`
}

// swarmPlan decides each pane against the daemon's state now, not at
// snapshot time: a peer may have come back, been closed, or given its
// session to another process since.
func (h *Hub) swarmPlan(ctx context.Context, m SwarmManifest) (SwarmPlan, error) {
	if m.Schema != SwarmSchema {
		return SwarmPlan{}, fmt.Errorf("manifest schema %d, want %d", m.Schema, SwarmSchema)
	}
	executors := map[proto.PeerID]string{}
	if h.store != nil {
		work, err := h.store.ListWork(ctx, state.WorkFilter{})
		if err != nil {
			return SwarmPlan{}, err
		}
		for _, w := range work {
			if w.AssignedPeerID != nil && !state.IsTerminalState(w.State) {
				executors[proto.PeerID(*w.AssignedPeerID)] = w.WorkID
			}
		}
	}
	plan := SwarmPlan{Schema: SwarmSchema, Panes: make([]SwarmPlanPane, 0, len(m.Panes))}
	for _, p := range m.Panes {
		pane := SwarmPlanPane{PaneGeometry: p.PaneGeometry, Action: SwarmShell, CWD: p.CWD}
		if a := p.Agent; a != nil {
			pane.PeerID, pane.Backend = a.PeerID, a.Backend
			if skip := h.swarmSkip(ctx, a, executors); skip != "" {
				pane.Skip = skip
			} else if cmd, skip := h.swarmCommand(a); skip != "" {
				pane.Skip = skip
			} else {
				pane.Action, pane.CWD, pane.Command = SwarmLaunch, a.Path, cmd
				pane.AwaitsFirstTurn = proto.AgentType(a.Backend) == proto.AgentCodex
			}
		}
		plan.Panes = append(plan.Panes, pane)
	}
	plan.PlanID = h.swarmIssued().issue(plan)
	return plan, nil
}

// swarmSkip is why an agent is not relaunched, or "".
func (h *Hub) swarmSkip(ctx context.Context, a *SwarmAgent, executors map[proto.PeerID]string) string {
	switch {
	case a.Exclude != "":
		return a.Exclude
	case h.reg.Retirement(a.PeerID) == "hard":
		return "closed by an operator"
	case a.Path == "":
		return "no registered path"
	case a.RuntimeSessionID == "":
		return "no runtime session to resume"
	}
	if p, ok := h.reg.GetPeer(a.PeerID); ok && (p.Status == proto.StatusOnline || p.Status == proto.StatusBusy) {
		return "online now: relaunching it would make a twin"
	}
	if job, ok := executors[a.PeerID]; ok {
		return "executor of live job " + job + ": its runner restarts it"
	}
	// The session is bound to another peer (a fork, or a later owner): the
	// binding would bring the relaunch back as that peer, live or not, so it
	// could only come back under the wrong identity.
	if h.store != nil {
		if bound, _, err := h.store.PeerForRuntimeSession(ctx, proto.AgentType(a.Backend), a.RuntimeSessionID); err == nil && bound != "" && bound != a.PeerID {
			return "its session is bound to peer " + string(bound) + ": a relaunch would come back as that peer"
		}
	}
	// The session file, checked with the binding's resume capability as
	// restart checks it.
	var capability map[string]any
	var repowireSID *string
	if p := h.swarmPeer(a); p != nil {
		in := h.resumeInputsFor(ctx, p)
		capability, repowireSID = in.capability, in.repowireSID
	}
	if _, ok := service.ResolveLocalResume(proto.AgentType(a.Backend), a.Path, a.RuntimeSessionID, repowireSID, capability); !ok {
		return "no resumable session file"
	}
	return ""
}

// swarmPeer is the daemon's record of a's peer: live in the registry, else
// its durable mapping (after a restart nothing is live yet).
func (h *Hub) swarmPeer(a *SwarmAgent) *proto.Peer {
	if p, ok := h.reg.GetPeer(a.PeerID); ok {
		return p
	}
	m, ok := h.reg.GetMapping(a.PeerID)
	if !ok {
		return nil
	}
	p := &proto.Peer{PeerID: a.PeerID, Backend: m.Backend, Circle: m.Circle, Role: m.Role, DisplayName: m.DisplayName}
	if m.Path != nil {
		p.Path = *m.Path
	}
	return p
}

// swarmCommand is the agent's relaunch: its backend's launch template (with
// its spawn profile), the permissions read at snapshot time, and its
// backend's resume of its session. Only modeled permissions are rebuilt; a
// relaunch never replays a raw argv.
func (h *Hub) swarmCommand(a *SwarmAgent) (string, string) {
	backend := proto.AgentType(a.Backend)
	var profile *string
	if a.Profile != "" {
		profile = &a.Profile
	}
	base, err := h.spawn.svc.ResolveCommand(backend, profile)
	if err != nil {
		if service.IsCodexEmbeddedRefusal(err) {
			return "", "codex launch refused: " + err.Error()
		}
		if base = service.DefaultSpawnCommands[backend]; base == "" {
			return "", "no launch template for " + a.Backend
		}
	}
	if a.Permission.Mode == service.ModeArgvVerbatim {
		return "", "its permission arguments are not modeled; restart it by hand"
	}
	if base, err = service.ApplyPermission(base, backend, a.Permission.Words); err != nil {
		return "", "permissions: " + err.Error()
	}
	cmd, err := service.BuildResumeCommand(base, backend, a.RuntimeSessionID)
	if err != nil {
		return "", "resume: " + err.Error()
	}
	if backend == proto.AgentCodex {
		if err := h.spawn.svc.CheckCodexLaunch(cmd, "the swarm relaunch"); err != nil {
			return "", "codex launch refused: " + err.Error()
		}
	}
	return cmd, ""
}

// SwarmPlaced is a pane the CLI created and launched an agent in, under the
// plan that told it to.
type SwarmPlaced struct {
	PlanID      string       `json:"plan_id"`
	PaneID      string       `json:"pane_id"`
	PeerID      proto.PeerID `json:"peer_id"`
	TmuxSession string       `json:"tmux_session"`
}

// swarmPlaced records the pane as spawn records a pane it launched, so
// kill_peer and restart can prove ownership of it. An ownership record is
// that proof, so only a pane an issued plan placed is recorded: the plan's
// slot for that peer, used once, and the pane where the plan put it. What
// the record says about the peer comes from the daemon's own mapping, never
// the request.
func (h *Hub) swarmPlaced(pl SwarmPlaced) error {
	if pl.PaneID == "" || pl.PeerID == "" || pl.PlanID == "" {
		return fmt.Errorf("plan_id, pane_id and peer_id are required")
	}
	m, ok := h.reg.GetMapping(pl.PeerID)
	if !ok {
		return fmt.Errorf("unknown peer %s", pl.PeerID)
	}
	ev := h.spawn.svc.Container().Probe(pl.PaneID)
	if ev == nil {
		return fmt.Errorf("pane %s is not live", pl.PaneID)
	}
	if err := h.swarmIssued().consume(pl.PlanID, pl.PeerID, ev); err != nil {
		return err
	}
	path := ""
	if m.Path != nil {
		path = *m.Path
	}
	id := string(pl.PeerID)
	own := h.spawn.svc.Ownership()
	own.MarkSpawned(pl.PaneID)
	own.Record(service.OwnershipRecord{PaneID: pl.PaneID, Path: path, Backend: string(m.Backend), Circle: m.Circle, Role: string(m.Role),
		DisplayName: string(m.DisplayName), TmuxSession: pl.TmuxSession, Machine: h.spawn.selfMachine, PeerID: &id})
	return nil
}

// Verify outcomes.
const (
	SwarmBack            = "back"              // registered under its own peer_id, in its new pane
	SwarmAwaitsFirstTurn = "awaits_first_turn" // Codex: registers on its first turn
	SwarmMissing         = "missing"           // nothing registered yet
	SwarmTwin            = "twin"              // another identity registered in its pane
)

// SwarmVerified is one placed agent's outcome.
type SwarmVerified struct {
	SwarmPlaced
	Outcome string       `json:"outcome"`
	Got     proto.PeerID `json:"got,omitempty"`
}

// swarmVerify reports whether each placed agent came back under its own
// identity. It only reads: a twin is reported, never rebound. Whether an
// agent is Codex (it registers on its first turn) is the daemon's record of
// its backend, not the caller's.
func (h *Hub) swarmVerify(placed []SwarmPlaced) []SwarmVerified {
	out := make([]SwarmVerified, 0, len(placed))
	for _, pl := range placed {
		v := SwarmVerified{SwarmPlaced: pl, Outcome: SwarmMissing}
		if p, ok := h.reg.GetPeer(pl.PeerID); ok && p.PaneID != nil && *p.PaneID == pl.PaneID && (p.Status == proto.StatusOnline || p.Status == proto.StatusBusy) {
			v.Outcome = SwarmBack
		} else if other, ok := h.reg.GetPeerByPane(pl.PaneID); ok && other.PeerID != pl.PeerID {
			v.Outcome, v.Got = SwarmTwin, other.PeerID
		} else if m, ok := h.reg.GetMapping(pl.PeerID); ok && m.Backend == proto.AgentCodex {
			v.Outcome = SwarmAwaitsFirstTurn
		}
		out = append(out, v)
	}
	return out
}

// registerSwarmRestoreRoutes mounts POST /swarm/plan, /swarm/placed and
// /swarm/verify beside the snapshot, under the same experiment.
func (h *Hub) registerSwarmRestoreRoutes(mux *http.ServeMux) {
	gate := func(next func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
			if !h.swarmEnabled {
				writeError(w, http.StatusNotFound, swarmOffHint)
				return
			}
			next(w, r)
		})
	}
	mux.HandleFunc("POST /swarm/plan", gate(func(w http.ResponseWriter, r *http.Request) {
		var m SwarmManifest
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			writeError(w, http.StatusBadRequest, "invalid manifest: "+err.Error())
			return
		}
		plan, err := h.swarmPlan(r.Context(), m)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, plan)
	}))
	mux.HandleFunc("POST /swarm/placed", gate(func(w http.ResponseWriter, r *http.Request) {
		var pl SwarmPlaced
		if err := json.NewDecoder(r.Body).Decode(&pl); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
		if err := h.swarmPlaced(pl); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /swarm/verify", gate(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Placed []SwarmPlaced `json:"placed"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"agents": h.swarmVerify(body.Placed)})
	}))
}

// swarmPlanTTL is how long a plan's placements are accepted: long enough to
// rebuild a large layout with staggered starts, short enough that a plan
// cannot be replayed later.
const swarmPlanTTL = 30 * time.Minute

// swarmSlot is where a plan put one agent, and whether its placement was
// recorded already.
type swarmSlot struct {
	session, window string
	used            bool
}

type issuedPlan struct {
	expires time.Time
	slots   map[proto.PeerID]*swarmSlot
}

// issuedPlans are the plans this daemon issued, in memory: a restarted
// daemon accepts no placement from before it, so a restore re-plans.
type issuedPlans struct {
	mu    sync.Mutex
	now   func() time.Time
	plans map[string]*issuedPlan
}

func (h *Hub) swarmIssued() *issuedPlans {
	h.swarmPlansOnce.Do(func() {
		if h.swarmPlans == nil {
			h.swarmPlans = &issuedPlans{now: time.Now, plans: map[string]*issuedPlan{}}
		}
	})
	return h.swarmPlans
}

// issue records plan's launch slots and returns its id. Expired plans go.
func (p *issuedPlans) issue(plan SwarmPlan) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for id, ip := range p.plans {
		if now.After(ip.expires) {
			delete(p.plans, id)
		}
	}
	id := "swarm-" + uuid.NewString()[:12]
	ip := &issuedPlan{expires: now.Add(swarmPlanTTL), slots: map[proto.PeerID]*swarmSlot{}}
	for _, pane := range plan.Panes {
		if pane.Action == SwarmLaunch {
			ip.slots[pane.PeerID] = &swarmSlot{session: pane.Session, window: pane.WindowName}
		}
	}
	p.plans[id] = ip
	return id
}

// consume accepts one placement: an unexpired plan, a launch slot for peer
// not used before, and a pane in the slot's session and window.
func (p *issuedPlans) consume(planID string, peer proto.PeerID, ev *service.TmuxPaneEvidence) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ip, ok := p.plans[planID]
	if !ok || p.now().After(ip.expires) {
		return fmt.Errorf("no current plan %s: plan the restore again", planID)
	}
	slot, ok := ip.slots[peer]
	switch {
	case !ok:
		return fmt.Errorf("plan %s does not launch %s", planID, peer)
	case slot.used:
		return fmt.Errorf("plan %s already placed %s", planID, peer)
	case ev.SessionName != slot.session || ev.TmuxSession != slot.session+":"+slot.window:
		return fmt.Errorf("pane %s is in %s, but the plan put %s in %s:%s", ev.PaneID, ev.TmuxSession, peer, slot.session, slot.window)
	}
	slot.used = true
	return nil
}
