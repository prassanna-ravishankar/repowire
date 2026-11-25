package hooks

import "testing"

// A runtime the daemon brokers itself runs with REPOWIRE_HOOKS=off: every
// session hook returns at once, reading nothing.
func TestHooksOffMakesEverySessionHookANoOp(t *testing.T) {
	t.Setenv(HooksDisabledEnv, "off")
	for _, name := range []string{"session", "stop", "prompt", "notification", "pretooluse"} {
		if code := Run([]string{name}); code != 0 {
			t.Fatalf("hook %s with hooks off: exit %d, want 0", name, code)
		}
	}
}
