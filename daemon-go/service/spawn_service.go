package service

// spawn_service.go owns tmux exec + ownership recording for the spawn area,
// ported from repowire.spawn.spawn_peer + repowire.daemon.spawn_service.SpawnService
// + repowire.spawn_hints.write_hint. The registry/store stay pure: the service
// places and launches through the injected Container (container.go) and records durable proof. Path
// allowlisting and command resolution gate every spawn before any pane is created
// (fail loud over silent degrade — a misconfigured spawn 403/422s, never spawns
// into an unexpected directory).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// sha256Hex16 returns the first 16 hex chars of sha256(s), matching the Python
// _hint_key derivation (hexdigest()[:16]).
func sha256Hex16(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// SpawnConfig is the input to a spawn. Path must be a caller-pre-resolved
// absolute project dir; Circle is the logical routing circle. Window boundary
// spawns split TargetPane; session-boundary spawns create a window in Circle.
type SpawnConfig struct {
	Path           string
	Circle         string
	CircleBoundary proto.CircleBoundary
	TargetPane     string
	Backend        proto.AgentType
	Command        string
	Profile        string
	Message        *string
	Role           proto.PeerRole
	PeerID         *proto.PeerID
	Env            map[string]string
	// SameWindow splits TargetPane's window even under the session boundary
	// (restart keeps the replacement beside the pane it replaces). A failed
	// split falls back to the boundary's normal placement.
	SameWindow bool
	// Reserved is a pane created earlier by Container.Reserve; Launch uses it
	// instead of creating one.
	Reserved *PanePlacement
}

// PanePlacement is a created-but-unlaunched pane and its tmux locator.
type PanePlacement struct {
	PaneID      string
	TmuxSession string
	DisplayName string
}

// SpawnResult is the outcome of a spawn: the (possibly suffixed) display name, the
// "circle:window" tmux session string, the stable pane id, and the warmup intent
// echo.
type SpawnResult struct {
	DisplayName string
	TmuxSession string
	PaneID      string
	Circle      string
	Message     *string
}

// SpawnError carries an HTTP status + structured detail so the routes can surface
// the Python HTTPException shape (a string detail or a {"error","hint",...} map).
type SpawnError struct {
	Status int
	Detail any
}

func (e *SpawnError) Error() string {
	if s, ok := e.Detail.(string); ok {
		return s
	}
	if m, ok := e.Detail.(map[string]any); ok {
		if hint, ok := m["hint"].(string); ok {
			return hint
		}
	}
	return fmt.Sprintf("spawn error (%d)", e.Status)
}

// AsSpawnError unwraps a *SpawnError from err, if present.
func AsSpawnError(err error) (*SpawnError, bool) {
	var se *SpawnError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

// SpawnService spawns peers while preserving /spawn validation and ownership
// semantics. It owns the Container + PaneOwnership; commands/allowedPaths
// come from config (passed as plain maps/slices — there is no Go config package
// yet).
type SpawnService struct {
	container     Container
	peerPlugs     []PeerContainer
	own           PaneOwnership
	boundary      proto.CircleBoundary
	commands      map[proto.AgentType]string
	commandLoader func() (map[proto.AgentType]string, error)
	allowedPaths  []string
	profiles      map[proto.AgentType]map[string][]string
	env           map[string]string
	// codexOnHooks reports whether Codex uses the hooks transport; nil reads
	// the installed hooks (codexHooksTransport).
	codexOnHooks func() bool
}

// WithCircleBoundary applies the daemon-wide circle boundary to every spawn path.
func (s *SpawnService) WithCircleBoundary(boundary proto.CircleBoundary) *SpawnService {
	if boundary == "" {
		boundary = proto.CircleBoundarySession
	}
	s.boundary = boundary
	return s
}

func (s *SpawnService) WithRuntimeConfig(profiles map[proto.AgentType]map[string][]string, env map[string]string) *SpawnService {
	s.profiles, s.env = profiles, env
	return s
}

// WithCommandLoader makes backend command resolution request-driven. Setup can
// update config on disk without restarting the daemon (and bouncing every mesh
// connection); the next real spawn observes the new command. The startup map
// remains the fallback for tests and explicit flag-only daemon launches.
func (s *SpawnService) WithCommandLoader(loader func() (map[proto.AgentType]string, error)) *SpawnService {
	s.commandLoader = loader
	return s
}

func (s *SpawnService) currentCommands() (map[proto.AgentType]string, error) {
	if s.commandLoader == nil {
		return s.commands, nil
	}
	commands, err := s.commandLoader()
	if err != nil {
		return nil, err
	}
	return commands, nil
}

// NewSpawnService constructs the service over an injected Container and
// PaneOwnership store. commands is the per-backend launch line; allowedPaths is
// the spawn allowlist root set. Empty commands OR empty allowedPaths means spawn
// is disabled (ValidatePath 403s).
func NewSpawnService(container Container, own PaneOwnership, commands map[proto.AgentType]string, allowedPaths []string) *SpawnService {
	return &SpawnService{container: container, own: own, boundary: proto.CircleBoundarySession, commands: commands, allowedPaths: allowedPaths}
}

// Ownership exposes the PaneOwnership store so the routes (kill/restart/switch)
// can consult/record proof without a second store instance.
func (s *SpawnService) Ownership() PaneOwnership { return s.own }

// Container exposes the plug so routes can stop and probe what spawn placed.
func (s *SpawnService) Container() Container { return s.container }

// WithPeerContainers adds plugs that own peers outright (rfcs/agent-container.md).
// Call while wiring.
func (s *SpawnService) WithPeerContainers(plugs ...PeerContainer) *SpawnService {
	s.peerPlugs = append(s.peerPlugs, plugs...)
	return s
}

// PeerContainerFor returns the plug that owns p outright, or nil when p is
// pane-backed or a peer Repowire does not run.
func (s *SpawnService) PeerContainerFor(p *proto.Peer) PeerContainer {
	for _, plug := range s.peerPlugs {
		if plug.Owns(p) {
			return plug
		}
	}
	return nil
}

// Enabled reports whether spawn is configured (commands AND allowed_paths set).
func (s *SpawnService) Enabled() bool {
	commands, err := s.currentCommands()
	return err == nil && len(commands) > 0 && len(s.allowedPaths) > 0
}

// Commands returns the configured per-backend launch lines (for /spawn/config).
func (s *SpawnService) Commands() map[proto.AgentType]string {
	commands, err := s.currentCommands()
	if err != nil {
		return s.commands
	}
	return commands
}

// ConfiguredModels lists the model ids that appear after --model/-m in the
// configured spawn commands and profiles: the only model values a redacted
// restart command shows.
func (s *SpawnService) ConfiguredModels() map[string]bool {
	models := map[string]bool{}
	collect := func(values []string) {
		for i, v := range values {
			if name, value, ok := strings.Cut(v, "="); ok && (name == "--model" || name == "-m") {
				models[value] = true
			} else if (v == "--model" || v == "-m") && i+1 < len(values) {
				models[values[i+1]] = true
			}
		}
	}
	for _, command := range s.Commands() {
		if words, ok := splitShellWords(command); ok {
			values := make([]string, 0, len(words))
			for _, w := range words {
				values = append(values, w.Value)
			}
			collect(values)
		}
	}
	for _, byName := range s.profiles {
		for _, args := range byName {
			collect(args)
		}
	}
	return models
}

// Profiles returns the configured launch profiles for spawn discovery.
func (s *SpawnService) Profiles() map[proto.AgentType]map[string][]string { return s.profiles }

// AllowedPaths returns the configured spawn allowlist roots (for /spawn/config).
func (s *SpawnService) AllowedPaths() []string { return s.allowedPaths }

// ValidatePath ports SpawnService.validate_path: realpath the path, 403 when
// spawn is disabled, 404 when the path is missing, 403 when it is not under any
// allowed root. Returns the resolved absolute path.
func (s *SpawnService) ValidatePath(path string) (string, error) {
	if !s.Enabled() {
		return "", &SpawnError{Status: 403, Detail: "Spawn is disabled. Set daemon.spawn.commands and daemon.spawn.allowed_paths in ~/.repowire/config.yaml"}
	}
	resolved := NormPath(path)
	if _, err := os.Stat(resolved); err != nil {
		return "", &SpawnError{Status: 404, Detail: "Path does not exist: " + path}
	}
	for _, root := range s.allowedPaths {
		r := NormPath(root)
		if resolved == r || strings.HasPrefix(resolved, r+string(os.PathSeparator)) {
			return resolved, nil
		}
	}
	return "", &SpawnError{Status: 403, Detail: "Path not under any allowed_paths: " + path}
}

// ResolveCommand ports SpawnService.resolve_command: 422 command_unavailable when
// no commands entry maps to the backend. An unknown profile is 422.
func (s *SpawnService) ResolveCommand(b proto.AgentType, profile *string) (string, error) {
	commands, err := s.currentCommands()
	if err != nil {
		return "", &SpawnError{Status: 500, Detail: "reload spawn commands: " + err.Error()}
	}
	command := commands[b]
	if command == "" {
		return "", &SpawnError{Status: 422, Detail: map[string]any{
			"error":   "command_unavailable",
			"hint":    fmt.Sprintf("No daemon.spawn.commands entry for %q. Add it to ~/.repowire/config.yaml.", string(b)),
			"backend": string(b),
		}}
	}
	if profile != nil && *profile != "" {
		args, ok := s.profiles[b][*profile]
		if !ok {
			return "", &SpawnError{Status: 422, Detail: map[string]any{
				"error":   "profile_unavailable",
				"hint":    fmt.Sprintf("No daemon.spawn.profiles.%s.%s entry in ~/.repowire/config.yaml.", string(b), *profile),
				"backend": string(b),
				"profile": *profile,
			}}
		}
		for _, arg := range args {
			command += " " + ShellQuote(arg)
		}
	}
	if err := s.validateCommandHead(command); err != nil {
		return "", err
	}
	if b == proto.AgentCodex {
		source := "daemon.spawn.commands.codex"
		if profile != nil && *profile != "" {
			source += " or daemon.spawn.profiles.codex." + *profile
		}
		if err := s.CheckCodexLaunch(command, source+" in ~/.repowire/config.yaml"); err != nil {
			return "", err
		}
	}
	return command, nil
}

// CodexOnHooks reports whether Codex uses the hooks transport rather than the
// codex bridge on Codex's shared App Server, in the Codex home of env (the
// spawn environment when nil).
func (s *SpawnService) CodexOnHooks(env map[string]string) bool {
	if env == nil {
		env = s.env
	}
	if s.codexOnHooks != nil {
		return s.codexOnHooks()
	}
	return codexHooksTransport(env)
}

// CheckCodexLaunch refuses a Codex launch that would run Codex's private
// embedded App Server while Repowire delivers through the shared one: the
// pane would register but never receive a message (#567). source names where
// the launch came from, for the hint.
func (s *SpawnService) CheckCodexLaunch(command, source string) error {
	words, ok := splitShellWords(command)
	if !ok {
		return nil
	}
	env := codexLaunchEnv(words[:programIndex(words)], s.env)
	reason := CodexEmbeddedReason(command, env)
	if reason == "" || s.CodexOnHooks(env) {
		return nil
	}
	return &SpawnError{Status: 422, Detail: map[string]any{
		"error":  "codex_embedded_app_server",
		"reason": reason,
		"hint": fmt.Sprintf("Codex runs a private embedded App Server when launched with %s, and Repowire reaches Codex sessions only through the shared App Server, "+
			"so this peer would register but never receive messages. Remove %s from %s and set it in ~/.codex/config.toml instead "+
			"(--model, -s, -a, --enable and --disable keep the shared server; feature toggles must be spelled features.<name>).", reason, reason, source),
		"backend": string(proto.AgentCodex),
	}}
}

func (s *SpawnService) validateCommandHead(command string) error {
	head := ""
	for rest := strings.TrimSpace(command); rest != ""; {
		word, next, ok := firstShellWord(rest)
		if !ok {
			return &SpawnError{Status: 422, Detail: "configured spawn command has unmatched quotes"}
		}
		rest = strings.TrimSpace(next)
		if word == "env" || strings.Contains(word, "=") {
			continue
		}
		head = word
		break
	}
	if head == "" {
		return &SpawnError{Status: 422, Detail: "configured spawn command is empty"}
	}
	pathValue := s.env["PATH"]
	if pathValue == "" {
		pathValue = os.Getenv("PATH")
	}
	if commandExists(head, pathValue) {
		return nil
	}
	return &SpawnError{Status: 422, Detail: map[string]any{
		"error": "command_unavailable",
		"hint":  fmt.Sprintf("Configured spawn command %q is not executable on PATH.", head),
	}}
}

func firstShellWord(input string) (string, string, bool) {
	var word strings.Builder
	quote := rune(0)
	escaped := false
	for i, r := range input {
		if escaped {
			word.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' {
			return word.String(), input[i+1:], true
		}
		word.WriteRune(r)
	}
	if quote != 0 || escaped {
		return "", "", false
	}
	return word.String(), "", true
}

func commandExists(command, pathValue string) bool {
	if strings.ContainsRune(command, os.PathSeparator) {
		info, err := os.Stat(command)
		return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
	}
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = "."
		}
		info, err := os.Stat(filepath.Join(dir, command))
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

// PrepareSpawn applies the daemon-wide boundary and proves window placement.
func (s *SpawnService) PrepareSpawn(cfg SpawnConfig) (SpawnConfig, error) {
	cfg.CircleBoundary = s.boundary
	if cfg.CircleBoundary != proto.CircleBoundaryWindow {
		return cfg, nil
	}
	if s.container == nil {
		return SpawnConfig{}, &SpawnError{Status: 503, Detail: "agent container is not configured"}
	}
	if cfg.TargetPane == "" {
		id := strings.TrimPrefix(cfg.Circle, "window-")
		if id == "" || strings.Trim(id, "0123456789") != "" {
			return SpawnConfig{}, &SpawnError{Status: 422, Detail: "window-boundary circle must be a stable window-N id"}
		}
		cfg.TargetPane = "@" + id
	}
	evidence := s.container.Probe(cfg.TargetPane)
	if evidence == nil {
		return SpawnConfig{}, &SpawnError{Status: 409, Detail: "window-boundary spawn target is not live"}
	}
	circle := proto.TmuxCircle(proto.CircleBoundaryWindow, evidence.SessionName, evidence.WindowID)
	if circle == "" {
		return SpawnConfig{}, &SpawnError{Status: 409, Detail: "window-boundary spawn target has invalid tmux evidence"}
	}
	if cfg.Circle == "" {
		cfg.Circle = circle
	} else if circle != cfg.Circle {
		return SpawnConfig{}, &SpawnError{Status: 409, Detail: "window-boundary spawn target contradicts the requested circle"}
	}
	cfg.TargetPane = evidence.WindowID
	return cfg, nil
}

// Spawn validates, launches through tmux, and records durable pane ownership.
func (s *SpawnService) Spawn(cfg SpawnConfig) (SpawnResult, error) {
	var err error
	cfg, err = s.PrepareSpawn(cfg)
	if err != nil {
		return SpawnResult{}, err
	}
	resolvedPath, err := s.ValidatePath(cfg.Path)
	if err != nil {
		return SpawnResult{}, err
	}
	command := cfg.Command
	if command == "" {
		command, err = s.ResolveCommand(cfg.Backend, nil)
		if err != nil {
			return SpawnResult{}, err
		}
	}
	if cfg.Circle == "" {
		return SpawnResult{}, &SpawnError{Status: 422, Detail: "Circle is required; run inside a tmux session or pass --circle"}
	}
	if cfg.Role == "" {
		cfg.Role = proto.RoleAgent
	}
	cfg.Path = resolvedPath
	cfg.Command = command
	return s.Launch(s.withEnv(cfg))
}

func (s *SpawnService) withEnv(cfg SpawnConfig) SpawnConfig {
	env := map[string]string{}
	for key, value := range s.env {
		env[key] = value
	}
	for key, value := range cfg.Env {
		env[key] = value
	}
	cfg.Env = env
	return cfg
}

// EnvKeys lists the environment variables every launch is prefixed with, so a
// printed command can name them without leaking their values.
func (s *SpawnService) EnvKeys() []string {
	return slices.Sorted(maps.Keys(s.env))
}

// Reserve proves placement and creates the pane a later Launch fills. It skips
// the allowed_paths gate: its callers relaunch into a daemon-observed cwd, not
// a caller-supplied one. cfg.Command must already be resolved.
func (s *SpawnService) Reserve(cfg SpawnConfig) (SpawnConfig, error) {
	cfg, err := s.PrepareSpawn(cfg)
	if err != nil {
		return SpawnConfig{}, err
	}
	cfg.Path = NormPath(cfg.Path)
	if info, err := os.Stat(cfg.Path); err != nil || !info.IsDir() {
		return SpawnConfig{}, &SpawnError{Status: 404, Detail: "Path does not exist: " + cfg.Path}
	}
	if cfg.Circle == "" {
		return SpawnConfig{}, &SpawnError{Status: 422, Detail: "Circle is required"}
	}
	if cfg.Role == "" {
		cfg.Role = proto.RoleAgent
	}
	placement, err := s.container.Reserve(cfg)
	if err != nil {
		return SpawnConfig{}, &SpawnError{Status: 500, Detail: err.Error()}
	}
	cfg.Reserved = &placement
	return s.withEnv(cfg), nil
}

// Launch starts cfg.Command (in cfg.Reserved when set) and records durable
// pane ownership.
func (s *SpawnService) Launch(cfg SpawnConfig) (SpawnResult, error) {
	result, err := s.container.Launch(cfg)
	if err != nil {
		return SpawnResult{}, &SpawnError{Status: 500, Detail: err.Error()}
	}

	if result.PaneID != "" {
		s.own.MarkSpawned(result.PaneID)
		var peerID *string
		if cfg.PeerID != nil {
			id := string(*cfg.PeerID)
			peerID = &id
		}
		s.own.Record(OwnershipRecord{
			PaneID:      result.PaneID,
			Path:        cfg.Path,
			Backend:     string(cfg.Backend),
			Circle:      cfg.Circle,
			Role:        string(cfg.Role),
			DisplayName: result.DisplayName,
			TmuxSession: result.TmuxSession,
			PeerID:      peerID,
			Profile:     cfg.Profile,
		})
	}
	result.Circle = cfg.Circle
	return result, nil
}

// ---------------------------------------------------------------------------
// write_hint — the codex circle-discovery bridge (port of spawn_hints.write_hint).
// ---------------------------------------------------------------------------

// hintTTLSeconds mirrors spawn_hints.HINT_TTL_SECONDS: hints older than this are
// garbage. Generous slack for slow hosts and codex's late SessionStart.
const hintTTLSeconds = 300

// cacheDir resolves the spawn-hints cache root. Honors $REPOWIRE_CACHE_DIR for
// tests; else ~/.cache/repowire, matching repowire.config.models.CACHE_DIR.
func cacheDir() string {
	if dir := os.Getenv("REPOWIRE_CACHE_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".cache/repowire"
	}
	return filepath.Join(home, ".cache", "repowire")
}

// hintPath returns the sha256-keyed hint file for (path, backend). Key derivation
// matches _hint_key: sha256("{resolved}::{backend}")[:16].
func hintPath(path, backend string) string {
	key := sha256Hex16(fmt.Sprintf("%s::%s", NormPath(path), backend))
	return filepath.Join(cacheDir(), "spawn-hints", key+".json")
}

// writeHint appends a spawn-intent payload so a peer registering from this
// path+backend can recover its requested circle/role/peer_id (codex strips TMUX
// env). Queue-append semantics match write_hint; best-effort (a write failure is
// swallowed — the hint is a discovery convenience, not load-bearing identity).
func writeHint(path, backend, circle, paneID string, role *string, peerID *proto.PeerID, pendingFirstTurn bool) {
	payload := map[string]any{
		"path":    NormPath(path),
		"backend": backend,
		"circle":  circle,
		"ts":      float64(time.Now().UnixNano()) / 1e9,
	}
	if paneID != "" {
		payload["pane_id"] = paneID
	}
	if role != nil && *role != "" {
		payload["role"] = *role
	}
	if peerID != nil && *peerID != "" {
		payload["peer_id"] = string(*peerID)
	}
	if pendingFirstTurn {
		payload["pending_first_turn"] = true
	}
	target := hintPath(path, backend)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return
	}
	queue := readHintQueue(target)
	queue = append(queue, payload)
	blob, err := json.Marshal(queue)
	if err != nil {
		return
	}
	_ = os.WriteFile(target, blob, 0o644)
}

// readHintQueue returns queued hint payloads, accepting the legacy single-dict
// format (mirrors _read_hint_queue). A corrupt file is treated as empty.
func readHintQueue(target string) []map[string]any {
	raw, err := os.ReadFile(target)
	if err != nil {
		return nil
	}
	var asList []map[string]any
	if json.Unmarshal(raw, &asList) == nil {
		return asList
	}
	var asDict map[string]any
	if json.Unmarshal(raw, &asDict) == nil {
		return []map[string]any{asDict}
	}
	return nil
}

// commandWithEnv ports _command_with_env: prefix the command with explicit env
// assignments. The daemon captures the login-shell PATH when no explicit PATH
// is configured, so launchd-spawned agents see the same executables as a shell.
func commandWithEnv(command string, env map[string]string) string {
	if len(env) == 0 {
		return command
	}
	var assignments []string
	for _, key := range slices.Sorted(maps.Keys(env)) {
		if key == "" || env[key] == "" {
			continue
		}
		assignments = append(assignments, fmt.Sprintf("%s=%s", key, ShellQuote(env[key])))
	}
	if len(assignments) == 0 {
		return command
	}
	return "env " + strings.Join(assignments, " ") + " " + command
}

// ShellQuote quotes a value for a shell command line only when needed.
func ShellQuote(v string) string {
	if v != "" && strings.IndexFunc(v, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '@' || r == '%')
	}) == -1 {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}
