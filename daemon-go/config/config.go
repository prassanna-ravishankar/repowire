package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"gopkg.in/yaml.v3"
)

const (
	defaultHost     = "127.0.0.1"
	defaultPort     = 8377
	defaultRelayURL = "wss://repowire.io"
)

type Config struct {
	Daemon      DaemonConfig      `yaml:"daemon"`
	Relay       RelayConfig       `yaml:"relay"`
	Hosted      HostedConfig      `yaml:"hosted"`
	Telegram    TelegramConfig    `yaml:"telegram"`
	Slack       SlackConfig       `yaml:"slack"`
	Human       HumanConfig       `yaml:"human"`
	Experiments ExperimentsConfig `yaml:"experiments"`
	Retention   RetentionConfig   `yaml:"retention"`
	Wire        WireConfig        `yaml:"wire"`
	// Routing is how asks find their target; see RouterConfig. It is decoded
	// on its own (loadRouting), so a mistyped routing key can never fail
	// the load.
	Routing RouterConfig `yaml:"-"`
	// Warnings are load-time notices the daemon logs once at startup:
	// settings that were migrated or dropped rather than refused.
	Warnings []string `yaml:"-"`
	// strategyUnusable: routing.strategy in the file did not decode. It is
	// settled after env vars apply, since REPOWIRE_ROUTING__STRATEGY can
	// repair it and REPOWIRE_ROUTING__SHADOW_ARMS can add shadows back.
	strategyUnusable bool
}

// maxRetentionDays bounds a retention setting to what a time.Duration holds
// with room to spare (100 years; a Duration tops out near 292).
const maxRetentionDays = 36500

// WireConfig shapes the orchestrator's wire view
// (rfcs/wire-observability.md).
type WireConfig struct {
	Stuck WireStuckConfig `yaml:"stuck"`
}

// WireStuckConfig are the thresholds at which an open thread reads as stuck.
// Each is checked when the thread is read, never on a timer.
type WireStuckConfig struct {
	IgnoredTurns       int     `yaml:"ignored_turns"`       // holder turns since assignment with the ask still open
	SlowMinutes        float64 `yaml:"slow_minutes"`        // assignment age with no holder turn
	AskerGoneMinutes   float64 `yaml:"asker_gone_minutes"`  // reply stashed for an offline asker
	UndeliveredMinutes float64 `yaml:"undelivered_minutes"` // notify still queued
}

// RetentionConfig is how long the daemon keeps what it stores
// (rfcs/retention.md). Content is text a peer or the human wrote; records are
// the rows saying who did what and when. 0 keeps forever.
type RetentionConfig struct {
	ContentDays float64 `yaml:"content_days"`
	RecordsDays float64 `yaml:"records_days"`
}

// Durations is the policy as durations; zero keeps a class forever.
func (c RetentionConfig) Durations() (content, records time.Duration) {
	day := float64(24 * time.Hour)
	return time.Duration(c.ContentDays * day), time.Duration(c.RecordsDays * day)
}

type DaemonConfig struct {
	Host                    string                   `yaml:"host"`
	Port                    int                      `yaml:"port"`
	AuthToken               string                   `yaml:"auth_token"`
	HeartbeatInterval       int                      `yaml:"heartbeat_interval"`
	PruneMaxAgeHours        float64                  `yaml:"prune_max_age_hours"`
	PeerReapTTLSeconds      float64                  `yaml:"peer_reap_ttl_seconds"`
	StaleBusyTimeoutSeconds float64                  `yaml:"stale_busy_timeout_seconds"`
	DeliveryQueueTTLSeconds float64                  `yaml:"delivery_queue_ttl_seconds"`
	DeliveryQueueMaxPerPeer int                      `yaml:"delivery_queue_max_per_peer"`
	CircleBoundary          proto.CircleBoundary     `yaml:"circle_boundary"`
	OrchestratorRecall      OrchestratorRecallConfig `yaml:"orchestrator_recall"`
	Spawn                   SpawnConfig              `yaml:"spawn"`
	MCPHTTP                 MCPHTTPConfig            `yaml:"mcp_http"`
}

type SpawnConfig struct {
	Commands        map[string]string                  `yaml:"commands"`
	AllowedCommands []string                           `yaml:"allowed_commands"`
	AllowedPaths    []string                           `yaml:"allowed_paths"`
	Profiles        map[string]map[string]SpawnProfile `yaml:"profiles"`
	EnvPath         []string                           `yaml:"env_path"`
	Env             map[string]string                  `yaml:"env"`
}

type SpawnProfile struct {
	Args        []string `yaml:"args"`
	Description string   `yaml:"description"`
}

type MCPHTTPConfig struct {
	Enabled                       bool   `yaml:"enabled"`
	Bind                          string `yaml:"bind"`
	RequireAuth                   bool   `yaml:"require_auth"`
	AllowUnauthenticatedLocalhost bool   `yaml:"allow_unauthenticated_localhost"`
	AllowDangerousTools           bool   `yaml:"allow_dangerous_tools"`
}

type OrchestratorRecallConfig struct {
	Enabled      bool `yaml:"enabled"`
	MaxHits      int  `yaml:"max_hits"`
	MaxChars     int  `yaml:"max_chars"`
	MaxFileChars int  `yaml:"max_file_chars"`
}

type RelayConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`
	APIKey  string `yaml:"api_key"`
}

// HostedConfig points the daemon at a hosted runtime for managed agents.
// Both fields are required; either empty leaves hosted spawn disabled.
type HostedConfig struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

type TelegramConfig struct {
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"`
}

// HumanConfig shapes the daemon-owned human peer and its inbox. Telegram,
// the dashboard and the CLI render the inbox; senders address the human.
type HumanConfig struct {
	// Name is the human peer's display_name (default "human").
	Name string `yaml:"name"`
	// Broadcast stores mesh broadcasts in the inbox. Off by default so a
	// mesh-wide announcement does not page anyone.
	Broadcast bool `yaml:"broadcast"`
	// Surfaces holds per-surface notification policy, keyed by surface name
	// (telegram, slack).
	Surfaces map[string]HumanSurfaceConfig `yaml:"surfaces"`
}

// HumanSurfaceConfig is one surface's notification policy.
type HumanSurfaceConfig struct {
	// Broadcast pushes stored broadcasts to this surface (default off).
	Broadcast bool `yaml:"broadcast"`
}

// PushesBroadcast reports whether surface opted in to broadcast pushes.
func (h HumanConfig) PushesBroadcast(surface string) bool {
	return h.Surfaces[surface].Broadcast
}

type SlackConfig struct {
	BotToken  string `yaml:"bot_token"`
	AppToken  string `yaml:"app_token"`
	ChannelID string `yaml:"channel_id"`
}

type ExperimentsConfig struct {
	ACPBrokerClient    bool                     `yaml:"acp_broker_client"`
	ChatTurnStreaming  bool                     `yaml:"chat_turn_streaming"`
	RemoteToolApproval RemoteToolApprovalConfig `yaml:"remote_tool_approval"`
	// GitTrailers enables the repository prepare-commit-msg hook installed by
	// setup --git-hooks to add Repowire-Thread / Repowire-Session trailers.
	GitTrailers bool `yaml:"git_trailers"`
}

type RemoteToolApprovalConfig struct {
	Enabled        bool     `yaml:"enabled"`
	GatedTools     []string `yaml:"gated_tools"`
	TimeoutSeconds float64  `yaml:"timeout_seconds"`
}

func Defaults() Config {
	return Config{
		Daemon: DaemonConfig{
			Host: defaultHost, Port: defaultPort, HeartbeatInterval: 30,
			CircleBoundary:     proto.CircleBoundarySession,
			PruneMaxAgeHours:   24,
			PeerReapTTLSeconds: 600, StaleBusyTimeoutSeconds: 1800,
			DeliveryQueueTTLSeconds: 86400, DeliveryQueueMaxPerPeer: 100,
			OrchestratorRecall: OrchestratorRecallConfig{Enabled: true, MaxHits: 3, MaxChars: 900, MaxFileChars: 12000},
			Spawn: SpawnConfig{
				Commands: map[string]string{},
			},
			MCPHTTP: MCPHTTPConfig{
				Bind:        "localhost-only",
				RequireAuth: true,
			},
		},
		Relay:     RelayConfig{URL: defaultRelayURL},
		Retention: RetentionConfig{ContentDays: 7, RecordsDays: 180},
		Wire:      WireConfig{Stuck: WireStuckConfig{IgnoredTurns: 3, SlowMinutes: 30, AskerGoneMinutes: 60, UndeliveredMinutes: 10}},
		Human:     HumanConfig{Name: "human"},
		Experiments: ExperimentsConfig{RemoteToolApproval: RemoteToolApprovalConfig{
			GatedTools:     []string{"Bash", "Edit", "Write", "MultiEdit", "NotebookEdit"},
			TimeoutSeconds: 45,
		}, GitTrailers: true},
		Routing: defaultRouting(),
	}
}

// legacySpawnCommands preserves the one-release allowed_commands migration.
func legacySpawnCommands(commands []string) map[string]string {
	out := map[string]string{}
	for _, command := range commands {
		fields := strings.Fields(command)
		if len(fields) == 0 {
			continue
		}
		backend := map[string]string{
			"claude": "claude-code", "codex": "codex",
			"opencode": "opencode", "pi": "pi",
		}[fields[0]]
		if backend != "" {
			if _, exists := out[backend]; !exists {
				out[backend] = command
			}
		}
	}
	return out
}

func Load() (Config, error) { return LoadFile(Path()) }

// LoadFile loads the config at path with the daemon's rules (warnings,
// credential blocks all or nothing, environment overrides). A missing file
// yields the defaults.
func LoadFile(path string) (Config, error) {
	cfg := Defaults()
	var raw []byte
	if b, err := os.ReadFile(path); err == nil {
		if err := decodeFile(b, &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
		raw = b
	} else if !os.IsNotExist(err) {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if len(cfg.Daemon.Spawn.Commands) == 0 {
		cfg.Daemon.Spawn.Commands = legacySpawnCommands(cfg.Daemon.Spawn.AllowedCommands)
	}
	if err := loadRouting(&cfg, raw); err != nil {
		return cfg, err
	}
	applyEnv(&cfg)
	normalize(&cfg)
	if !cfg.Daemon.CircleBoundary.Valid() {
		return cfg, fmt.Errorf("daemon.circle_boundary must be session or window, got %q", cfg.Daemon.CircleBoundary)
	}
	if err := cfg.settleRouting(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// rejected is the diagnostic for a setting whose value does not decode. It
// says where, never what: yaml's own error quotes the value, which may be a
// credential, and diagnostics are logged at startup and printed by
// `repowire config check`. line is 0 when unknown.
func rejected(path string, line int) string {
	if line > 0 {
		return fmt.Sprintf("config: %s is ignored: the value on line %d is not valid for this setting", path, line)
	}
	return fmt.Sprintf("config: %s is ignored: its value is not valid for this setting", path)
}

// coreDaemonKeys are the settings the daemon cannot run correctly without: a
// mistyped one fails the load rather than starting on a silently different
// address, without its auth token, or with another circle boundary.
var coreDaemonKeys = []string{"host", "port", "auth_token", "circle_boundary"}

// decodeFile reads the file into cfg one section and one key at a time (#427).
// A syntax error, an unreadable daemon block, or a mistyped core daemon key is
// an error. Any other key that does not decode keeps its default and adds a
// warning, so a typo in an optional feature never stops the daemon (under
// launchd KeepAlive, a failed load is a crash loop). A nested block drops only
// its mistyped keys. routing: and experiments.router are decoded separately
// by loadRouting.
func decodeFile(raw []byte, cfg *Config) error {
	var file struct {
		Daemon      yaml.Node `yaml:"daemon"`
		Relay       yaml.Node `yaml:"relay"`
		Telegram    yaml.Node `yaml:"telegram"`
		Slack       yaml.Node `yaml:"slack"`
		Human       yaml.Node `yaml:"human"`
		Experiments yaml.Node `yaml:"experiments"`
		Retention   yaml.Node `yaml:"retention"`
		Wire        yaml.Node `yaml:"wire"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return err
	}
	warn := func(warnings []string) { cfg.Warnings = append(cfg.Warnings, warnings...) }

	warnings, failed := decodeSection(&file.Daemon, &cfg.Daemon, "daemon")
	for i, key := range failed {
		if key == "" || slices.Contains(coreDaemonKeys, key) {
			return fmt.Errorf("%s", strings.TrimPrefix(warnings[i], "config: "))
		}
	}
	warn(nested(&file.Daemon, warnings, failed, map[string]func(*yaml.Node, string) []string{
		"spawn":    func(n *yaml.Node, p string) []string { w, _ := decodeSection(n, &cfg.Daemon.Spawn, p); return w },
		"mcp_http": func(n *yaml.Node, p string) []string { w, _ := decodeSection(n, &cfg.Daemon.MCPHTTP, p); return w },
		"orchestrator_recall": func(n *yaml.Node, p string) []string {
			w, _ := decodeSection(n, &cfg.Daemon.OrchestratorRecall, p)
			return w
		},
	}, "daemon"))
	warnings, failed = decodeSection(&file.Experiments, &cfg.Experiments, "experiments")
	warn(nested(&file.Experiments, warnings, failed, map[string]func(*yaml.Node, string) []string{
		"remote_tool_approval": func(n *yaml.Node, p string) []string {
			w, f := decodeSection(n, &cfg.Experiments.RemoteToolApproval, p)
			// A security control fails closed: when whether approval is on
			// cannot be read, it is on (with the default gated tools).
			if slices.Contains(f, "") || slices.Contains(f, "enabled") {
				cfg.Experiments.RemoteToolApproval.Enabled = true
				w = append(w, "config: experiments.remote_tool_approval is enabled because its setting could not be read")
			}
			return w
		},
	}, "experiments"))
	// Blocks that carry credentials are read whole or not at all: a relay
	// whose url did not decode must not send its key to the default relay,
	// and a bridge with a token but no readable channel stays off.
	defaults := Defaults()
	warn(allOrNothing(&file.Relay, &cfg.Relay, defaults.Relay, "relay"))
	warn(allOrNothing(&file.Telegram, &cfg.Telegram, defaults.Telegram, "telegram"))
	warn(allOrNothing(&file.Slack, &cfg.Slack, defaults.Slack, "slack"))
	w, _ := decodeSection(&file.Human, &cfg.Human, "human")
	warn(w)
	w, _ = decodeSection(&file.Retention, &cfg.Retention, "retention")
	warn(w)
	w, _ = decodeSection(&file.Wire, &cfg.Wire, "wire")
	warn(w)
	return nil
}

// allOrNothing decodes a section like decodeSection, but when any key in it
// fails the whole section reverts to def, so no credential is used with a
// default substituted for a sibling setting.
func allOrNothing[T any](node *yaml.Node, dst *T, def T, path string) []string {
	warnings, failed := decodeSection(node, dst, path)
	if len(failed) > 0 {
		*dst = def
		warnings = append(warnings, fmt.Sprintf("config: %s is disabled because part of it could not be read", path))
	}
	return warnings
}

// decodeSection decodes a section that may be absent into dst, whole when it
// can and otherwise key by key, keeping each key that decodes. It works on
// the file's own nodes, so a value keeps exactly what it decoded to before
// (a string field holding 012345 stays "012345"). failed names each dropped
// key, aligned with warnings; "" means the section was not a mapping.
func decodeSection[T any](node *yaml.Node, dst *T, path string) (warnings, failed []string) {
	n := node
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.Tag == "!!null") {
		return nil, nil
	}
	if n.Kind != yaml.MappingNode {
		return []string{fmt.Sprintf("config: %s must be a mapping; ignored", path)}, []string{""}
	}
	if whole, err := clone(*dst); err == nil && n.Decode(&whole) == nil {
		*dst = whole
		return nil, nil
	}
	pairs := flattenMerges(n)
	for i := 0; i+1 < len(pairs); i += 2 {
		key, value := pairs[i], pairs[i+1]
		one := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{key, value}}
		trial, err := clone(*dst)
		if err == nil {
			err = one.Decode(&trial)
		}
		if err != nil {
			warnings = append(warnings, rejected(path+"."+key.Value, value.Line))
			failed = append(failed, key.Value)
			continue
		}
		*dst = trial
	}
	return warnings, failed
}

// nested replaces the warning for a failed key that is itself a block with
// that block decoded key by key, so only its mistyped keys are dropped.
// warnings and failed are decodeKeys' aligned results for parent.
func nested(parent *yaml.Node, warnings, failed []string, blocks map[string]func(*yaml.Node, string) []string, path string) []string {
	var out []string
	for i, key := range failed {
		decode, ok := blocks[key]
		child := mappingValue(parent, key)
		if !ok || child == nil || key == "" {
			out = append(out, warnings[i])
			continue
		}
		out = append(out, decode(child, path+"."+key)...)
	}
	return out
}

// flattenMerges returns a mapping's key/value pairs with merge keys (<<)
// expanded in place of the merge, as decoding would see them: an explicit key
// beats a merged one, and an earlier merge beats a later one. Recovery then
// judges each merged key like one written out.
func flattenMerges(n *yaml.Node) []*yaml.Node {
	return flattenMergesOn(n, map[*yaml.Node]bool{})
}

// flattenMergesOn is flattenMerges with the mappings already on the path, so
// a merge that refers back into itself is skipped instead of followed forever.
func flattenMergesOn(n *yaml.Node, path map[*yaml.Node]bool) []*yaml.Node {
	path[n] = true
	defer delete(path, n)
	seen := map[string]bool{}
	var out, merged []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i]; k.Tag == "!!merge" || k.Value == "<<" {
			continue
		}
		seen[n.Content[i].Value] = true
		out = append(out, n.Content[i], n.Content[i+1])
	}
	var add func(*yaml.Node)
	add = func(m *yaml.Node) {
		for m != nil && m.Kind == yaml.AliasNode {
			m = m.Alias
		}
		switch {
		case m == nil || path[m]:
		case m.Kind == yaml.SequenceNode:
			path[m] = true
			for _, item := range m.Content {
				add(item)
			}
			delete(path, m)
		case m.Kind == yaml.MappingNode && !path[m]:
			pairs := flattenMergesOn(m, path)
			for i := 0; i+1 < len(pairs); i += 2 {
				if !seen[pairs[i].Value] {
					seen[pairs[i].Value] = true
					merged = append(merged, pairs[i], pairs[i+1])
				}
			}
		}
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i]; k.Tag == "!!merge" || k.Value == "<<" {
			add(n.Content[i+1])
		}
	}
	return append(out, merged...)
}

// mappingValue returns the value node for key in a mapping node, or nil. It
// sees through aliases and merge keys (an explicit key beats a merged one),
// as decoding does.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	return mappingValueOn(node, key, map[*yaml.Node]bool{})
}

// mergedValue looks key up in a merge value: a mapping, or a sequence of
// them, through aliases, skipping anything already on path.
func mergedValue(merge *yaml.Node, key string, path map[*yaml.Node]bool) *yaml.Node {
	for merge != nil && merge.Kind == yaml.AliasNode {
		merge = merge.Alias
	}
	if merge == nil || path[merge] {
		return nil
	}
	if merge.Kind != yaml.SequenceNode {
		return mappingValueOn(merge, key, path)
	}
	path[merge] = true
	defer delete(path, merge)
	for _, item := range merge.Content {
		if v := mergedValue(item, key, path); v != nil {
			return v
		}
	}
	return nil
}

func mappingValueOn(node *yaml.Node, key string, path map[*yaml.Node]bool) *yaml.Node {
	for node != nil && (node.Kind == yaml.AliasNode || (node.Kind == yaml.DocumentNode && len(node.Content) == 1)) {
		if node.Kind == yaml.AliasNode {
			node = node.Alias
		} else {
			node = node.Content[0]
		}
	}
	if node == nil || node.Kind != yaml.MappingNode || path[node] {
		return nil
	}
	path[node] = true
	defer delete(path, node)
	var merges []*yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch k := node.Content[i]; {
		case k.Value == key:
			return node.Content[i+1]
		case k.Tag == "!!merge" || k.Value == "<<":
			merges = append(merges, node.Content[i+1])
		}
	}
	for _, merge := range merges {
		if v := mergedValue(merge, key, path); v != nil {
			return v
		}
	}
	return nil
}

func Path() string {
	if p := os.Getenv("REPOWIRE_CONFIG"); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".repowire", "config.yaml")
	}
	return filepath.Join(".repowire", "config.yaml")
}

func (s SpawnConfig) CommandsJSON() string {
	if len(s.Commands) == 0 {
		return ""
	}
	b, _ := json.Marshal(s.Commands)
	return string(b)
}

func applyEnv(cfg *Config) {
	if v := firstEnv("REPOWIRE_DAEMON__HOST", "REPOWIRE_DAEMON_HOST"); v != "" {
		cfg.Daemon.Host = v
	}
	if v := firstEnv("REPOWIRE_DAEMON__PORT", "REPOWIRE_DAEMON_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Daemon.Port = n
		}
	}
	if v := firstEnv("REPOWIRE_DAEMON__AUTH_TOKEN", "REPOWIRE_AUTH_TOKEN"); v != "" {
		cfg.Daemon.AuthToken = v
	}
	setIntEnv(&cfg.Daemon.HeartbeatInterval, "REPOWIRE_DAEMON__HEARTBEAT_INTERVAL")
	setFloatEnv(&cfg.Daemon.PruneMaxAgeHours, "REPOWIRE_DAEMON__PRUNE_MAX_AGE_HOURS")
	setFloatEnv(&cfg.Daemon.PeerReapTTLSeconds, "REPOWIRE_DAEMON__PEER_REAP_TTL_SECONDS")
	setFloatEnv(&cfg.Daemon.StaleBusyTimeoutSeconds, "REPOWIRE_DAEMON__STALE_BUSY_TIMEOUT_SECONDS")
	setFloatEnv(&cfg.Daemon.DeliveryQueueTTLSeconds, "REPOWIRE_DAEMON__DELIVERY_QUEUE_TTL_SECONDS")
	setIntEnv(&cfg.Daemon.DeliveryQueueMaxPerPeer, "REPOWIRE_DAEMON__DELIVERY_QUEUE_MAX_PER_PEER")
	setFloatEnv(&cfg.Retention.ContentDays, "REPOWIRE_RETENTION__CONTENT_DAYS")
	setFloatEnv(&cfg.Retention.RecordsDays, "REPOWIRE_RETENTION__RECORDS_DAYS")
	if value := os.Getenv("REPOWIRE_DAEMON__CIRCLE_BOUNDARY"); value != "" {
		cfg.Daemon.CircleBoundary = proto.CircleBoundary(value)
	}
	setBoolEnv(&cfg.Daemon.MCPHTTP.Enabled, "REPOWIRE_DAEMON__MCP_HTTP__ENABLED")
	if v := os.Getenv("REPOWIRE_DAEMON__MCP_HTTP__BIND"); v != "" {
		cfg.Daemon.MCPHTTP.Bind = v
	}
	setBoolEnv(&cfg.Daemon.MCPHTTP.RequireAuth, "REPOWIRE_DAEMON__MCP_HTTP__REQUIRE_AUTH")
	setBoolEnv(&cfg.Daemon.MCPHTTP.AllowUnauthenticatedLocalhost, "REPOWIRE_DAEMON__MCP_HTTP__ALLOW_UNAUTHENTICATED_LOCALHOST")
	setBoolEnv(&cfg.Daemon.MCPHTTP.AllowDangerousTools, "REPOWIRE_DAEMON__MCP_HTTP__ALLOW_DANGEROUS_TOOLS")
	setBoolEnv(&cfg.Daemon.OrchestratorRecall.Enabled, "REPOWIRE_DAEMON__ORCHESTRATOR_RECALL__ENABLED")
	setIntEnv(&cfg.Daemon.OrchestratorRecall.MaxHits, "REPOWIRE_DAEMON__ORCHESTRATOR_RECALL__MAX_HITS")
	setIntEnv(&cfg.Daemon.OrchestratorRecall.MaxChars, "REPOWIRE_DAEMON__ORCHESTRATOR_RECALL__MAX_CHARS")
	setIntEnv(&cfg.Daemon.OrchestratorRecall.MaxFileChars, "REPOWIRE_DAEMON__ORCHESTRATOR_RECALL__MAX_FILE_CHARS")
	if v := os.Getenv("REPOWIRE_SPAWN_COMMANDS"); v != "" {
		var commands map[string]string
		if json.Unmarshal([]byte(v), &commands) == nil {
			cfg.Daemon.Spawn.Commands = commands
		}
	}
	if v := os.Getenv("REPOWIRE_SPAWN_ALLOWED_PATHS"); v != "" {
		cfg.Daemon.Spawn.AllowedPaths = SplitCSV(v)
	}
	if v := os.Getenv("REPOWIRE_HOSTED__URL"); v != "" {
		cfg.Hosted.URL = v
	}
	if v := os.Getenv("REPOWIRE_HOSTED__TOKEN"); v != "" {
		cfg.Hosted.Token = v
	}
	if v := firstEnv("REPOWIRE_RELAY_URL", "REPOWIRE_RELAY__URL"); v != "" {
		cfg.Relay.URL = v
	}
	setBoolEnv(&cfg.Relay.Enabled, "REPOWIRE_RELAY__ENABLED")
	if v := firstEnv("REPOWIRE_API_KEY", "REPOWIRE_RELAY__API_KEY", "REPOWIRE_RELAY_API_KEY"); v != "" {
		cfg.Relay.APIKey = v
		cfg.Relay.Enabled = true
	}
	if v := firstEnv("TELEGRAM_BOT_TOKEN", "REPOWIRE_TELEGRAM__BOT_TOKEN"); v != "" {
		cfg.Telegram.BotToken = v
	}
	if v := firstEnv("TELEGRAM_CHAT_ID", "REPOWIRE_TELEGRAM__CHAT_ID"); v != "" {
		cfg.Telegram.ChatID = v
	}
	if v := firstEnv("SLACK_BOT_TOKEN", "REPOWIRE_SLACK__BOT_TOKEN"); v != "" {
		cfg.Slack.BotToken = v
	}
	if v := firstEnv("SLACK_APP_TOKEN", "REPOWIRE_SLACK__APP_TOKEN"); v != "" {
		cfg.Slack.AppToken = v
	}
	if v := firstEnv("SLACK_CHANNEL_ID", "REPOWIRE_SLACK__CHANNEL_ID"); v != "" {
		cfg.Slack.ChannelID = v
	}
	setStringEnv(&cfg.Human.Name, "REPOWIRE_HUMAN__NAME")
	setBoolEnv(&cfg.Human.Broadcast, "REPOWIRE_HUMAN__BROADCAST")
	setBoolEnv(&cfg.Experiments.ACPBrokerClient, "REPOWIRE_EXPERIMENTS__ACP_BROKER_CLIENT")
	setBoolEnv(&cfg.Experiments.ChatTurnStreaming, "REPOWIRE_EXPERIMENTS__CHAT_TURN_STREAMING")
	setBoolEnv(&cfg.Experiments.GitTrailers, "REPOWIRE_EXPERIMENTS__GIT_TRAILERS")
	setBoolEnv(&cfg.Experiments.RemoteToolApproval.Enabled, "REPOWIRE_EXPERIMENTS__REMOTE_TOOL_APPROVAL__ENABLED")
	setFloatEnv(&cfg.Experiments.RemoteToolApproval.TimeoutSeconds, "REPOWIRE_EXPERIMENTS__REMOTE_TOOL_APPROVAL__TIMEOUT_SECONDS")
	applyRoutingEnv(cfg)
	if v := os.Getenv("REPOWIRE_EXPERIMENTS__REMOTE_TOOL_APPROVAL__GATED_TOOLS"); v != "" {
		var tools []string
		if json.Unmarshal([]byte(v), &tools) == nil {
			cfg.Experiments.RemoteToolApproval.GatedTools = tools
		} else {
			cfg.Experiments.RemoteToolApproval.GatedTools = SplitCSV(v)
		}
	}
}

func normalize(cfg *Config) {
	if cfg.Daemon.Host == "" {
		cfg.Daemon.Host = defaultHost
	}
	if cfg.Daemon.Port == 0 {
		cfg.Daemon.Port = defaultPort
	}
	if cfg.Daemon.CircleBoundary == "" {
		cfg.Daemon.CircleBoundary = proto.CircleBoundarySession
	}
	if cfg.Daemon.Spawn.Commands == nil {
		cfg.Daemon.Spawn.Commands = map[string]string{}
	}
	if cfg.Daemon.Spawn.Profiles == nil {
		cfg.Daemon.Spawn.Profiles = map[string]map[string]SpawnProfile{}
	}
	if cfg.Daemon.Spawn.Env == nil {
		cfg.Daemon.Spawn.Env = map[string]string{}
	}
	if cfg.Daemon.MCPHTTP.Bind == "" {
		cfg.Daemon.MCPHTTP.Bind = "localhost-only"
	}
	if cfg.Relay.URL == "" {
		cfg.Relay.URL = defaultRelayURL
	}
	defaults := Defaults().Retention
	for _, days := range []struct {
		value    *float64
		fallback float64
		key      string
	}{{&cfg.Retention.ContentDays, defaults.ContentDays, "content_days"}, {&cfg.Retention.RecordsDays, defaults.RecordsDays, "records_days"}} {
		if v := *days.value; !(v >= 0 && v <= maxRetentionDays) {
			*days.value = days.fallback
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("config: retention.%s is ignored: it must be 0 (keep forever) or a number of days up to %d", days.key, maxRetentionDays))
		}
		// The sweep never expires anything younger than an ask may stay open
		// (state.RetentionPolicy.Effective); say so rather than raise it silently.
		askHours := cfg.Daemon.PruneMaxAgeHours
		if !(askHours > 0) {
			askHours = 24 // the ask tracker's own default
		}
		if v := *days.value; v > 0 && v*24 < askHours {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("config: retention.%s (%g days) is shorter than the ask lifetime (daemon.prune_max_age_hours, %g hours): it acts as %g days so open asks keep their data", days.key, v, askHours, askHours/24))
		}
	}
	stuck, stuckDefaults := &cfg.Wire.Stuck, Defaults().Wire.Stuck
	if stuck.IgnoredTurns <= 0 {
		stuck.IgnoredTurns = stuckDefaults.IgnoredTurns
	}
	for _, m := range []struct {
		value    *float64
		fallback float64
	}{{&stuck.SlowMinutes, stuckDefaults.SlowMinutes}, {&stuck.AskerGoneMinutes, stuckDefaults.AskerGoneMinutes}, {&stuck.UndeliveredMinutes, stuckDefaults.UndeliveredMinutes}} {
		if !(*m.value > 0) {
			*m.value = m.fallback
		}
	}
	if len(cfg.Experiments.RemoteToolApproval.GatedTools) == 0 {
		cfg.Experiments.RemoteToolApproval.GatedTools = []string{"Bash", "Edit", "Write", "MultiEdit", "NotebookEdit"}
	}
	if cfg.Routing.Strategy == "" {
		cfg.Routing.Strategy = StrategyStatic
	}
	if cfg.Routing.TimeoutSeconds <= 0 {
		cfg.Routing.TimeoutSeconds = 8
	}
	if cfg.Experiments.RemoteToolApproval.TimeoutSeconds <= 0 {
		cfg.Experiments.RemoteToolApproval.TimeoutSeconds = 45
	}
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return ""
}

func setIntEnv(target *int, key string) {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			*target = parsed
		}
	}
}
func setFloatEnv(target *float64, key string) {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			*target = parsed
		}
	}
}

func setStringEnv(target *string, key string) {
	if value := os.Getenv(key); value != "" {
		*target = value
	}
}

func setBoolEnv(target *bool, key string) {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			*target = parsed
		}
	}
}

// SplitCSV parses comma-separated config and flag values.
func SplitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
