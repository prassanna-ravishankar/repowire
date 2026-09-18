package proto

// PeerSource is how a peer reached the mesh. It is orthogonal to Backend
// (which runtime) and to tmux placement (PaneID/TmuxSession).
type PeerSource string

const (
	// SourceHook is a runtime hook registration (Claude Code, OpenCode, Pi, or
	// codex CLI in a pane).
	SourceHook PeerSource = "hook"
	// SourceCodexAppServer is a thread registered by the codex bridge from the
	// Codex App Server socket (ChatGPT desktop, VS Code, or codex CLI threads).
	SourceCodexAppServer PeerSource = "codex-app-server"
	// SourceUnknown is a peer whose registration predates provenance or that
	// never declared one. It is a first-class value, never guessed away.
	SourceUnknown PeerSource = "unknown"
)

func (s PeerSource) Valid() bool {
	switch s {
	case SourceHook, SourceCodexAppServer, SourceUnknown:
		return true
	}
	return false
}

// PeerInitiator is who opened the runtime session behind a peer. Empty means
// the runtime did not say (hooks today), which lists like a user session.
type PeerInitiator string

const (
	// InitiatorUser: a person opened it (Codex threadSource "user").
	InitiatorUser PeerInitiator = "user"
	// InitiatorAgent: another agent spawned it (Codex threadSource "subagent").
	InitiatorAgent PeerInitiator = "agent"
	// InitiatorSystem: runtime machinery (Codex threadSource "system", e.g.
	// helper threads the desktop app spins up). Hidden from the default view.
	InitiatorSystem PeerInitiator = "system"
)

func (i PeerInitiator) Valid() bool {
	switch i {
	case "", InitiatorUser, InitiatorAgent, InitiatorSystem:
		return true
	}
	return false
}

// Provenance records where a peer came from and whether the mesh may address
// it. Fields are orthogonal on purpose: a sub-agent thread can be ephemeral,
// and an ephemeral thread can still accept direct input. Addressable is the
// runtime's own verdict (Codex publishes canAcceptDirectInput); a false value
// is inbound-only, the peer can still ack, reply, and notify.
type Provenance struct {
	Source    PeerSource    `json:"source"`
	Initiator PeerInitiator `json:"initiator,omitempty"`
	// ParentRuntimeID is the parent's runtime thread id (for example a Codex
	// parentThreadId). It is NOT a peer_id; the hub resolves parent_peer_id at
	// read time and leaves it empty when the parent is not registered.
	ParentRuntimeID string `json:"parent_runtime_id,omitempty"`
	Ephemeral       bool   `json:"ephemeral,omitempty"`
	Addressable     bool   `json:"addressable"`
	// AddressableReason explains a false Addressable, e.g.
	// "subagent_direct_input_denied".
	AddressableReason string `json:"addressable_reason,omitempty"`
}

// DefaultProvenance is what a peer has until something classifies it: unknown
// source, addressable. Migrated rows and hook registrations start here.
func DefaultProvenance() Provenance {
	return Provenance{Source: SourceUnknown, Addressable: true}
}

// IsZero reports whether p carries no classification at all (the JSON '{}'
// stored before migration 14 populated the column).
func (p Provenance) IsZero() bool {
	return p == Provenance{}
}

// Normalized fills an unclassified value with DefaultProvenance and rejects an
// invalid source by mapping it to unknown.
func (p Provenance) Normalized() Provenance {
	if p.IsZero() {
		return DefaultProvenance()
	}
	if !p.Source.Valid() {
		p.Source = SourceUnknown
	}
	if !p.Initiator.Valid() {
		p.Initiator = ""
	}
	return p
}

// Merge applies a fresh classification onto the stored one. A nil update keeps
// the stored value; an explicit update wins outright. Inference (InferSource)
// is applied by the caller only while Source is still unknown.
func (p Provenance) Merge(update *Provenance) Provenance {
	if update == nil {
		return p.Normalized()
	}
	return update.Normalized()
}

// InferSource fills an unknown source from registration evidence without
// touching any other field: the codex bridge advertises transport
// codex-app-server, and a hook_version means a runtime hook. Anything else
// stays unknown.
func (p Provenance) InferSource(metadata map[string]any, hookVersion bool) Provenance {
	p = p.Normalized()
	if p.Source != SourceUnknown {
		return p
	}
	if transport, _ := metadata["transport"].(string); transport == "codex-app-server" {
		p.Source = SourceCodexAppServer
	} else if hookVersion {
		p.Source = SourceHook
	}
	return p
}
