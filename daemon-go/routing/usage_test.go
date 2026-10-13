package routing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/repowire/repowire/daemon-go/config"
)

func TestRouterMetersProviderCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"m","answers":{"c1":{"type":"noul","noul":0.9}},"usage":{"input_tokens":1200,"output_tokens":4}}`))
	}))
	defer srv.Close()
	jev := Jev{View: ViewAttested, APIKey: "k", Model: "m", BaseURL: srv.URL, HTTP: srv.Client()}
	unkeyed := Jev{View: ViewBoth, Model: "m"}
	router, err := NewRouter(config.RouterConfig{TimeoutSeconds: 5}, map[string]Arm{jev.Name(): jev, unkeyed.Name(): unkeyed})
	if err != nil {
		t.Fatal(err)
	}
	in := Input{Candidates: []Candidate{{PeerID: "p1"}}}
	for range 3 {
		if res := router.Run(context.Background(), jev.Name(), in); res.Fallback {
			t.Fatalf("fallback %q", res.FallbackReason)
		}
	}
	router.Run(context.Background(), unkeyed.Name(), in) // unconfigured: no request left the process
	got := router.Usage()["jev"]
	if got.Calls != 3 || got.InputTokens != 3600 || got.Counted != 3 || got.OutputTokens != 12 || got.OutputCounted != 3 {
		t.Fatalf("usage = %+v, want 3 calls, 3600 input and 12 output tokens over 3", got)
	}
}

// Frontier meters both counts on every answered call, an abstention included:
// the provider bills it like any other.
func TestRouterMetersFrontierTokens(t *testing.T) {
	r := frontierRouter(t, func(call int) string {
		if call == 1 {
			return frontierAbstains
		}
		return `{\"confidence\":0.8,\"ranking\":[\"c1\",\"c2\"]}`
	})
	for range 2 {
		r.Run(context.Background(), "frontier-claimed", testInput())
	}
	got := r.Usage()["anthropic"]
	want := ProviderUsage{Calls: 2, InputTokens: 2400, Counted: 2, OutputTokens: 60, OutputCounted: 2}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}
