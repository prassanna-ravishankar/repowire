package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
	Telegram    TelegramConfig    `yaml:"telegram"`
	Slack       SlackConfig       `yaml:"slack"`
	Experiments ExperimentsConfig `yaml:"experiments"`
}

type DaemonConfig struct {
	Host                    string                   `yaml:"host"`
	Port                    int                      `yaml:"port"`
	AuthToken               string                   `yaml:"auth_token"`
	HeartbeatInterval       int                      `yaml:"heartbeat_interval"`
	PruneMaxAgeHours        float64                  `yaml:"prune_max_age_hours"`
	DescriptionTTLSeconds   float64                  `yaml:"description_ttl_seconds"`
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

type TelegramConfig struct {
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"`
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
			CircleBoundary:   proto.CircleBoundarySession,
			PruneMaxAgeHours: 24, DescriptionTTLSeconds: 900,
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
		Relay: RelayConfig{URL: defaultRelayURL},
		Experiments: ExperimentsConfig{RemoteToolApproval: RemoteToolApprovalConfig{
			GatedTools:     []string{"Bash", "Edit", "Write", "MultiEdit", "NotebookEdit"},
			TimeoutSeconds: 45,
		}},
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
			"claude": "claude-code", "codex": "codex", "gemini": "gemini",
			"opencode": "opencode", "agy": "antigravity", "pi": "pi",
		}[fields[0]]
		if backend != "" {
			if _, exists := out[backend]; !exists {
				out[backend] = command
			}
		}
	}
	return out
}

func Load() (Config, error) {
	cfg := Defaults()
	path := Path()
	if b, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if len(cfg.Daemon.Spawn.Commands) == 0 {
		cfg.Daemon.Spawn.Commands = legacySpawnCommands(cfg.Daemon.Spawn.AllowedCommands)
	}
	applyEnv(&cfg)
	normalize(&cfg)
	if !cfg.Daemon.CircleBoundary.Valid() {
		return cfg, fmt.Errorf("daemon.circle_boundary must be session or window, got %q", cfg.Daemon.CircleBoundary)
	}
	return cfg, nil
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
	setFloatEnv(&cfg.Daemon.DescriptionTTLSeconds, "REPOWIRE_DAEMON__DESCRIPTION_TTL_SECONDS")
	setFloatEnv(&cfg.Daemon.PeerReapTTLSeconds, "REPOWIRE_DAEMON__PEER_REAP_TTL_SECONDS")
	setFloatEnv(&cfg.Daemon.StaleBusyTimeoutSeconds, "REPOWIRE_DAEMON__STALE_BUSY_TIMEOUT_SECONDS")
	setFloatEnv(&cfg.Daemon.DeliveryQueueTTLSeconds, "REPOWIRE_DAEMON__DELIVERY_QUEUE_TTL_SECONDS")
	setIntEnv(&cfg.Daemon.DeliveryQueueMaxPerPeer, "REPOWIRE_DAEMON__DELIVERY_QUEUE_MAX_PER_PEER")
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
	setBoolEnv(&cfg.Experiments.ACPBrokerClient, "REPOWIRE_EXPERIMENTS__ACP_BROKER_CLIENT")
	setBoolEnv(&cfg.Experiments.ChatTurnStreaming, "REPOWIRE_EXPERIMENTS__CHAT_TURN_STREAMING")
	setBoolEnv(&cfg.Experiments.RemoteToolApproval.Enabled, "REPOWIRE_EXPERIMENTS__REMOTE_TOOL_APPROVAL__ENABLED")
	setFloatEnv(&cfg.Experiments.RemoteToolApproval.TimeoutSeconds, "REPOWIRE_EXPERIMENTS__REMOTE_TOOL_APPROVAL__TIMEOUT_SECONDS")
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
	if len(cfg.Experiments.RemoteToolApproval.GatedTools) == 0 {
		cfg.Experiments.RemoteToolApproval.GatedTools = []string{"Bash", "Edit", "Write", "MultiEdit", "NotebookEdit"}
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
