package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func clearRouterEnv(t *testing.T) {
	t.Helper()
	keys := []string{"JEV_API_KEY", "TYPESAFE_API_KEY", "ANTHROPIC_API_KEY", routingEnvPrefix + "STRATEGY", legacyEnvPrefix + "MODE", legacyEnvPrefix + "ARM"}
	for _, k := range routerEnvKeys {
		keys = append(keys, routingEnvPrefix+k, legacyEnvPrefix+k)
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}
}

// loadYAML loads body as the config file with a clean routing env.
func loadYAML(t *testing.T, body string) Config {
	t.Helper()
	cfg, err := tryLoadYAML(t, body)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func tryLoadYAML(t *testing.T, body string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("REPOWIRE_CONFIG", path)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load()
}

func TestRoutingDefaultsAreStatic(t *testing.T) {
	clearRouterEnv(t)
	t.Setenv("REPOWIRE_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Routing
	if r.Strategy != StrategyStatic || r.Record || len(r.ShadowArms) != 0 || !r.RecordText || r.TimeoutSeconds != 8 || len(cfg.Warnings) != 0 {
		t.Fatalf("routing defaults = %+v warnings %v", r, cfg.Warnings)
	}
}

func TestRoutingYAMLAndEnv(t *testing.T) {
	clearRouterEnv(t)
	t.Setenv(routingEnvPrefix+"RECORD_TEXT", "false")
	t.Setenv(routingEnvPrefix+"SHADOW_ARMS", "jev-both,frontier-claimed")
	cfg := loadYAML(t, `
routing:
  record: true
  shadow_arms: [random, jev-attested]
  sample_every: {frontier-claimed: 20}
  allow_service_targets: true
  jev: {api_key: yaml-key}
`)
	r := cfg.Routing
	if !r.AllowServiceTargets {
		t.Fatal("allow_service_targets not loaded")
	}
	if r.Strategy != StrategyStatic || !r.Record || r.RecordText || r.SampleEvery["frontier-claimed"] != 20 {
		t.Fatalf("routing = %+v", r)
	}
	if !reflect.DeepEqual(r.ShadowArms, []string{"jev-both", "frontier-claimed"}) {
		t.Fatalf("shadow arms = %v", r.ShadowArms)
	}
	if r.Jev.APIKey != "yaml-key" || r.Jev.Model != "jev-1.13.0" || r.Jev.BaseURL == "" {
		t.Fatalf("jev = %+v (partial YAML must keep defaults)", r.Jev)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("warnings = %v", cfg.Warnings)
	}
}

// A legacy experiments.router block starts the daemon with exactly the
// arms that ran before, and says how to move it.
func TestLegacyRouterBlockMigratesInMemory(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want RouterConfig
	}{
		"shadow keeps its shadows": {
			"mode: shadow\n    record: true\n    shadow_arms: [jev-attested]\n    sample_every: {jev-attested: 2}\n    jev: {api_key: secret-key}",
			func() RouterConfig {
				r := defaultRouting()
				r.Record, r.ShadowArms, r.SampleEvery, r.Jev.APIKey = true, []string{"jev-attested"}, map[string]int{"jev-attested": 2}, "secret-key"
				return r
			}(),
		},
		"off ran no shadows": {
			"mode: off\n    record: true\n    shadow_arms: [jev-attested]",
			func() RouterConfig { r := defaultRouting(); r.Record = true; return r }(),
		},
		"live becomes its arm, without shadows": {
			"mode: live\n    arm: jev-attested\n    record: true\n    shadow_arms: [random]",
			func() RouterConfig { r := defaultRouting(); r.Strategy, r.Record = "jev-attested", true; return r }(),
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			clearRouterEnv(t)
			cfg := loadYAML(t, "experiments:\n  git_trailers: false\n  router:\n    "+c.yaml+"\n")
			if !reflect.DeepEqual(cfg.Routing, c.want) {
				t.Fatalf("routing = %+v\nwant      %+v", cfg.Routing, c.want)
			}
			if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "experiments.router is deprecated and was migrated in memory") ||
				!strings.Contains(cfg.Warnings[0], "strategy: "+c.want.Strategy) {
				t.Fatalf("warnings = %v", cfg.Warnings)
			}
			if strings.Contains(cfg.Warnings[0], "secret-key") {
				t.Fatalf("migration warning leaked an API key: %s", cfg.Warnings[0])
			}
			if cfg.Experiments.GitTrailers {
				t.Fatal("the rest of experiments: must still load")
			}
		})
	}
}

func TestLegacyRouterEnvMigrates(t *testing.T) {
	clearRouterEnv(t)
	t.Setenv(legacyEnvPrefix+"MODE", "live")
	t.Setenv(legacyEnvPrefix+"ARM", "jev-both")
	t.Setenv(legacyEnvPrefix+"RECORD", "true")
	t.Setenv(legacyEnvPrefix+"JEV__API_KEY", "env-key")
	cfg := loadYAML(t, "")
	if cfg.Routing.Strategy != "jev-both" || !cfg.Routing.Record || cfg.Routing.Jev.APIKey != "env-key" {
		t.Fatalf("routing = %+v", cfg.Routing)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], legacyEnvPrefix+"MODE") || strings.Contains(cfg.Warnings[0], "env-key") {
		t.Fatalf("warnings = %v", cfg.Warnings)
	}
}

func TestRoutingBlockWinsOverLegacy(t *testing.T) {
	clearRouterEnv(t)
	t.Setenv(legacyEnvPrefix+"MODE", "live")
	t.Setenv(legacyEnvPrefix+"JEV__API_KEY", "env-key")
	cfg := loadYAML(t, "experiments:\n  router:\n    mode: live\n    arm: random\nrouting:\n  record: true\n")
	if cfg.Routing.Strategy != StrategyStatic || !cfg.Routing.Record || cfg.Routing.Jev.APIKey != "env-key" {
		t.Fatalf("routing = %+v", cfg.Routing)
	}
	joined := strings.Join(cfg.Warnings, "\n")
	for _, want := range []string{"experiments.router is ignored because routing: is set", legacyEnvPrefix + "MODE is ignored", legacyEnvPrefix + "JEV__API_KEY is deprecated; use " + routingEnvPrefix + "JEV__API_KEY"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings missing %q:\n%s", want, joined)
		}
	}
}

// Any key env var beats YAML, and a prefixed one beats the bare vars, as
// before the graduation.
func TestRoutingKeyPrecedence(t *testing.T) {
	clearRouterEnv(t)
	t.Setenv("JEV_API_KEY", "bare")
	if got := loadYAML(t, "routing:\n  jev: {api_key: yaml}\n").Routing.Jev.APIKey; got != "bare" {
		t.Fatalf("bare env vs yaml: %q", got)
	}
	t.Setenv(routingEnvPrefix+"JEV__API_KEY", "prefixed")
	if got := loadYAML(t, "routing:\n  jev: {api_key: yaml}\n").Routing.Jev.APIKey; got != "prefixed" {
		t.Fatalf("prefixed env vs bare: %q", got)
	}
}

// Shadows cannot run unrecorded; that drops the shadows, it does not stop
// the daemon.
func TestShadowsWithoutRecordAreDroppedNotRefused(t *testing.T) {
	clearRouterEnv(t)
	cfg := loadYAML(t, "routing:\n  shadow_arms: [jev-attested]\n")
	if len(cfg.Routing.ShadowArms) != 0 || len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "routing.record is false") {
		t.Fatalf("routing %+v warnings %v", cfg.Routing, cfg.Warnings)
	}
}

// Real load failures stay fatal: a file that is not YAML, and (until the
// general decode boundary is reworked) a mistyped key outside routing:.
func TestNonRoutingLoadFailuresStillRefuse(t *testing.T) {
	for name, body := range map[string]string{
		"not yaml":            "routing: [unclosed\n",
		"mistyped relay flag": "relay:\n  enabled: definitely\n",
	} {
		t.Run(name, func(t *testing.T) {
			clearRouterEnv(t)
			if _, err := tryLoadYAML(t, body); err == nil {
				t.Fatalf("%s loaded", name)
			}
		})
	}
}

// No routing mistake refuses to load: the daemon is a KeepAlive service, so
// a refusal is a crash loop that takes the mesh down. Each mistake drops
// the setting it breaks, with a warning.
func TestRoutingTyposDegradeInsteadOfRefusing(t *testing.T) {
	for name, c := range map[string]struct {
		body, warning string
		check         func(RouterConfig) bool
	}{
		"duplicate arm": {"routing:\n  record: true\n  shadow_arms: [jev-both, jev-both]\n", "twice",
			func(r RouterConfig) bool { return reflect.DeepEqual(r.ShadowArms, []string{"jev-both"}) }},
		"zero sample": {"routing:\n  record: true\n  shadow_arms: [frontier-claimed, random]\n  sample_every: {frontier-claimed: 0}\n", ">= 1",
			func(r RouterConfig) bool {
				return reflect.DeepEqual(r.ShadowArms, []string{"random"}) && len(r.SampleEvery) == 0
			}},
		"negative sample": {"routing:\n  sample_every: {frontier-claimed: -3}\n", ">= 1",
			func(r RouterConfig) bool { return len(r.SampleEvery) == 0 }},
		"legacy unknown mode": {"experiments:\n  router:\n    mode: sometimes\n    shadow_arms: [random]\n", "must be off, shadow or live",
			func(r RouterConfig) bool { return r.Strategy == StrategyStatic && len(r.ShadowArms) == 0 }},
		"legacy key of the wrong type": {"experiments:\n  router:\n    mode: shadow\n    record: true\n    shadow_arms: {not: a list}\n", "experiments.router.shadow_arms is ignored",
			func(r RouterConfig) bool { return r.Strategy == StrategyStatic && r.Record && len(r.ShadowArms) == 0 }},
		"strategy of the wrong type": {"routing:\n  strategy: [random]\n  record: true\n", "routing.strategy is ignored",
			func(r RouterConfig) bool { return r.Strategy == StrategyStatic && r.Record }},
		"strategy of the wrong type drops the shadows": {"routing:\n  strategy: [random]\n  record: true\n  shadow_arms: [random]\n", "routing.strategy is unusable",
			func(r RouterConfig) bool { return r.Strategy == StrategyStatic && r.Record && len(r.ShadowArms) == 0 }},
		"shadow_arms of the wrong type": {"routing:\n  record: true\n  shadow_arms: {jev-attested: 1}\n", "routing.shadow_arms is ignored",
			func(r RouterConfig) bool { return r.Record && len(r.ShadowArms) == 0 }},
		"sample_every of the wrong type": {"routing:\n  record: true\n  sample_every: 5\n", "routing.sample_every is ignored",
			func(r RouterConfig) bool { return r.Record && len(r.SampleEvery) == 0 }},
		"record of the wrong type": {"routing:\n  record: maybe\n  strategy: random\n", "routing.record is ignored",
			func(r RouterConfig) bool { return !r.Record && r.Strategy == "random" }},
		"routing that is not a mapping": {"routing: static\n", "routing must be a mapping",
			func(r RouterConfig) bool { return r.Strategy == StrategyStatic }},
	} {
		t.Run(name, func(t *testing.T) {
			clearRouterEnv(t)
			cfg, err := tryLoadYAML(t, c.body)
			if err != nil {
				t.Fatalf("refused to load: %v", err)
			}
			if !c.check(cfg.Routing) || !strings.Contains(strings.Join(cfg.Warnings, "\n"), c.warning) {
				t.Fatalf("routing %+v warnings %v", cfg.Routing, cfg.Warnings)
			}
		})
	}
}

// Recovery must not change what a block means: YAML merge keys and aliases
// resolve exactly as a whole-block decode would, with an explicit key
// winning over a merged one.
func TestRoutingKeepsYAMLMergeAndAliasSemantics(t *testing.T) {
	for name, c := range map[string]struct {
		body     string
		strategy string
		record   bool
		warning  string
	}{
		"explicit key beats a later merge":    {"routing:\n  strategy: static\n  <<: {strategy: random, record: true}\n", StrategyStatic, true, ""},
		"explicit key beats an earlier merge": {"routing:\n  <<: {strategy: random}\n  strategy: static\n", StrategyStatic, false, ""},
		"merge fills an unset key":            {"base: &b {strategy: random}\nrouting:\n  <<: *b\n  record: true\n", "random", true, ""},
		"aliased block":                       {"template: &r {strategy: random, record: true}\nrouting: *r\n", "random", true, ""},
		"merge with a mistyped key":           {"routing:\n  strategy: static\n  <<: {strategy: random, record: maybe}\n", StrategyStatic, false, "routing.record is ignored"},
	} {
		t.Run(name, func(t *testing.T) {
			clearRouterEnv(t)
			cfg := loadYAML(t, c.body)
			if cfg.Routing.Strategy != c.strategy || cfg.Routing.Record != c.record {
				t.Fatalf("routing %+v", cfg.Routing)
			}
			joined := strings.Join(cfg.Warnings, "\n")
			if (c.warning == "" && joined != "") || !strings.Contains(joined, c.warning) {
				t.Fatalf("warnings %q, want %q", joined, c.warning)
			}
		})
	}
}
