// Command daemon-go is the Go hub: it wires the SQLite state store into the
// peer registry and serves the WebSocket mesh protocol over HTTP. The
// dependency-inversion seam: peer never imports state; it depends on the
// peer.Store interface, which state satisfies. This file (and hub/service,
// which import both) wire the concrete Store into the registry. Routing,
// identity, and lifecycle live in the registry/hub; this file is plumbing.
//
// This file assembles the FULL daemon: every application service (AskTracker,
// PeerDelivery, session control, spawn, jobs/work, scheduling — package
// service) is constructed and handed to the matching hub route group
// (messaging, ask-lifecycle, session wiring, schedules, reviews, shares, read
// deps) so it is live rather than a nil-guarded no-op. The wiring order is
// load-bearing (transport → registry → hub → services → reconciliation) and is
// called out at each step.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/repowire/repowire/daemon-go/cli"
	"github.com/repowire/repowire/daemon-go/codexbridge"
	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/hub"
	"github.com/repowire/repowire/daemon-go/mcpstdio"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/relay"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// realLiveness implements peer.Liveness against the OS: signal 0 probes the
// process table without delivering a signal, so a nil error means the PID is
// alive (or a zombie we don't own — close enough for ghost eviction).
type realLiveness struct{}

func (realLiveness) PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// realPaneProbe implements peer.PaneProbe: runtime evidence for an OFFLINE peer
// is a live agent_pid OR a tmux pane that still exists. agent_pid is the strong
// signal (syscall.Kill(pid,0)); a leftover pane must not keep a dead pid alive.
type realPaneProbe struct{}

func (realPaneProbe) HasRuntimeEvidence(p *proto.Peer) bool {
	if p.AgentPID != nil && *p.AgentPID > 0 {
		return syscall.Kill(*p.AgentPID, 0) == nil
	}
	if p.PaneID == nil || *p.PaneID == "" {
		return false
	}
	// tmux display-message -p -t <pane> '#{pane_pid}' exits non-zero if the pane
	// is gone. Best-effort: any error means "no evidence".
	out, err := exec.Command("tmux", "display-message", "-p", "-t", *p.PaneID, "#{pane_pid}").Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

// realProcessProbe implements peer.ProcessProbe for the destructive pane-claim
// proof: one `ps` snapshot for the ancestor walk, and `tmux display-message` for
// the pane root pid. Both are best-effort — ok=false on any error lets the claim
// through (matching the Python guard's safe default). Mirrors
// repowire.daemon.registry_repair.process_ancestors / tmux_pane_pid.
type realProcessProbe struct{}

func (realProcessProbe) Ancestors(pid int) (map[int]struct{}, bool) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil, false
	}
	parent := make(map[int]int)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		p, err1 := strconv.Atoi(f[0])
		pp, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		parent[p] = pp
	}
	ancestors := make(map[int]struct{})
	cur := pid
	for len(ancestors) < 128 {
		next, ok := parent[cur]
		if !ok || next <= 0 {
			break
		}
		if _, seen := ancestors[next]; seen {
			break // cycle guard
		}
		ancestors[next] = struct{}{}
		cur = next
	}
	return ancestors, true
}

func (realProcessProbe) PaneRootPID(paneID string) (int, bool) {
	out, err := exec.Command("tmux", "display-message", "-t", paneID, "-p", "#{pane_pid}").Output()
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, false
	}
	return pid, true
}

// ----------------------------------------------------------------------------
// Reconciliation seam adapters.
//
// reg.WithReconciliation takes peer.AskTracker + peer.PeerDelivery interface
// values, which are NARROWER, StashedAsk-projection variants of the concrete
// *service.AskTracker / *service.PeerDelivery (the service types return
// []*service.Ask and a NotifyParams-shaped Notify; the peer seams want
// []peer.StashedAsk and a positional Notify). These adapters bridge the two so
// the OFFLINE→live stash-redelivery pass and stash-loss emission run against
// the real tracker.
//
// ponytail: pure shape conversion. If the peer package later imports the
// service projection directly (or the service tracker exposes StashedAsk
// variants), these collapse away.
// ----------------------------------------------------------------------------

// reconcileAsks adapts *service.AskTracker to peer.AskTracker.
type reconcileAsks struct{ t *service.AskTracker }

func (a reconcileAsks) TakePendingRepliesForAsker(asker proto.PeerID) []peer.StashedAsk {
	return toStashed(a.t.TakePendingRepliesForAsker(asker))
}

func (a reconcileAsks) TakeOrphanPendingRepliesMatching(id peer.AskerIdentity, live map[proto.PeerID]struct{}) []peer.StashedAsk {
	return toStashed(a.t.TakeOrphanPendingRepliesMatching(service.AskerIdentity(id), live))
}

func (a reconcileAsks) MarkPendingReplyDelivered(cid string, newFrom *proto.PeerID, reason string) bool {
	return a.t.MarkPendingReplyDelivered(context.Background(), cid, newFrom, reason)
}

func (a reconcileAsks) SnapshotPendingRepliesForPeer(id proto.PeerID) []peer.StashedAsk {
	return toStashed(a.t.SnapshotPendingRepliesForPeer(id))
}

func (a reconcileAsks) SnapshotExpiredPendingReplies() []peer.StashedAsk {
	return toStashed(a.t.SnapshotExpiredPendingReplies())
}

func (a reconcileAsks) EvictExpired(includeStashed bool) int {
	return a.t.EvictExpired(context.Background(), includeStashed)
}

func (a reconcileAsks) ForgetPeer(id proto.PeerID) int {
	return a.t.ForgetPeer(context.Background(), id)
}

// toStashed projects service Asks to the read-only peer.StashedAsk shape the
// reconciler consumes.
func toStashed(asks []*service.Ask) []peer.StashedAsk {
	if len(asks) == 0 {
		return nil
	}
	out := make([]peer.StashedAsk, 0, len(asks))
	for _, a := range asks {
		s := peer.StashedAsk{
			CorrelationID:  a.CorrelationID,
			FromPeerID:     a.FromPeerID,
			FromPeerName:   a.FromPeerName,
			ToPeerID:       a.ToPeerID,
			ToPeerName:     a.ToPeerName,
			PendingReply:   a.PendingReply,
			PendingReplyAt: a.PendingReplyAt,
		}
		if a.AskerIdentity != nil {
			id := peer.AskerIdentity(*a.AskerIdentity)
			s.AskerIdentity = &id
		}
		out = append(out, s)
	}
	return out
}

// reconcileDelivery adapts *service.PeerDelivery to peer.PeerDelivery (the
// positional Notify the reconciler calls to redeliver a stashed reply).
type reconcileDelivery struct{ d *service.PeerDelivery }

func (r reconcileDelivery) Notify(ctx context.Context, from, to proto.PeerID, text string, bypassCircle bool) error {
	_, err := r.d.Notify(ctx, service.NotifyParams{
		FromPeer:     string(from),
		ToPeer:       string(to),
		Text:         text,
		BypassCircle: bypassCircle,
	})
	return err
}

// defaultDBPath resolves the state DB path: $REPOWIRE_STATE_DB wins, else
// ~/.repowire/state.db, matching the Python daemon's layout.
func defaultDBPath() string {
	if p := os.Getenv("REPOWIRE_STATE_DB"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "state.db"
	}
	return filepath.Join(home, ".repowire", "state.db")
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "help", "--help", "-h", "version", "--version":
			os.Exit(cli.Run(os.Args[1:]))
		case "serve":
			os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
			runDaemon()
			return
		case "hook":
			os.Exit(hooks.Run(os.Args[2:]))
		case "ws-hook":
			os.Exit(hooks.RunWS())
		case "chat-stream":
			os.Exit(hooks.RunChatStream(os.Args[2:]))
		case "mcp":
			os.Exit(mcpstdio.Run())
		case "codex-bridge":
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := codexbridge.Run(ctx, cli.Version); err != nil {
				log.Fatal(err)
			}
			return
		case "lifecycle":
			os.Exit(hooks.RunLifecycle(os.Args[2:]))
		}
		if !strings.HasPrefix(os.Args[1], "-") {
			os.Exit(cli.Run(os.Args[1:]))
		}
	}
	runDaemon()
}

func runDaemon() {
	// Load ~/.repowire/config.yaml FIRST so flag defaults reflect it. The Go hub
	// now reads the same file the Python code writes (auth, spawn, relay, HTTP
	// MCP), which is what lets `repowire serve` stop threading config as flags.
	// Flags still override for ad-hoc runs and the transition period. env>file>
	// default precedence is applied inside config.Load.
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	dbPath := flag.String("db", defaultDBPath(), "path to the schema-v12 SQLite state DB ($REPOWIRE_STATE_DB)")
	addr := flag.String("addr", fmt.Sprintf("%s:%d", cfg.Daemon.Host, cfg.Daemon.Port), "host:port to serve the hub on")
	hostAlias := flag.String("host", "", "bind host override (CLI compatibility alias)")
	portAlias := flag.Int("port", 0, "bind port override (CLI compatibility alias)")
	authToken := flag.String("auth-token", cfg.Daemon.AuthToken, "shared ws auth token ($REPOWIRE_AUTH_TOKEN / config daemon.auth_token); empty disables auth")
	// Spawn + relay defaults come from config.yaml (env-overridden in config.Load);
	// the flags remain as explicit overrides. Empty allowlist OR empty commands →
	// spawn disabled (SpawnService.Enabled()==false), the safe default: spawn
	// routes 503 rather than spawn into an unexpected directory.
	spawnPathsFlag := flag.String("spawn-allowed-paths", strings.Join(cfg.Daemon.Spawn.AllowedPaths, ","), "comma-separated spawn allowlist roots override ($REPOWIRE_SPAWN_ALLOWED_PATHS / config daemon.spawn.allowed_paths); empty disables spawn")
	spawnCommandsFlag := flag.String("spawn-commands", cfg.Daemon.Spawn.CommandsJSON(), "per-backend launch commands as JSON override ($REPOWIRE_SPAWN_COMMANDS / config daemon.spawn.commands); empty disables spawn")
	relayEnabled := flag.Bool("relay-enabled", cfg.Relay.Enabled, "enable relay client and /shares proxy (config relay.enabled)")
	relayAlias := flag.Bool("relay", false, "enable relay client (CLI compatibility alias)")
	_ = flag.Bool("no-install-hooks", false, "accepted for CLI compatibility; setup owns hook installation")
	relayURL := flag.String("relay-url", cfg.Relay.URL, "relay base url for the /shares proxy ($REPOWIRE_RELAY_URL / config relay.url)")
	relayKey := flag.String("relay-api-key", cfg.Relay.APIKey, "relay api key ($REPOWIRE_RELAY_API_KEY / config relay.api_key); empty leaves /shares as a 503 stub")
	flag.Parse()
	if *hostAlias != "" || *portAlias != 0 {
		host, portText, err := net.SplitHostPort(*addr)
		if err != nil {
			log.Fatalf("parse daemon addr %q: %v", *addr, err)
		}
		port, _ := strconv.Atoi(portText)
		if *hostAlias != "" {
			host = *hostAlias
		}
		if *portAlias != 0 {
			port = *portAlias
		}
		*addr = net.JoinHostPort(host, strconv.Itoa(port))
	}
	if *relayAlias {
		*relayEnabled = true
	}

	// (1) Open the state store. NewStore owns the schema-v12 bootstrap/migration
	// now, so a fresh Go daemon no longer needs Python to pre-create the DB.
	store, err := state.NewStore(*dbPath)
	if err != nil {
		log.Fatalf("open state db %q: %v", *dbPath, err)
	}

	// (2-4) Wiring order is load-bearing: build the transport FIRST so the same
	// transport is both the registry's liveness/sever seam and the socket the
	// hub serves on (ghost eviction must see the live sockets). Then build the
	// registry against it, then wrap registry+transport in the hub.
	ctx, cancelDaemon := context.WithCancel(context.Background())
	defer cancelDaemon()
	liveness := realLiveness{}
	transport := service.NewWebSocketTransport()
	transport.EnableACP(cfg.Experiments.ACPBrokerClient)

	reg, err := peer.NewRegistry(ctx, store, liveness, transport)
	if err != nil {
		_ = store.Close()
		log.Fatalf("hydrate registry: %v", err)
	}
	if events, err := store.LoadRecentEvents(ctx, 500); err != nil {
		log.Printf("hydrate events: %v", err)
	} else {
		reg.HydrateEvents(events)
	}
	reg.ConfigureDurations(time.Duration(cfg.Daemon.HeartbeatInterval)*time.Second, time.Duration(cfg.Daemon.PeerReapTTLSeconds*float64(time.Second)), time.Duration(cfg.Daemon.DescriptionTTLSeconds*float64(time.Second)))

	// (5) NewHubWithTransport wires reg.OnOffline -> tracker.CancelQueriesToPeer
	// internally, so a terminal/transport offline cascades query cancellation
	// without the registry learning the tracker's shape. It also builds the
	// QueryTracker + MessageRouter the delivery/session groups need.
	h := hub.NewHubWithTransport(reg, transport, *authToken)
	selfMachine, _ := os.Hostname()

	// (6) Application services. AskTracker is the in-memory open-ask store;
	// PeerDelivery composes registry-access + transport-choice + ask/notify
	// lifecycle + the durable queued-delivery fallback (store satisfies the
	// queue seam). The router (WS-only) is the one the hub minted in step 5.
	asks := service.NewAskTracker(time.Duration(cfg.Daemon.PruneMaxAgeHours * float64(time.Hour)))
	delivery := service.NewPeerDelivery(reg, h.Router(), transport, asks, store).WithQueueConfig(cfg.Daemon.DeliveryQueueTTLSeconds, cfg.Daemon.DeliveryQueueMaxPerPeer).WithOperationStore(store).WithOrchestratorRecall(cfg.Daemon.OrchestratorRecall)
	if recovered := service.ReconcileACPInflight(ctx, store, cfg.Daemon.DeliveryQueueTTLSeconds, cfg.Daemon.DeliveryQueueMaxPerPeer); recovered > 0 {
		log.Printf("acp reconcile: closed %d ask(s) lost across restart", recovered)
	}
	transport.SetACPPermissionHandler(service.NewACPPermissionHandler(asks, func(kind string, data map[string]any) {
		reg.AddEvent(ctx, kind, data)
	}))

	// (7) Reconciliation seams. Inject AskTracker + PeerDelivery (via the shape
	// adapters) so the OFFLINE->live stash-redelivery pass and stash-loss
	// emission are LIVE (no longer the nil/dormant spike path). The PaneProbe is
	// the production ps/tmux runtime-evidence gate; the experiment flag and TTLs
	// are defaults until config wiring lands.
	reg.WithReconciliation(
		reconcileAsks{asks},
		reconcileDelivery{delivery},
		realPaneProbe{},
		peer.ExperimentsConfig{ACPBrokerClient: cfg.Experiments.ACPBrokerClient},
		time.Duration(cfg.Daemon.StaleBusyTimeoutSeconds*float64(time.Second)),
		time.Duration(cfg.Daemon.PruneMaxAgeHours*float64(time.Hour)),
	)
	// Process/tmux probe for the destructive pane-claim proof (hijack guard).
	reg.WithProcessProbe(realProcessProbe{})

	// (8) Spawn area. The SpawnService owns the real tmux controller + durable
	// pane-ownership store (proof for destructive kill/restart). SessionControl
	// shares the SAME *SpawnService instance with the work/jobs runner so an
	// executor acquired for a durable job records ownership the spawn routes can
	// later consult.
	tmuxCtl := service.NewRealTmuxController()
	ownership := service.NewFileOwnership(selfMachine, tmuxCtl.ProbePane)
	// Per-backend launch commands come from -spawn-commands (JSON), which
	// `repowire serve` populates from config.daemon.spawn.commands. An empty/invalid
	// map keeps spawn disabled-by-default (SpawnService.Enabled()==false → 503);
	// allowedPaths gates the rest.
	spawnCommands := map[proto.AgentType]string{}
	if s := *spawnCommandsFlag; s != "" {
		if err := json.Unmarshal([]byte(s), &spawnCommands); err != nil {
			log.Fatalf("parse -spawn-commands JSON: %v", err)
		}
	}
	profiles := map[proto.AgentType]map[string][]string{}
	for backend, items := range cfg.Daemon.Spawn.Profiles {
		typed := map[string][]string{}
		for name, profile := range items {
			typed[name] = profile.Args
		}
		profiles[proto.AgentType(backend)] = typed
	}
	spawnEnv := map[string]string{}
	for key, value := range cfg.Daemon.Spawn.Env {
		spawnEnv[key] = value
	}
	if len(cfg.Daemon.Spawn.EnvPath) > 0 {
		spawnEnv["PATH"] = strings.Join(cfg.Daemon.Spawn.EnvPath, string(os.PathListSeparator))
	} else if _, configured := spawnEnv["PATH"]; !configured {
		if path := captureLoginShellPath(); path != "" {
			spawnEnv["PATH"] = path
		}
	}
	spawnService := service.NewSpawnService(tmuxCtl, ownership, spawnCommands, config.SplitCSV(*spawnPathsFlag)).WithRuntimeConfig(profiles, spawnEnv)

	// (9) Work/jobs + scheduler. SessionControl is the executor-acquisition
	// ladder (assigned → reuse → resume → spawn); JobRunner dispatches durable
	// jobs through PeerDelivery.OpenScheduledAsk (reply_delivery=pull). The
	// Scheduler fires one-shot/recurring check-ins off a deadline-driven sleep.
	// JobRunner.SetSenderPeerID is also unset: the synthetic @jobs service peer
	// isn't registered here, so dispatch asks carry an empty `from`
	// (accessRegistry treats an unresolved sender as allowed, mirroring Python
	// notify behavior).
	sessionControl := service.NewSessionControl(reg, spawnService, store).WithResume(service.ResolveLocalResume)
	jobRunner := service.NewJobRunner(store, delivery, sessionControl)
	jobCompletion := service.NewJobCompletion(store, asks, sessionControl, reg, delivery)
	reg.OnTerminalOffline = jobCompletion.OnPeerTerminalOffline
	scheduler := service.NewScheduler(store, delivery)

	// (10) Relay/shares config. A nil-or-keyless RelayConfig leaves /shares as
	// the documented degrade (503 POST/DELETE, empty-list GET) — i.e. the 503
	// stub when the relay isn't configured.
	relayCfg := &hub.RelayConfig{
		Enabled: *relayEnabled && *relayKey != "",
		URL:     *relayURL,
		APIKey:  *relayKey,
	}
	// The relay CLIENT (outbound tunnel to repowire.io). Separate from the /shares
	// proxy above: this is what makes the dashboard/phone reach THIS daemon. nil
	// when relay is disabled. Dials the relay and forwards tunneled requests to the
	// local HTTP surface (127.0.0.1) — same trust model as the Python client.
	var relayClient *relay.Client
	if relayCfg.Enabled {
		relayClient = relay.NewClient(relayCfg.URL, relayCfg.APIKey, selfMachine, "http://"+*addr).WithAuthToken(cfg.Daemon.AuthToken)
	}

	// (11) Wire EVERY route group onto the hub. Each With* gates a route group
	// that is otherwise a nil-guarded no-op in Routes(); the order is
	// independent (all read from already-built services). traces uses the same
	// *state.Store delivery-trace store.
	h.WithReadDeps(asks, store).
		WithMessaging(delivery, store).
		WithAskLifecycle(asks, delivery, reg).
		WithSessionRoutes(reg, store).
		WithSpawn(spawnService, reg, asks, selfMachine, cfg.Daemon.CircleBoundary).
		WithWork(jobRunner, store, reg).
		WithJobCompletion(jobCompletion).
		WithSchedules(store, scheduler).
		WithShares(relayCfg).
		// HTTP MCP (/mcp) — config-gated (cfg.Daemon.MCPHTTP.Enabled); reuses the
		// same delivery service the notify/broadcast routes use. No-op route when
		// disabled, so /mcp falls through to the dashboard catch-all as before.
		WithMCP(cfg.Daemon.MCPHTTP, delivery).
		// forgetSpawnedPane drops a dead pane from the spawn-ownership store so
		// destructivePaneProof can't authorize kill/restart against a reused pane id.
		// clearPaneRuntimeState is nil here because NewLifecycleHandler defaults
		// it to hooks.ClearPaneRuntimeState.
		WithLifecycle(hub.NewLifecycleHandler(reg, transport, hub.TmuxPaneLister{}, spawnService.Ownership().Forget, nil, cfg.Daemon.CircleBoundary).
			WithPlacementUpdater(spawnService.Ownership().UpdatePlacement))
	// Reviews defaults its JSON store at Routes() time if unset; leave it.

	// Surface relay-client status on /health and drive its lazy self-heal there
	// (mirrors the Python /health relay block + ensure_running).
	if relayClient != nil {
		h.WithRelayStatus(func() map[string]any {
			relayClient.EnsureRunning(ctx)
			return relayClient.Status().HealthMap()
		})
	}

	// (12) Start the background dispatch loops (deadline-driven, never polling).
	jobRunner.Start(ctx)
	scheduler.Start(ctx)
	reconcileDone := make(chan struct{})
	go func() {
		defer close(reconcileDone)
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			jobCompletion.ReconcileInflight(ctx)
		case <-ctx.Done():
		}
	}()
	if relayClient != nil {
		relayClient.Start(ctx)
	}

	// (13) Register HTTP/ws handlers.
	mux := http.NewServeMux()
	h.Routes(mux)
	registerDashboardRoutes(mux, findWebOutputDir())

	srv := &http.Server{Addr: *addr, Handler: mux}

	// (14) Graceful shutdown on SIGINT/SIGTERM: stop accepting, drain, stop the
	// dispatch loops, close DB.
	shutCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("repowire hub listening on %s (db=%s, auth=%v, spawn=%v, relay=%v)",
			*addr, *dbPath, *authToken != "", spawnService.Enabled(), relayCfg.Enabled)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			cancelDaemon()
			<-reconcileDone
			jobRunner.Stop()
			scheduler.Stop()
			_ = store.Close()
			log.Fatalf("serve: %v", err)
		}
	case <-shutCtx.Done():
		log.Printf("shutdown signal received, draining...")
		drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(drainCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}

	cancelDaemon()
	<-reconcileDone
	jobRunner.Stop()
	scheduler.Stop()
	if relayClient != nil {
		relayClient.Stop()
	}
	// delivery.Close() before reg.Close() is load-bearing: closing PeerDelivery's
	// closeCh unblocks any deferBroadcastUntilSeedSettled goroutine parked in the
	// (up to 25s) seed-gate poll, so the registry's Close (which joins
	// redelivery/LazyRepairAsync goroutines, some of which call back into
	// delivery) doesn't itself wait out that poll. Both must finish before
	// store.Close() — either can still touch the DB.
	//
	// Known limitation (tracked as repowire-fk4): per-connection WS handler
	// goroutines spawned by HandleWS are NOT drained here — srv.Shutdown stops
	// accepting new connections but does not close already-hijacked sockets. A
	// live WS read loop can still be running when store.Close() returns; closing
	// it requires a hub-level socket sweep, which is out of scope for this fix.
	delivery.Close()
	transport.CloseACP()
	reg.Close()
	if err := store.Close(); err != nil {
		log.Printf("close state db: %v", err)
	}
	log.Printf("hub stopped")
}

func captureLoginShellPath() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, shell, "-lc", `printf 'REPOWIRE_PATH_START:%s:REPOWIRE_PATH_END' "$PATH"`).Output()
	if err != nil {
		return ""
	}
	text := string(out)
	start := strings.LastIndex(text, "REPOWIRE_PATH_START:")
	if start < 0 {
		return ""
	}
	start += len("REPOWIRE_PATH_START:")
	end := strings.Index(text[start:], ":REPOWIRE_PATH_END")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}

const dashboardFallback = "Dashboard not found. Please run 'repowire build-ui'."

func findWebOutputDir() string {
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "web", "out"),
			filepath.Join(cwd, "..", "web", "out"),
		)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "web", "out"),
			filepath.Join(dir, "..", "web", "out"),
			filepath.Join(dir, "..", "..", "web", "out"),
		)
	}
	for _, dir := range candidates {
		if stat, err := os.Stat(filepath.Join(dir, "dashboard.html")); err == nil && !stat.IsDir() {
			return filepath.Clean(dir)
		}
	}
	return ""
}

func registerDashboardRoutes(mux *http.ServeMux, webOut string) {
	if webOut != "" {
		nextStatic := filepath.Join(webOut, "_next")
		if stat, err := os.Stat(nextStatic); err == nil && stat.IsDir() {
			mux.Handle("/_next/", http.StripPrefix("/_next/", http.FileServer(http.Dir(nextStatic))))
		}
	}

	serveDashboard := func(w http.ResponseWriter, r *http.Request) {
		if webOut != "" {
			dashboardPath := filepath.Join(webOut, "dashboard.html")
			if stat, err := os.Stat(dashboardPath); err == nil && !stat.IsDir() {
				http.ServeFile(w, r, dashboardPath)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardFallback))
	}

	mux.HandleFunc("/dashboard", serveDashboard)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			if webOut != "" {
				for _, name := range []string{"dashboard.html", "index.html"} {
					path := filepath.Join(webOut, name)
					if stat, err := os.Stat(path); err == nil && !stat.IsDir() {
						http.ServeFile(w, r, path)
						return
					}
				}
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(dashboardFallback))
			return
		}
		if webOut == "" {
			http.NotFound(w, r)
			return
		}
		serveExport(w, r, webOut)
	})
}

// serveExport serves the Next.js `output: export` tree, mirroring the hosted
// nginx config (web-image/nginx.conf): `try_files $uri $uri.html
// $uri/index.html =404` with `error_page 404 /404.html`. A bare
// http.FileServer 404s (or 301s into a dead dir) on extensionless routes like
// /docs/concepts, whose export file is docs/concepts.html — so resolve the
// .html / index.html variants before falling back to the export's 404 page.
func serveExport(w http.ResponseWriter, r *http.Request, webOut string) {
	// Leading-slash Clean collapses any ".." so the join cannot escape webOut.
	rel := filepath.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	for _, cand := range []string{
		filepath.Join(webOut, rel),
		filepath.Join(webOut, rel+".html"),
		filepath.Join(webOut, rel, "index.html"),
	} {
		if stat, err := os.Stat(cand); err == nil && !stat.IsDir() {
			http.ServeFile(w, r, cand)
			return
		}
	}
	// try_files =404 → error_page 404 /404.html, served WITH a 404 status
	// (http.ServeFile would force 200, so write the body ourselves).
	if body, err := os.ReadFile(filepath.Join(webOut, "404.html")); err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(body)
		return
	}
	http.NotFound(w, r)
}
