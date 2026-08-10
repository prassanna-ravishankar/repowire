// Package service holds the application-service layer of the Go daemon: ask
// lifecycle (AskTracker), delivery (PeerDelivery, MessageRouter, QueryTracker),
// spawn (SpawnService, PaneOwnership, pane-runtime helpers), session/executor
// acquisition (SessionControl, JobRunner), and scheduling (Scheduler, cron).
// These are the pieces the hub package's HTTP/WS routes compose.
//
// Layering rule: service must never import hub. hub is the route/transport
// layer — it decodes requests, calls into service, and writes responses; the
// dependency points one way, hub -> service, so a service type can never
// reference an hub HTTP concept (net/http, Hub, writeJSON, requireAuth, …).
// If a service file ever needs something hub-shaped, that's a sign the
// abstraction belongs at a lower layer instead — Go's import graph rejects an
// hub -> service -> hub cycle outright, so this boundary is compiler-enforced,
// not just a convention.
//
// Both hub and service sit on the same foundation: peer (the lifecycle
// registry), proto (the wire contract), and state (the SQLite store).
package service
