package routing

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
)

// Arms builds every known arm from config. Model arms without a key are still
// built; they fall back with reason "unconfigured" when run.
func Arms(cfg config.RouterConfig) map[string]Arm {
	httpClient := &http.Client{}
	arms := map[string]Arm{"random": Random{}}
	for _, view := range []View{ViewClaimed, ViewAttested, ViewBoth} {
		clef := Clef{Family: ClefFamily, View: view, APIKey: cfg.Clef.APIKey, AccountID: cfg.Clef.AccountID, Model: cfg.Clef.Model, BaseURL: cfg.Clef.BaseURL, HTTP: httpClient}
		flash := clef
		flash.Family, flash.Model = ClefFlashFamily, cfg.Clef.FlashModel
		for _, arm := range []Arm{
			Jev{View: view, APIKey: cfg.Jev.APIKey, Model: cfg.Jev.Model, BaseURL: cfg.Jev.BaseURL, HTTP: httpClient},
			Frontier{View: view, APIKey: cfg.Frontier.APIKey, Model: cfg.Frontier.Model, BaseURL: cfg.Frontier.BaseURL, HTTP: httpClient},
			clef, flash,
		} {
			arms[arm.Name()] = arm
		}
	}
	return arms
}

// maxShadowInFlight bounds concurrent shadow runs across all asks. A planned
// shadow arm that finds the pool full is recorded as saturated, not queued.
const maxShadowInFlight = 4

// providerArm is implemented by arms backed by an external provider; arms
// sharing a provider share a breaker.
type providerArm interface{ Provider() string }

// Router holds the configured arms, the per-arm shadow sampling counters, the
// per-provider breakers and the shadow pool.
type Router struct {
	cfg  config.RouterConfig
	arms map[string]Arm

	mu       sync.Mutex
	seen     map[string]uint64
	breakers map[string]*breaker
	shadow   chan struct{}
	now      func() time.Time
}

// NewRouter returns a router over arms (Arms(cfg) when nil). Every arm the
// config names must exist.
func NewRouter(cfg config.RouterConfig, arms map[string]Arm) (*Router, error) {
	if arms == nil {
		arms = Arms(cfg)
	}
	named := append([]string{}, cfg.ShadowArms...)
	if cfg.Strategy != "" && cfg.Strategy != StrategyStatic {
		named = append(named, cfg.Strategy)
	}
	for _, name := range named {
		if _, ok := arms[name]; !ok {
			return nil, fmt.Errorf("routing: unknown arm %q (strategy is static or an arm name)", name)
		}
	}
	// A misspelled sample_every key would silently leave its arm unsampled
	// (every ask), so it is an error like any other unknown arm.
	for name := range cfg.SampleEvery {
		if _, ok := arms[name]; !ok {
			return nil, fmt.Errorf("routing.sample_every: unknown arm %q", name)
		}
	}
	return &Router{
		cfg: cfg, arms: arms, seen: map[string]uint64{}, breakers: map[string]*breaker{},
		shadow: make(chan struct{}, maxShadowInFlight), now: time.Now,
	}, nil
}

// NewRouterOrStatic is the daemon's constructor: it never fails. A shadow arm
// or sample_every key that names no arm is dropped with a warning. A
// strategy that names no arm degrades to static and drops the shadow arms
// with it. Recording is kept either way. A typo in routing degrades routing
// and never stops the daemon.
func NewRouterOrStatic(cfg config.RouterConfig, arms map[string]Arm) (*Router, []string) {
	if arms == nil {
		arms = Arms(cfg)
	}
	var warnings []string
	if cfg.Strategy != "" && cfg.Strategy != StrategyStatic {
		if _, ok := arms[cfg.Strategy]; !ok {
			warnings = append(warnings, fmt.Sprintf("routing.strategy %q names no arm; routing stays static", cfg.Strategy))
			if len(cfg.ShadowArms) > 0 {
				warnings = append(warnings, config.StaticFallbackDropsShadows(cfg.ShadowArms))
			}
			cfg.Strategy, cfg.ShadowArms, cfg.SampleEvery = StrategyStatic, nil, nil
		}
	}
	var shadows []string
	for _, name := range cfg.ShadowArms {
		if _, ok := arms[name]; ok {
			shadows = append(shadows, name)
		} else {
			warnings = append(warnings, fmt.Sprintf("routing.shadow_arms: %q names no arm; it is not run", name))
		}
	}
	cfg.ShadowArms = shadows
	sample := map[string]int{}
	for name, n := range cfg.SampleEvery {
		if _, ok := arms[name]; ok {
			sample[name] = n
		} else {
			warnings = append(warnings, fmt.Sprintf("routing.sample_every: %q names no arm; ignored", name))
		}
	}
	cfg.SampleEvery = sample
	r, err := NewRouter(cfg, arms)
	if err != nil { // unreachable: every name was checked above
		warnings = append(warnings, fmt.Sprintf("routing disabled: %v", err))
		r, _ = NewRouter(config.RouterConfig{Strategy: StrategyStatic, TimeoutSeconds: cfg.TimeoutSeconds}, arms)
	}
	return r, warnings
}

// TryAcquireShadow takes a shadow pool slot without blocking.
func (r *Router) TryAcquireShadow() bool {
	select {
	case r.shadow <- struct{}{}:
		return true
	default:
		return false
	}
}

// ReleaseShadow returns a slot taken by TryAcquireShadow.
func (r *Router) ReleaseShadow() { <-r.shadow }

func (r *Router) breakerFor(arm Arm) *breaker {
	p, ok := arm.(providerArm)
	if !ok {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.breakers[p.Provider()]
	if b == nil {
		b = &breaker{now: r.now}
		r.breakers[p.Provider()] = b
	}
	return b
}

// Config is the effective routing config: what this router actually runs,
// after NewRouterOrStatic dropped anything unusable.
func (r *Router) Config() config.RouterConfig { return r.cfg }

// Recording reports whether route_decision rows are written.
func (r *Router) Recording() bool { return r != nil && r.cfg.Record }

// SelectionPolicy is the policy a strategy delivers under.
func (r *Router) SelectionPolicy() string {
	if r != nil && r.cfg.AllowServiceTargets {
		return PolicyAllowServiceTargets
	}
	return PolicyExcludeServiceTargets
}

// RecordText reports whether recorded rows keep the ask body.
func (r *Router) RecordText() bool { return r != nil && r.cfg.RecordText }

// Mode is the mode recorded on each decision, derived from the config so
// rows read as they did under experiments.router: live when a strategy
// routes targetless asks, else shadow when shadow arms run, else off.
func (r *Router) Mode() string {
	switch {
	case r == nil:
		return config.RouterModeOff
	case r.Strategy() != StrategyStatic:
		return config.RouterModeLive
	case len(r.cfg.ShadowArms) > 0:
		return config.RouterModeShadow
	}
	return config.RouterModeOff
}

// StrategyStatic is the default strategy: every ask names its target and a
// targetless ask has nowhere to go. Any other strategy is the name of the
// arm that chooses a target for an ask that names none.
const StrategyStatic = config.StrategyStatic

// Strategy is the configured routing strategy, static for a nil router.
func (r *Router) Strategy() string {
	if r == nil || r.cfg.Strategy == "" {
		return StrategyStatic
	}
	return r.cfg.Strategy
}

// Timeout bounds one arm call.
func (r *Router) Timeout() time.Duration {
	return time.Duration(r.cfg.TimeoutSeconds * float64(time.Second))
}

// SampleEvery is the shadow sampling period for arm: the configured value
// (validated >= 1 at config load), else 1.
func (r *Router) SampleEvery(arm string) int {
	if n, ok := r.cfg.SampleEvery[arm]; ok {
		return n
	}
	return 1
}

// ShadowPlan picks the shadow arms that run for this ask, advancing each arm's
// sampling counter.
func (r *Router) ShadowPlan() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var plan []string
	for _, arm := range r.cfg.ShadowArms {
		n := uint64(r.SampleEvery(arm))
		if r.seen[arm]%n == 0 {
			plan = append(plan, arm)
		}
		r.seen[arm]++
	}
	return plan
}

// Skipped is a fallback for an arm that never ran, stamped with the arm's
// configured model and template so missing work stays attributable.
func (r *Router) Skipped(name, reason string) RouterResult {
	res := fallbackResult(name, reason)
	if arm, ok := r.arms[name]; ok {
		res = withIdentity(arm, res)
	}
	return res
}

// Run executes the named arm with the configured timeout behind its
// provider's breaker. An unknown arm or an open breaker is a recorded
// fallback, never a panic or a silent pick.
func (r *Router) Run(ctx context.Context, name string, in Input) RouterResult {
	arm, ok := r.arms[name]
	if !ok {
		return fallbackResult(name, FallbackUnknownArm)
	}
	b := r.breakerFor(arm)
	if b == nil {
		return Run(ctx, arm, in, r.Timeout())
	}
	allowed, probe, gatedBy := b.allow()
	if !allowed {
		res := withIdentity(arm, fallbackResult(name, FallbackBreakerOpen))
		res.GatedBy = gatedBy
		return res
	}
	res := Run(ctx, arm, in, r.Timeout())
	b.record(res, probe)
	return res
}
