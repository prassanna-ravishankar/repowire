package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/routing"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

type traceRow struct {
	traceID, stage, status, peerID, fromPeerID string
	detail                                     map[string]any
}

// rowTracer keeps every trace row with its detail.
type rowTracer struct {
	mu   sync.Mutex
	rows []traceRow
}

func (r *rowTracer) RecordTrace(_ context.Context, traceID, _, stage, status, _, peerID, fromPeerID string, detail map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, traceRow{traceID, stage, status, peerID, fromPeerID, detail})
	return nil
}

func (r *rowTracer) stages(traceID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, row := range r.rows {
		if traceID == "" || row.traceID == traceID {
			out = append(out, row.stage)
		}
	}
	return out
}

func (r *rowTracer) find(traceID, stage string) []traceRow {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []traceRow
	for _, row := range r.rows {
		if (traceID == "" || row.traceID == traceID) && row.stage == stage {
			out = append(out, row)
		}
	}
	return out
}

func (r *rowTracer) decision(t *testing.T, traceID string) routing.Decision {
	t.Helper()
	rows := r.find(traceID, routing.Stage)
	if len(rows) != 1 {
		t.Fatalf("route_decision rows for %s = %d, want 1 (stages %v)", traceID, len(rows), r.stages(traceID))
	}
	d, err := routing.ParseDecision(rows[0].detail)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// stubArm returns a fixed ranking or error.
type stubArm struct {
	name   string
	scores []routing.Score
	err    error
	calls  int
	mu     sync.Mutex
}

func (s *stubArm) Name() string { return s.name }
func (s *stubArm) Rank(context.Context, routing.Input) (routing.RouterResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.err != nil {
		return routing.RouterResult{}, s.err
	}
	return routing.RouterResult{Scores: append([]routing.Score(nil), s.scores...)}, nil
}

type routingRig struct {
	srv   *httptest.Server
	hub   *Hub
	asks  *service.AskTracker
	tr    *rowTracer
	f     *fakeTransport
	alpha *proto.Peer
	beta  *proto.Peer
	gamma *proto.Peer
	other *proto.Peer
	sub   *proto.Peer
}

// newRoutingRig wires a hub over five peers: alpha (sender), beta and gamma in
// alpha's circle, other in another circle, and sub, a non-addressable thread.
func newRoutingRig(t *testing.T, cfg *config.RouterConfig, arms map[string]routing.Arm) *routingRig {
	t.Helper()
	return newRoutingRigWith(t, cfg, arms, nil)
}

// newRoutingRigWith records into store (a real state db) when it is non-nil.
func newRoutingRigWith(t *testing.T, cfg *config.RouterConfig, arms map[string]routing.Arm, store *state.Store) *routingRig {
	t.Helper()
	alpha := peerWith("repow-a", "alpha", "default", proto.StatusOnline)
	beta := peerWith("repow-b", "beta", "default", proto.StatusOnline)
	gamma := peerWith("repow-c", "gamma", "default", proto.StatusOnline)
	other := peerWith("repow-o", "other", "elsewhere", proto.StatusOnline)
	sub := peerWith("repow-s", "sub", "default", proto.StatusOnline)
	alpha.Path, alpha.Backend, alpha.Description = "/src/alpha", proto.AgentType("claude-code"), "writing the parser"
	beta.Path, beta.Backend, beta.TurnState, beta.Description = "/src/beta", proto.AgentType("codex"), proto.TurnIdle, "reviewing PR 12"
	sub.Provenance = proto.Provenance{Addressable: false, AddressableReason: "sub-agent thread"}
	for _, p := range []*proto.Peer{alpha, beta, gamma, other} {
		p.Provenance = proto.DefaultProvenance()
	}
	reg := newAskFakeRegistry(alpha, beta, gamma, other, sub)
	f := &fakeTransport{ackFrame: map[string]any{"status": "accepted"}}
	asks := service.NewAskTracker(0)
	tr := &rowTracer{}
	h := &Hub{deliveryTraces: tr}
	if store != nil {
		h.deliveryTraces, h.store = store, store
	}
	h.WithAskLifecycle(asks, service.NewPeerDelivery(reg, newRouterWithFake(f), f, asks, nil), reg)
	if cfg != nil {
		h.WithRouting(routing.NewRouter(*cfg, arms))
	}
	mux := http.NewServeMux()
	h.registerAskLifecycleRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &routingRig{srv, h, asks, tr, f, alpha, beta, gamma, other, sub}
}

func (r *routingRig) ask(t *testing.T, req AskRequest) (int, AskResponse, string) {
	t.Helper()
	resp := postJSON(t, r.srv.URL+"/ask", req)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out AskResponse
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

func (r *routingRig) deliveredText(t *testing.T) string {
	t.Helper()
	raw, _ := json.Marshal(r.f.lastFrame)
	return string(raw)
}

func recordCfg() *config.RouterConfig {
	cfg := config.Defaults().Experiments.Router
	cfg.Record = true
	return &cfg
}

// The flag-off contract: wiring the router with default config changes
// nothing a client or the trace ledger can see.
func TestRoutingDefaultsLeaveAskUnchanged(t *testing.T) {
	want := []string{"created", "resolved_peer", "routed", "thread_input_accepted", "acked", "closed"}
	for name, cfg := range map[string]*config.RouterConfig{"unwired": nil, "defaults": ptr(config.Defaults().Experiments.Router)} {
		t.Run(name, func(t *testing.T) {
			rig := newRoutingRig(t, cfg, nil)
			status, out, raw := rig.ask(t, AskRequest{FromPeer: "alpha", ToPeer: "beta", Text: "hi"})
			if status != http.StatusOK || strings.Contains(raw, "routing") {
				t.Fatalf("status %d body %s", status, raw)
			}
			reply := "ok"
			postJSON(t, rig.srv.URL+"/ack", AckRequest{CorrelationID: out.CorrelationID, Message: &reply}).Body.Close()
			if got := rig.tr.stages(""); !equalStrings(got, want) {
				t.Fatalf("stages = %v, want %v", got, want)
			}
			for _, row := range rig.tr.find("", "closed") {
				if _, ok := row.detail["time_to_close_ms"]; ok {
					t.Fatalf("closed detail gained outcome fields with recording off: %v", row.detail)
				}
			}
			if strings.Contains(rig.deliveredText(t), "routed by") {
				t.Fatal("explicit ask carried a routing note")
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestRecordedAskSnapshotsCandidatesAndOutcome(t *testing.T) {
	rig := newRoutingRig(t, recordCfg(), nil)
	status, out, _ := rig.ask(t, AskRequest{FromPeer: "alpha", ToPeer: "beta", Text: "can you review?"})
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	reply := "done"
	postJSON(t, rig.srv.URL+"/ack", AckRequest{CorrelationID: out.CorrelationID, Message: &reply}).Body.Close()

	stages := rig.tr.stages(out.CorrelationID)
	if stages[0] != routing.Stage {
		t.Fatalf("route_decision must be the first stage, got %v", stages)
	}
	d := rig.tr.decision(t, out.CorrelationID)
	if d.Schema != routing.SchemaV1 || d.Surface != routing.SurfaceAsk || d.Arm != routing.ArmExplicit || d.Mode != config.RouterModeOff {
		t.Fatalf("header = %+v", d)
	}
	if d.Declared.Resolution != routing.ResolutionResolved || *d.Declared.PeerID != "repow-b" || *d.Declared.Raw != "beta" {
		t.Fatalf("declared = %+v", d.Declared)
	}
	if d.Chosen == nil || d.Chosen.PeerID != "repow-b" {
		t.Fatalf("chosen = %+v", d.Chosen)
	}
	if d.Sender == nil || d.Sender.PeerID != "repow-a" || d.Sender.Description != "writing the parser" {
		t.Fatalf("sender = %+v", d.Sender)
	}
	if d.Request.Text != "can you review?" || !d.Request.TextRecorded || len(d.Request.TextSHA256) != 64 {
		t.Fatalf("request = %+v", d.Request)
	}
	access := map[string]string{}
	for _, c := range d.Candidates {
		access[c.PeerID] = c.Access
	}
	wantAccess := map[string]string{"repow-b": "ok", "repow-c": "ok", "repow-o": "circle_boundary", "repow-s": "not_addressable"}
	if len(access) != len(wantAccess) {
		t.Fatalf("candidates = %v (sender must be excluded)", access)
	}
	for id, want := range wantAccess {
		if access[id] != want {
			t.Fatalf("access[%s] = %q, want %q", id, access[id], want)
		}
	}
	beta := d.Candidates[0]
	if beta.PeerID != "repow-b" || beta.Backend != "codex" || beta.Project != "beta" || beta.TurnState != "idle" || beta.Description != "reviewing PR 12" {
		t.Fatalf("beta snapshot = %+v", beta)
	}
	closed := rig.tr.find(out.CorrelationID, "closed")
	if len(closed) != 1 || closed[0].detail["time_to_close_ms"] == nil {
		t.Fatalf("closed outcome = %+v", closed)
	}
}

func TestRecordTextOffKeepsOnlyHash(t *testing.T) {
	cfg := recordCfg()
	cfg.RecordText = false
	rig := newRoutingRig(t, cfg, nil)
	_, out, _ := rig.ask(t, AskRequest{FromPeer: "alpha", ToPeer: "beta", Text: "secret plan"})
	d := rig.tr.decision(t, out.CorrelationID)
	if d.Request.Text != "" || d.Request.TextRecorded || len(d.Request.TextSHA256) != 64 {
		t.Fatalf("request = %+v", d.Request)
	}
}

func TestRecordedFailedResolutionStillTraces(t *testing.T) {
	rig := newRoutingRig(t, recordCfg(), nil)
	for _, tc := range []struct {
		to, reason, resolution string
		status                 int
	}{
		{"nobody", "unknown", routing.ResolutionUnknown, http.StatusNotFound},
		{"sub", "not_addressable", routing.ResolutionResolved, http.StatusConflict},
	} {
		rig.tr.rows = nil
		status, _, _ := rig.ask(t, AskRequest{FromPeer: "alpha", ToPeer: tc.to, Text: "x"})
		if status != tc.status {
			t.Fatalf("%s: status %d, want %d", tc.to, status, tc.status)
		}
		if got := rig.tr.stages(""); !equalStrings(got, []string{routing.Stage, "route_failed"}) {
			t.Fatalf("%s: stages %v", tc.to, got)
		}
		failed := rig.tr.find("", "route_failed")[0]
		if failed.detail["reason"] != tc.reason {
			t.Fatalf("%s: reason %v", tc.to, failed.detail["reason"])
		}
		d := rig.tr.decision(t, failed.traceID)
		if d.Declared.Resolution != tc.resolution || d.Chosen != nil {
			t.Fatalf("%s: decision %+v", tc.to, d.Declared)
		}
	}
	if rig.asks.OpenCount() != 0 {
		t.Fatal("failed asks registered")
	}
}

func TestShadowArmsSampledAndRecordedAfterDelivery(t *testing.T) {
	cfg := recordCfg()
	cfg.Mode = config.RouterModeShadow
	cfg.ShadowArms = []string{"random", "paid"}
	cfg.SampleEvery = map[string]int{"paid": 2}
	paid := &stubArm{name: "paid", scores: []routing.Score{{PeerID: "repow-c", Score: 0.9}}}
	arms := map[string]routing.Arm{"random": routing.Random{}, "paid": paid}
	rig := newRoutingRig(t, cfg, arms)

	var cids []string
	for i := 0; i < 3; i++ {
		_, out, _ := rig.ask(t, AskRequest{FromPeer: "alpha", ToPeer: "beta", Text: "x"})
		cids = append(cids, out.CorrelationID)
		if rig.f.lastTarget != "repow-b" {
			t.Fatalf("shadow mode must deliver to the declared target, got %s", rig.f.lastTarget)
		}
	}
	rig.hub.waitShadow()
	wantPlans := [][]string{{"random", "paid"}, {"random"}, {"random", "paid"}}
	for i, cid := range cids {
		d := rig.tr.decision(t, cid)
		if !equalStrings(d.ShadowPlanned, wantPlans[i]) || d.Arm != routing.ArmExplicit {
			t.Fatalf("ask %d planned %v arm %s", i, d.ShadowPlanned, d.Arm)
		}
		if d.ShadowSampleEvery["paid"] != 2 && len(wantPlans[i]) == 2 {
			t.Fatalf("ask %d sample_every %v", i, d.ShadowSampleEvery)
		}
		shadows := rig.tr.find(cid, routing.ShadowStage)
		if len(shadows) != len(wantPlans[i]) {
			t.Fatalf("ask %d shadow rows %d", i, len(shadows))
		}
	}
	if paid.calls != 2 {
		t.Fatalf("paid arm ran %d times, want 2", paid.calls)
	}
	if strings.Contains(rig.deliveredText(t), "routed by") {
		t.Fatal("shadow decision leaked into delivery")
	}
}

func liveCfg(arm string) *config.RouterConfig {
	cfg := recordCfg()
	cfg.Mode = config.RouterModeLive
	cfg.Arm = arm
	return cfg
}

func TestLiveRouteDeliversArmPickVisibly(t *testing.T) {
	arm := &stubArm{name: "stub", scores: []routing.Score{{PeerID: "repow-c", Score: 0.81}, {PeerID: "repow-b", Score: 0.2}}}
	rig := newRoutingRig(t, liveCfg("stub"), map[string]routing.Arm{"stub": arm})
	status, out, _ := rig.ask(t, AskRequest{FromPeer: "alpha", ToPeer: "@route", Text: "who owns the lexer?"})
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if rig.f.lastTarget != "repow-c" {
		t.Fatalf("delivered to %s, want arm pick repow-c", rig.f.lastTarget)
	}
	if out.Routing == nil || out.Routing.RoutedTo != "gamma" || out.Routing.Arm != "stub" || out.Routing.Fallback {
		t.Fatalf("routing = %+v", out.Routing)
	}
	text := rig.deliveredText(t)
	if !strings.Contains(text, "[routed by stub because you scored 0.81, the highest of 2 candidates (next 0.20)") {
		t.Fatalf("delivery note missing: %s", text)
	}
	d := rig.tr.decision(t, out.CorrelationID)
	if d.Mode != config.RouterModeLive || d.Arm != "stub" || d.Router == nil || d.Chosen.PeerID != "repow-c" || d.Declared.Resolution != routing.ResolutionAbsent {
		t.Fatalf("decision = %+v router %+v", d, d.Router)
	}
}

func TestLiveRouteFailsClosedToFallback(t *testing.T) {
	cases := map[string]*stubArm{
		"arm error":       {name: "stub", err: errors.New("boom")},
		"ineligible pick": {name: "stub", scores: []routing.Score{{PeerID: "repow-o", Score: 1}}},
	}
	for name, arm := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newRoutingRig(t, liveCfg("stub"), map[string]routing.Arm{"stub": arm})
			status, out, _ := rig.ask(t, AskRequest{FromPeer: "alpha", ToPeer: "@route", FallbackTo: "beta", Text: "x"})
			if status != http.StatusOK || rig.f.lastTarget != "repow-b" {
				t.Fatalf("status %d delivered to %s", status, rig.f.lastTarget)
			}
			if out.Routing == nil || !out.Routing.Fallback || out.Routing.RoutedTo != "beta" {
				t.Fatalf("routing = %+v", out.Routing)
			}
			if !strings.Contains(rig.deliveredText(t), "[routing: stub failed (") {
				t.Fatalf("fallback not visible in delivery: %s", rig.deliveredText(t))
			}
			d := rig.tr.decision(t, out.CorrelationID)
			if !d.Router.Fallback || d.Router.FallbackReason == "" || *d.Declared.PeerID != "repow-b" {
				t.Fatalf("decision router %+v declared %+v", d.Router, d.Declared)
			}
		})
	}
}

func TestLiveRouteWithoutFallbackRefusesLoudly(t *testing.T) {
	arm := &stubArm{name: "stub", err: errors.New("down")}
	rig := newRoutingRig(t, liveCfg("stub"), map[string]routing.Arm{"stub": arm})
	status, _, raw := rig.ask(t, AskRequest{FromPeer: "alpha", ToPeer: "@route", Text: "x"})
	if status != http.StatusServiceUnavailable || !strings.Contains(raw, "router_unavailable") {
		t.Fatalf("status %d body %s", status, raw)
	}
	if rig.f.lastTarget != "" || rig.asks.OpenCount() != 0 {
		t.Fatal("a failed route must not deliver or register")
	}
	if got := rig.tr.stages(""); !equalStrings(got, []string{routing.Stage, "route_failed"}) {
		t.Fatalf("stages %v", got)
	}
}

func TestRouteTargetIsOrdinaryNameUnlessLive(t *testing.T) {
	arm := &stubArm{name: "stub", scores: []routing.Score{{PeerID: "repow-c", Score: 1}}}
	cfg := recordCfg()
	cfg.Mode = config.RouterModeShadow
	cfg.Arm = "stub"
	rig := newRoutingRig(t, cfg, map[string]routing.Arm{"stub": arm})
	status, _, _ := rig.ask(t, AskRequest{FromPeer: "alpha", ToPeer: "@route", Text: "x"})
	if status != http.StatusNotFound || arm.calls != 0 {
		t.Fatalf("status %d calls %d", status, arm.calls)
	}
}

// The static track end to end: asks recorded into a real state db in shadow
// mode replay offline, both by re-running an arm and from the shadow rows.
func TestRecordedAsksReplayFromStateDB(t *testing.T) {
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := recordCfg()
	cfg.Mode = config.RouterModeShadow
	cfg.ShadowArms = []string{"oracle-ish"}
	cfg.SampleEvery = map[string]int{"oracle-ish": 2}
	arm := &stubArm{name: "oracle-ish", scores: []routing.Score{{PeerID: "repow-b", Score: 0.9}, {PeerID: "repow-c", Score: 0.1}}}
	rig := newRoutingRigWith(t, cfg, map[string]routing.Arm{"oracle-ish": arm}, store)

	for _, to := range []string{"beta", "gamma", "beta", "nobody"} {
		rig.ask(t, AskRequest{FromPeer: "alpha", ToPeer: to, Text: "x"})
	}
	rig.hub.waitShadow()

	ctx := context.Background()
	examples, err := routing.LoadExamples(ctx, store, time.Time{})
	if err != nil || len(examples) != 4 {
		t.Fatalf("examples = %d, %v", len(examples), err)
	}
	recorded, _ := routing.Replay(ctx, examples, "oracle-ish", nil, true)
	// Sampled on asks 1 and 3 (both to beta, both hits); ask 2 unsampled; ask 4 unresolved.
	if recorded.Scored != 2 || recorded.Hit1 != 2 || recorded.Skipped[routing.SkipNotSampled] != 1 || recorded.Skipped[routing.SkipNoDeclaredTarget] != 1 {
		t.Fatalf("recorded replay = %+v", recorded)
	}
	router := routing.NewRouter(*cfg, map[string]routing.Arm{"oracle-ish": arm})
	rerun, _ := routing.Replay(ctx, examples, "oracle-ish", func(ctx context.Context, in routing.Input) routing.RouterResult {
		return router.Run(ctx, "oracle-ish", in)
	}, false)
	// Re-run scores every labelled ask: beta, gamma, beta → 2 hits of 3.
	if rerun.Scored != 3 || rerun.Hit1 != 2 || rerun.Hit3 != 3 {
		t.Fatalf("rerun replay = %+v", rerun)
	}
}
