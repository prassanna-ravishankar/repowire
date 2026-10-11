package routing

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
)

// RouteTarget is the reserved address that asks a live router to choose.
const RouteTarget = "@route"

// IsRouteTarget reports whether to_peer names the router.
func IsRouteTarget(toPeer string) bool {
	return strings.EqualFold(strings.TrimSpace(toPeer), RouteTarget)
}

// Arms builds every known arm from config. Model arms without a key are still
// built; they fall back with reason "unconfigured" when run.
func Arms(cfg config.RouterConfig) map[string]Arm {
	httpClient := &http.Client{}
	arms := map[string]Arm{"random": Random{}}
	for _, view := range []View{ViewClaimed, ViewAttested, ViewBoth} {
		jev := Jev{View: view, APIKey: cfg.Jev.APIKey, Model: cfg.Jev.Model, BaseURL: cfg.Jev.BaseURL, HTTP: httpClient}
		frontier := Frontier{View: view, APIKey: cfg.Frontier.APIKey, Model: cfg.Frontier.Model, BaseURL: cfg.Frontier.BaseURL, HTTP: httpClient}
		arms[jev.Name()] = jev
		arms[frontier.Name()] = frontier
	}
	return arms
}

// Router holds the configured arms and the per-arm shadow sampling counters.
type Router struct {
	cfg  config.RouterConfig
	arms map[string]Arm

	mu   sync.Mutex
	seen map[string]uint64
}

// NewRouter returns a router over arms (Arms(cfg) when nil).
func NewRouter(cfg config.RouterConfig, arms map[string]Arm) *Router {
	if arms == nil {
		arms = Arms(cfg)
	}
	return &Router{cfg: cfg, arms: arms, seen: map[string]uint64{}}
}

// Recording reports whether route_decision rows are written.
func (r *Router) Recording() bool { return r != nil && r.cfg.Record }

// RecordText reports whether recorded rows keep the ask body.
func (r *Router) RecordText() bool { return r != nil && r.cfg.RecordText }

// Mode is the configured mode, "off" for a nil router.
func (r *Router) Mode() string {
	if r == nil {
		return config.RouterModeOff
	}
	return r.cfg.Mode
}

// Live reports whether asks to @route are routed.
func (r *Router) Live() bool { return r.Mode() == config.RouterModeLive }

// LiveArm is the arm that decides @route asks.
func (r *Router) LiveArm() string { return r.cfg.Arm }

// Timeout bounds one arm call.
func (r *Router) Timeout() time.Duration {
	return time.Duration(r.cfg.TimeoutSeconds * float64(time.Second))
}

// SampleEvery is the configured shadow sampling period for arm (>= 1).
func (r *Router) SampleEvery(arm string) int {
	if n := r.cfg.SampleEvery[arm]; n > 1 {
		return n
	}
	return 1
}

// ShadowPlan picks the shadow arms that run for this ask, advancing each arm's
// sampling counter. Empty unless mode is shadow.
func (r *Router) ShadowPlan() []string {
	if r.Mode() != config.RouterModeShadow {
		return nil
	}
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

// Run executes the named arm with the configured timeout. An unknown arm is a
// recorded fallback, never a panic or a silent pick.
func (r *Router) Run(ctx context.Context, name string, in Input) RouterResult {
	arm, ok := r.arms[name]
	if !ok {
		return RouterResult{Arm: name, Scores: []Score{}, Fallback: true, FallbackReason: FallbackUnknownArm, Error: "unknown arm " + name}
	}
	return Run(ctx, arm, in, r.Timeout())
}
