package service

// Container is the socket that owns a spawned agent's execution: where it
// runs, how it starts and stops, and the evidence that it is still there
// (rfcs/agent-container.md). The tmux plug (container_tmux.go) is the
// default and today the only one; ACP, Codex App Server threads and hosted
// agents are later plugs. A place is named by a plug-scoped id (for tmux,
// the pane id).
//
// Slice 1 extracts the socket with today's semantics unchanged: Probe still
// answers with tmux evidence, and the destructive proof stays in the routes.
type Container interface {
	// Kind names the plug ("tmux").
	Kind() string
	// Reserve creates a place without launching into it (restart reserves
	// the new place before it stops the old runtime).
	Reserve(cfg SpawnConfig) (PanePlacement, error)
	// Launch starts the agent, in cfg.Reserved when set, else in a new place.
	Launch(cfg SpawnConfig) (SpawnResult, error)
	// Stop removes the place and what runs in it; true on success. Callers
	// prove ownership first.
	Stop(placeID string) bool
	// Probe returns live evidence for a place, or nil when it is gone.
	Probe(placeID string) *TmuxPaneEvidence
}
