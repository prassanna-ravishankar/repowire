package hub

// swarm.go is the read-only half of swarm snapshot/restore (#505): a record
// of the tmux layout and of the agent each pane runs, taken while they are
// alive, so a restore after tmux or the machine went down can rebuild the
// layout and resume each agent. The tmux plug owns the geometry
// (service.LayoutDescriber); this joins it to what the daemon knows about
// each pane's peer, through the same resume and permission reading restart
// uses. Restore (plan, apply, verify) is a later step; nothing here changes
// any state.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// SwarmSchema versions the manifest.
const SwarmSchema = 1

// SwarmManifest is one snapshot. It holds no message content, credential,
// certificate, metadata blob, environment or shell command.
type SwarmManifest struct {
	Schema  int         `json:"schema"`
	TakenAt string      `json:"taken_at"`
	Machine string      `json:"machine"`
	Panes   []SwarmPane `json:"panes"`
}

// SwarmPane is one pane: its place in the layout, and the agent in it when
// it runs one (none for a plain shell, whose cwd is all that is kept).
type SwarmPane struct {
	service.PaneGeometry
	Agent *SwarmAgent `json:"agent,omitempty"`
}

// SwarmAgent is what a restore needs to bring one agent back under its own
// identity: it relaunches in Path (the registered path the session binding
// checks, not the pane's cwd) with its backend's resume of RuntimeSessionID.
// Permission is read now, from the live process, since after a reboot there
// is no process to read. Exclude names why a restore leaves the pane a
// plain shell, for agents another path owns.
type SwarmAgent struct {
	PeerID      proto.PeerID `json:"peer_id"`
	DisplayName string       `json:"display_name"`
	Circle      string       `json:"circle"`
	Role        string       `json:"role"`
	Backend     string       `json:"backend"`
	Path        string       `json:"path"`
	Status      string       `json:"status"`
	// Running is whether the agent ran in this pane when the snapshot was
	// taken (its process in the pane's tree). A pane whose agent was quit
	// still maps to its offline peer; restoring that would resurrect it.
	Running          bool             `json:"running"`
	RuntimeSessionID string           `json:"runtime_session_id,omitempty"`
	ResumeOK         bool             `json:"resume_ok"`
	Model            string           `json:"model,omitempty"`
	Profile          string           `json:"profile,omitempty"`
	Permission       permissionChoice `json:"permission"`
	Exclude          string           `json:"exclude,omitempty"`
}

// swarmExclusion is why an agent is not restored by a swarm restore, or "":
// the orchestrator has its own start path, the human and services are not
// panes a restore launches, and a sub-agent comes back with its parent.
func swarmExclusion(p *proto.Peer) string {
	prov := p.Provenance.Normalized()
	switch {
	case p.Role == proto.RoleOrchestrator:
		return "orchestrator: it has its own start path"
	case p.Role == proto.RoleHuman || p.Role == proto.RoleService:
		return "human or service peer"
	case prov.Ephemeral || prov.ParentRuntimeID != "":
		return "sub-agent: its parent's resume brings it back"
	}
	return ""
}

var errSwarmNoLayout = errors.New("the container plug cannot describe its layout")

// swarmSnapshot records the layout and the agents in it, now.
func (h *Hub) swarmSnapshot(ctx context.Context) (SwarmManifest, error) {
	describer, ok := h.spawn.svc.Container().(service.LayoutDescriber)
	if !ok {
		return SwarmManifest{}, errSwarmNoLayout
	}
	geometry, err := describer.Describe()
	if err != nil {
		return SwarmManifest{}, err
	}
	m := SwarmManifest{Schema: SwarmSchema, TakenAt: time.Now().UTC().Format(time.RFC3339), Machine: h.spawn.selfMachine,
		Panes: make([]SwarmPane, 0, len(geometry))}
	for _, g := range geometry {
		pane := SwarmPane{PaneGeometry: g}
		if p, ok := h.spawn.reg.GetPeerByPane(g.PaneID); ok && p != nil && !proto.IsRemotePeerID(p.PeerID) {
			pane.Agent = h.swarmAgent(ctx, p)
			ev := h.spawn.svc.Container().Probe(g.PaneID)
			pane.Agent.Running = ev != nil && service.PaneRunsPeer(h.spawn.proc, ev, p)
			if !pane.Agent.Running && pane.Agent.Exclude == "" {
				pane.Agent.Exclude = "not running: the agent had left its pane when the snapshot was taken"
			}
		}
		m.Panes = append(m.Panes, pane)
	}
	return m, nil
}

func (h *Hub) swarmAgent(ctx context.Context, p *proto.Peer) *SwarmAgent {
	in := h.resumeInputsFor(ctx, p)
	var argv []string
	exact := false
	if p.AgentPID != nil && h.spawn.proc.Alive(*p.AgentPID) {
		argv, exact = h.spawn.proc.Args(*p.AgentPID)
	}
	a := &SwarmAgent{
		PeerID: p.PeerID, DisplayName: string(p.DisplayName), Circle: p.Circle, Role: string(p.Role),
		Backend: string(p.Backend), Path: in.cwd, Status: string(p.Status), RuntimeSessionID: in.resumeID,
		Permission: launchPermission(p, in.binding, argv, exact), Exclude: swarmExclusion(p),
	}
	_, a.ResumeOK = service.ResolveLocalResume(p.Backend, in.cwd, in.resumeID, in.repowireSID, in.capability)
	if p.Model != nil {
		a.Model = *p.Model
	}
	if rec := h.spawn.svc.Ownership().ValidateForPeer(p); rec.OK && rec.Record != nil {
		a.Profile = rec.Record.Profile
	}
	return a
}

// registerSwarmRoutes mounts GET /swarm/snapshot. With the experiment off it
// answers 404 and names the setting, so the CLI can say what to turn on.
func (h *Hub) registerSwarmRoutes(mux *http.ServeMux) {
	if h.spawn == nil {
		return
	}
	mux.HandleFunc("GET /swarm/snapshot", h.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if !h.swarmEnabled {
			writeError(w, http.StatusNotFound, "swarm snapshot is an experiment: set experiments.swarm_snapshot: true in ~/.repowire/config.yaml, then run `repowire service restart`")
			return
		}
		m, err := h.swarmSnapshot(r.Context())
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "swarm snapshot: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, m)
	}))
}

// WithSwarmSnapshot turns on the swarm snapshot route
// (experiments.swarm_snapshot). Needs WithSpawn.
func (h *Hub) WithSwarmSnapshot(enabled bool) *Hub {
	h.swarmEnabled = enabled
	return h
}
