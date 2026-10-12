package config

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// RouterConfig is the routing block: how an ask finds its target, and what
// is recorded about each decision.
//
// Strategy "static" (the default) delivers every ask to the peer it names and
// refuses an ask that names none. Any other strategy is the arm that chooses
// a target for an ask that names none; a named target is never overridden.
// ShadowArms run beside named asks for the research record, which requires
// Record.
type RouterConfig struct {
	Strategy string `yaml:"strategy"`
	// Record writes a route_decision trace row for every ask.
	Record bool `yaml:"record"`
	// RecordText keeps the ask body in those rows; off keeps only its hash.
	RecordText bool     `yaml:"record_text"`
	ShadowArms []string `yaml:"shadow_arms"`
	// SampleEvery[arm] = N runs that shadow arm on every Nth ask (default 1),
	// so paid arms need not run on every message.
	SampleEvery    map[string]int `yaml:"sample_every"`
	TimeoutSeconds float64        `yaml:"timeout_seconds"`
	// AllowServiceTargets lets a strategy deliver a targetless ask to a
	// service- or human-role peer (a phone bot, a person). Off by default.
	// Named asks are unaffected.
	AllowServiceTargets bool        `yaml:"allow_service_targets"`
	Jev                 RouterModel `yaml:"jev"`
	Frontier            RouterModel `yaml:"frontier"`
}

// RouterModel is one model-backed arm's endpoint.
type RouterModel struct {
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
	BaseURL string `yaml:"base_url"`
}

// StrategyStatic is the default routing strategy.
const StrategyStatic = "static"

// Legacy experiments.router modes, kept for the in-memory migration and for
// the mode recorded on each decision.
const (
	RouterModeOff    = "off"
	RouterModeShadow = "shadow"
	RouterModeLive   = "live"
)

func defaultRouting() RouterConfig {
	return RouterConfig{
		Strategy: StrategyStatic, RecordText: true, TimeoutSeconds: 8,
		Jev:      RouterModel{Model: "jev-1.13.0", BaseURL: "https://api.typesafe.ai"},
		Frontier: RouterModel{Model: "claude-opus-5-5"},
	}
}

// legacyRouter is the experiments.router block routing graduated from.
type legacyRouter struct {
	RouterConfig `yaml:",inline"`
	Mode         string `yaml:"mode"`
	Arm          string `yaml:"arm"`
}

const (
	legacyEnvPrefix  = "REPOWIRE_EXPERIMENTS__ROUTER__"
	routingEnvPrefix = "REPOWIRE_ROUTING__"
)

// routerEnvKeys are the per-field routing env vars, after their prefix.
var routerEnvKeys = []string{"RECORD", "RECORD_TEXT", "SHADOW_ARMS", "TIMEOUT_SECONDS", "ALLOW_SERVICE_TARGETS", "JEV__API_KEY", "JEV__MODEL", "JEV__BASE_URL", "FRONTIER__API_KEY", "FRONTIER__MODEL", "FRONTIER__BASE_URL"}

// The rule for this block: an optional feature's configuration must never
// stop the daemon from starting. Legacy keys are migrated in memory with a
// warning, and a setting that cannot take effect is dropped with a warning.
// Nothing here refuses to load.

// loadRouting decodes the routing: block, or migrates a legacy
// experiments.router block and REPOWIRE_EXPERIMENTS__ROUTER__* env vars
// onto it. Both blocks are decoded key by key, so a mistyped key is dropped
// with a warning and never fails the load. The file is never written. A
// routing: block wins over a legacy block.
func loadRouting(cfg *Config, raw []byte) error {
	var probe struct {
		Routing     yaml.Node `yaml:"routing"`
		Experiments struct {
			Router yaml.Node `yaml:"router"`
		} `yaml:"experiments"`
	}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &probe); err != nil {
			return err
		}
	}
	hasRouting, hasLegacy := probe.Routing.Kind != 0, probe.Experiments.Router.Kind != 0
	legacyEnv := envSet(legacyEnvPrefix, append([]string{"MODE", "ARM"}, routerEnvKeys...))
	if hasRouting {
		warnings, failed := decodeKeys(&probe.Routing, &cfg.Routing, "routing")
		cfg.Warnings = append(cfg.Warnings, warnings...)
		cfg.strategyUnusable = slices.Contains(failed, "strategy")
		if hasLegacy {
			cfg.Warnings = append(cfg.Warnings, "config: experiments.router is ignored because routing: is set; remove the legacy block")
		}
		for _, key := range legacyEnv {
			if key == legacyEnvPrefix+"MODE" || key == legacyEnvPrefix+"ARM" {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("config: %s is ignored because routing: is set; use routing.strategy", key))
				continue
			}
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("config: %s is deprecated; use %s", key, routingEnvPrefix+strings.TrimPrefix(key, legacyEnvPrefix)))
		}
		applyRouterEnv(&cfg.Routing, legacyEnvPrefix)
		return nil
	}
	if !hasLegacy && len(legacyEnv) == 0 {
		return nil
	}
	legacy := legacyRouter{RouterConfig: defaultRouting(), Mode: RouterModeOff, Arm: "explicit"}
	if hasLegacy {
		warnings, _ := decodeKeys(&probe.Experiments.Router, &legacy, "experiments.router")
		cfg.Warnings = append(cfg.Warnings, warnings...)
	}
	applyRouterEnv(&legacy.RouterConfig, legacyEnvPrefix)
	setStringEnv(&legacy.Mode, legacyEnvPrefix+"MODE")
	setStringEnv(&legacy.Arm, legacyEnvPrefix+"ARM")
	migrated, err := legacy.migrate()
	if err != nil {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("config: %v; migrated as mode off (static, no shadows)", err))
	}
	cfg.Routing = migrated
	sources := legacyEnv
	if hasLegacy {
		sources = append([]string{"experiments.router"}, sources...)
	}
	cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
		"config: %s is deprecated and was migrated in memory for this run; the file was not changed. Equivalent settings (env vars use the %s prefix):\n%s",
		strings.Join(sources, ", "), routingEnvPrefix, migrated.redactedYAML()))
	return nil
}

// decodeKeys decodes a mapping node into dst. Aliases and merge keys are
// resolved first by YAML's own generic decode (an explicit key wins over a
// merged one), so recovery never changes what the block means. If the whole
// block decodes, it is taken as is. Otherwise it is decoded one key at a
// time: a key whose value does not decode is skipped with a warning naming
// path.key, and dst keeps that setting's previous value.
func decodeKeys[T any](node *yaml.Node, dst *T, path string) (warnings, failed []string) {
	var generic any
	if err := node.Decode(&generic); err != nil {
		return []string{fmt.Sprintf("config: %s is ignored because it cannot be read: %v", path, err)}, []string{""}
	}
	if generic == nil {
		return nil, nil
	}
	resolved, err := plainNode(generic)
	if err != nil {
		return []string{fmt.Sprintf("config: %s is ignored because it cannot be read: %v", path, err)}, []string{""}
	}
	if resolved.Kind != yaml.MappingNode {
		return []string{fmt.Sprintf("config: %s must be a mapping; ignored", path)}, []string{""}
	}
	if whole, err := clone(*dst); err == nil && resolved.Decode(&whole) == nil {
		*dst = whole
		return nil, nil
	}
	for i := 0; i+1 < len(resolved.Content); i += 2 {
		key, value := resolved.Content[i], resolved.Content[i+1]
		one := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{key, value}}
		trial, err := clone(*dst)
		if err == nil {
			err = one.Decode(&trial)
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("config: %s.%s is ignored: %v", path, key.Value, err))
			failed = append(failed, key.Value)
			continue
		}
		*dst = trial
	}
	return warnings, failed
}

// plainNode re-encodes a generically decoded value as a node with no
// aliases or merge keys left in it.
func plainNode(v any) (*yaml.Node, error) {
	raw, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		return doc.Content[0], nil
	}
	return &doc, nil
}

// StaticFallbackDropsShadows is the warning for the one recovery that
// reaches past its own key: when the strategy is unusable, routing falls
// back to static and the shadow arms are dropped too, so a rejected config
// never spends provider calls on a counterfactual nobody asked for.
// Recording is unaffected.
func StaticFallbackDropsShadows(arms []string) string {
	return fmt.Sprintf("config: routing.shadow_arms %v are not run because routing.strategy is unusable and routing fell back to static", arms)
}

// clone deep-copies v through YAML, so a failed decode cannot leave a
// partly filled map or slice behind in the original.
func clone[T any](v T) (T, error) {
	var out T
	raw, err := yaml.Marshal(v)
	if err != nil {
		return out, err
	}
	err = yaml.Unmarshal(raw, &out)
	return out, err
}

// migrate maps the legacy mode onto a strategy, keeping exactly what ran:
// off ran no arms, shadow ran its shadow arms beside every ask, live let Arm
// route @route asks and ran no shadows.
func (l legacyRouter) migrate() (RouterConfig, error) {
	out := l.RouterConfig
	switch l.Mode {
	case RouterModeOff, "":
		out.Strategy, out.ShadowArms, out.SampleEvery = StrategyStatic, nil, nil
	case RouterModeShadow:
		out.Strategy = StrategyStatic
	case RouterModeLive:
		out.Strategy, out.ShadowArms, out.SampleEvery = l.Arm, nil, nil
	default:
		out.Strategy, out.ShadowArms, out.SampleEvery = StrategyStatic, nil, nil
		return out, fmt.Errorf("experiments.router.mode must be off, shadow or live, got %q", l.Mode)
	}
	return out, nil
}

// redactedYAML renders the block as a routing: section with keys hidden.
func (r RouterConfig) redactedYAML() string {
	for _, m := range []*RouterModel{&r.Jev, &r.Frontier} {
		if m.APIKey != "" {
			m.APIKey = "<set>"
		}
	}
	b, err := yaml.Marshal(map[string]RouterConfig{"routing": r})
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// envSet lists which prefix+key env vars are set, sorted.
func envSet(prefix string, keys []string) []string {
	var set []string
	for _, k := range keys {
		if os.Getenv(prefix+k) != "" {
			set = append(set, prefix+k)
		}
	}
	sort.Strings(set)
	return set
}

// applyRouterEnv overlays the per-field env vars under prefix onto r.
func applyRouterEnv(r *RouterConfig, prefix string) {
	setBoolEnv(&r.Record, prefix+"RECORD")
	setBoolEnv(&r.RecordText, prefix+"RECORD_TEXT")
	if v := os.Getenv(prefix + "SHADOW_ARMS"); v != "" {
		r.ShadowArms = SplitCSV(v)
	}
	setFloatEnv(&r.TimeoutSeconds, prefix+"TIMEOUT_SECONDS")
	setBoolEnv(&r.AllowServiceTargets, prefix+"ALLOW_SERVICE_TARGETS")
	setStringEnv(&r.Jev.APIKey, prefix+"JEV__API_KEY")
	setStringEnv(&r.Jev.Model, prefix+"JEV__MODEL")
	setStringEnv(&r.Jev.BaseURL, prefix+"JEV__BASE_URL")
	setStringEnv(&r.Frontier.APIKey, prefix+"FRONTIER__API_KEY")
	setStringEnv(&r.Frontier.Model, prefix+"FRONTIER__MODEL")
	setStringEnv(&r.Frontier.BaseURL, prefix+"FRONTIER__BASE_URL")
}

// applyRoutingEnv applies REPOWIRE_ROUTING__* and the provider key vars.
// Any key env var wins over YAML; a prefixed one wins over the bare vars.
func applyRoutingEnv(cfg *Config) {
	r := &cfg.Routing
	setStringEnv(&r.Strategy, routingEnvPrefix+"STRATEGY")
	applyRouterEnv(r, routingEnvPrefix)
	if v := firstEnv(routingEnvPrefix+"JEV__API_KEY", legacyEnvPrefix+"JEV__API_KEY", "JEV_API_KEY", "TYPESAFE_API_KEY"); v != "" {
		r.Jev.APIKey = v
	}
	if v := firstEnv(routingEnvPrefix+"FRONTIER__API_KEY", legacyEnvPrefix+"FRONTIER__API_KEY", "ANTHROPIC_API_KEY"); v != "" {
		r.Frontier.APIKey = v
	}
}

// settleRouting drops, with a warning, settings that cannot take effect: a
// repeated shadow arm, a sample period below 1, and shadow arms without
// recording. Strategy and arm names are checked against the arm registry
// by routing.NewRouterOrStatic, which degrades the same way.
func (cfg *Config) settleRouting() error {
	r := &cfg.Routing
	// Runs after every source (file, then env) has applied: an unusable
	// strategy that no env var repaired takes the shadows with it.
	if cfg.strategyUnusable && os.Getenv(routingEnvPrefix+"STRATEGY") == "" && len(r.ShadowArms) > 0 {
		cfg.Warnings = append(cfg.Warnings, StaticFallbackDropsShadows(r.ShadowArms))
		r.ShadowArms, r.SampleEvery = nil, nil
	}
	seen := map[string]bool{}
	var arms []string
	for _, arm := range r.ShadowArms {
		if seen[arm] {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("config: routing.shadow_arms lists %q twice; it runs once", arm))
			continue
		}
		seen[arm] = true
		arms = append(arms, arm)
	}
	r.ShadowArms = arms
	for arm, n := range r.SampleEvery {
		if n < 1 {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("config: routing.sample_every.%s must be >= 1, got %d; that arm is not run", arm, n))
			r.ShadowArms = slices.DeleteFunc(r.ShadowArms, func(s string) bool { return s == arm })
			delete(r.SampleEvery, arm)
		}
	}
	if len(r.ShadowArms) > 0 && !r.Record {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("config: routing.shadow_arms %v are not run because routing.record is false (shadow results are only kept in the record)", r.ShadowArms))
		r.ShadowArms = nil
	}
	return nil
}
