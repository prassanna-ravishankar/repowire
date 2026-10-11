package config

import (
	"os"
	"path/filepath"
	"testing"
)

func clearRouterEnv(t *testing.T) {
	for _, key := range []string{"JEV_API_KEY", "TYPESAFE_API_KEY", "ANTHROPIC_API_KEY", "REPOWIRE_EXPERIMENTS__ROUTER__JEV__API_KEY", "REPOWIRE_EXPERIMENTS__ROUTER__FRONTIER__API_KEY"} {
		t.Setenv(key, "")
	}
}

func TestRouterDefaultsAreOff(t *testing.T) {
	clearRouterEnv(t)
	t.Setenv("REPOWIRE_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Experiments.Router
	if r.Record || r.Mode != RouterModeOff || r.Arm != "explicit" || len(r.ShadowArms) != 0 || !r.RecordText || r.TimeoutSeconds != 8 {
		t.Fatalf("router defaults = %+v", r)
	}
}

func TestRouterYAMLAndEnv(t *testing.T) {
	clearRouterEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("REPOWIRE_CONFIG", path)
	if err := os.WriteFile(path, []byte(`
experiments:
  router:
    record: true
    mode: shadow
    shadow_arms: [random, jev-attested]
    sample_every: {frontier-claimed: 20}
    jev: {api_key: yaml-key}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REPOWIRE_EXPERIMENTS__ROUTER__RECORD_TEXT", "false")
	t.Setenv("REPOWIRE_EXPERIMENTS__ROUTER__SHADOW_ARMS", "jev-both,frontier-claimed")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Experiments.Router
	if !r.Record || r.RecordText || r.Mode != RouterModeShadow || r.SampleEvery["frontier-claimed"] != 20 {
		t.Fatalf("router = %+v", r)
	}
	if len(r.ShadowArms) != 2 || r.ShadowArms[0] != "jev-both" {
		t.Fatalf("shadow arms = %v", r.ShadowArms)
	}
	if r.Jev.APIKey != "yaml-key" || r.Jev.Model != "jev-1.13.0" || r.Jev.BaseURL == "" {
		t.Fatalf("jev = %+v (partial YAML must keep defaults)", r.Jev)
	}
}
