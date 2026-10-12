package hub

// Characterization of POST /ask. Each scenario drives the real handler over
// logging fakes and records everything a client, the ledger, the tracker or
// a transport can observe: status and body, the interleaved order of
// registry, ledger and transport calls, every trace row, the tracker's asks
// and the delivered frame. The goldens were written against the pre-refactor
// ask path; any change to them is a behaviour change and must be deliberate.
//
// Regenerate with: go test ./hub -run TestAskCharacterization -update-characterization

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

var updateCharacterization = flag.Bool("update-characterization", false, "rewrite the /ask characterization goldens")

// charLog is the interleaved call log shared by every logging fake.
type charLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *charLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func circleArg(c *string) string {
	if c == nil {
		return "nil"
	}
	return *c
}

type charRegistry struct {
	*trickyRegistry
	log *charLog
}

func (r *charRegistry) GetPeerByName(name string, circle *string) (*proto.Peer, error) {
	p, err := r.trickyRegistry.GetPeerByName(name, circle)
	r.log.add("reg.GetPeerByName(%q, %s) -> %s %v", name, circleArg(circle), peerIDOf(p), err)
	return p, err
}

func (r *charRegistry) CheckAccess(ctx context.Context, from, to string, bypass bool, circle *string) (*proto.Peer, *proto.Peer, error) {
	f, t, err := r.trickyRegistry.CheckAccess(ctx, from, to, bypass, circle)
	r.log.add("reg.CheckAccess(%q, %q, bypass=%t, %s) -> %s %s %v", from, to, bypass, circleArg(circle), peerIDOf(f), peerIDOf(t), err)
	return f, t, err
}

func (r *charRegistry) GetAllPeers() []*proto.Peer {
	r.log.add("reg.GetAllPeers")
	return r.trickyRegistry.GetAllPeers()
}

func (r *charRegistry) AddEvent(ctx context.Context, typ string, payload map[string]any) string {
	r.log.add("reg.AddEvent(%s)", typ)
	return r.trickyRegistry.AddEvent(ctx, typ, payload)
}

func (r *charRegistry) MarkOffline(ctx context.Context, id proto.PeerID, terminal bool) (int, error) {
	r.log.add("reg.MarkOffline(%s)", id)
	return r.trickyRegistry.MarkOffline(ctx, id, terminal)
}

func peerIDOf(p *proto.Peer) string {
	if p == nil {
		return "<nil>"
	}
	return string(p.PeerID)
}

type charTracer struct {
	rowTracer
	log *charLog
}

func (c *charTracer) RecordTrace(ctx context.Context, traceID, kind, stage, status, deliveryID, peerID, fromPeerID string, detail map[string]any) error {
	c.log.add("trace %s %s %s", traceID, stage, status)
	return c.rowTracer.RecordTrace(ctx, traceID, kind, stage, status, deliveryID, peerID, fromPeerID, detail)
}

type charTransport struct {
	*fakeTransport
	log *charLog
}

func (c *charTransport) Send(ctx context.Context, id proto.PeerID, v any) error {
	c.log.add("transport.Send(%s)", id)
	return c.fakeTransport.Send(ctx, id, v)
}

func (c *charTransport) SendAndWaitDeliveryAck(ctx context.Context, id proto.PeerID, v any, timeout time.Duration) (map[string]any, error) {
	c.log.add("transport.SendAndWaitDeliveryAck(%s)", id)
	return c.fakeTransport.SendAndWaitDeliveryAck(ctx, id, v, timeout)
}

// charRig is one scenario's hub and fakes.
type charRig struct {
	hub  *Hub
	asks *service.AskTracker
	reg  *charRegistry
	tr   *charTracer
	f    *fakeTransport
	log  *charLog
	arms map[string]*stubArm
	// extra maps random values a scenario's setup created to stable names.
	extra map[string]string
}

// charScenario is one /ask call against one configuration.
type charScenario struct {
	name     string
	cfg      func() *config.RouterConfig // nil: router unwired
	notReady bool                        // no ask lifecycle wired
	setup    func(t *testing.T, r *charRig)
	req      AskRequest
	body     string // raw body instead of req, when set
	cancel   bool   // the request context is already cancelled
}

func charOff() *config.RouterConfig { return nil }
func charDefaults() *config.RouterConfig {
	cfg := config.Defaults().Experiments.Router
	return &cfg
}
func charRecord() *config.RouterConfig { return recordCfg() }
func charRecordNoText() *config.RouterConfig {
	cfg := recordCfg()
	cfg.RecordText = false
	return cfg
}
func charShadow() *config.RouterConfig { return shadowCfg("shadowstub") }
func charLive() *config.RouterConfig   { return liveCfg("stub") }
func charLiveNoRecord() *config.RouterConfig {
	cfg := liveCfg("stub")
	cfg.Record = false
	return cfg
}

var charConfigs = []struct {
	name string
	cfg  func() *config.RouterConfig
}{
	{"off", charOff}, {"defaults", charDefaults}, {"record", charRecord},
	{"record_notext", charRecordNoText}, {"shadow", charShadow},
}

func newCharRig(t *testing.T, sc charScenario) *charRig {
	t.Helper()
	log := &charLog{}
	alpha := peerWith("repow-a", "alpha", "default", proto.StatusOnline)
	beta := peerWith("repow-b", "beta", "default", proto.StatusOnline)
	gamma := peerWith("repow-c", "gamma", "default", proto.StatusOnline)
	other := peerWith("repow-o", "other", "elsewhere", proto.StatusOnline)
	sub := peerWith("repow-s", "sub", "default", proto.StatusOnline)
	twin := peerWith("repow-t", "alpha", "elsewhere", proto.StatusOnline)
	alpha.Path, alpha.Backend, alpha.Description = "/src/alpha", proto.AgentType("claude-code"), "writing the parser"
	beta.Path, beta.Backend, beta.TurnState, beta.Description = "/src/beta", proto.AgentType("codex"), proto.TurnIdle, "reviewing PR 12"
	sub.Provenance = proto.Provenance{Addressable: false, AddressableReason: "sub-agent thread"}
	for _, p := range []*proto.Peer{alpha, beta, gamma, other, twin} {
		p.Provenance = proto.DefaultProvenance()
	}
	tricky := &trickyRegistry{askFakeRegistry: newAskFakeRegistry(alpha, beta, gamma, other, sub, twin), ambiguous: map[string]bool{}, deny: map[string]bool{}}
	reg := &charRegistry{trickyRegistry: tricky, log: log}
	f := &fakeTransport{ackFrame: map[string]any{"status": "accepted"}}
	ct := &charTransport{fakeTransport: f, log: log}
	asks := service.NewAskTracker(0)
	tr := &charTracer{log: log}
	h := &Hub{deliveryTraces: tr}
	arms := map[string]*stubArm{
		"stub":       {name: "stub", scores: []routing.Score{{PeerID: "repow-c", Score: 0.81}, {PeerID: "repow-b", Score: 0.2}}},
		"shadowstub": {name: "shadowstub", scores: []routing.Score{{PeerID: "repow-b", Score: 0.6}}},
	}
	if !sc.notReady {
		h.WithAskLifecycle(asks, service.NewPeerDelivery(reg, service.NewMessageRouter(ct, nil), ct, asks, nil), reg)
	}
	if sc.cfg != nil {
		if cfg := sc.cfg(); cfg != nil {
			router, err := routing.NewRouter(*cfg, map[string]routing.Arm{"stub": arms["stub"], "shadowstub": arms["shadowstub"]})
			if err != nil {
				t.Fatal(err)
			}
			h.WithRouting(router)
		}
	}
	return &charRig{hub: h, asks: asks, reg: reg, tr: tr, f: f, log: log, arms: arms, extra: map[string]string{}}
}

type charTrace struct {
	Trace  string         `json:"trace"`
	Stage  string         `json:"stage"`
	Status string         `json:"status"`
	Peer   string         `json:"peer"`
	From   string         `json:"from"`
	Detail map[string]any `json:"detail"`
}

type charObservation struct {
	Status    int              `json:"status"`
	Body      any              `json:"body"`
	Calls     []string         `json:"calls"`
	Traces    []charTrace      `json:"traces"`
	Tracker   []*service.Ask   `json:"tracker"`
	Frame     any              `json:"frame"`
	Delivered []proto.PeerID   `json:"delivered_to"`
	Events    []map[string]any `json:"events"`
	Offlined  []proto.PeerID   `json:"offlined"`
	ArmCalls  map[string]int   `json:"arm_calls"`
}

// priorAsk is the reply_to target scenarios close. Not hex, so it is never
// normalized.
const priorAsk = "ask-prior"

func runCharScenario(t *testing.T, sc charScenario) (charObservation, map[string]string) {
	t.Helper()
	rig := newCharRig(t, sc)
	if sc.setup != nil {
		sc.setup(t, rig)
	}
	rig.log.add("--- request")
	body := sc.body
	if body == "" {
		raw, err := json.Marshal(sc.req)
		if err != nil {
			t.Fatal(err)
		}
		body = string(raw)
	}
	req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body))
	if sc.cancel {
		ctx, cancel := context.WithCancel(req.Context())
		cancel()
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	rig.hub.handleAsk(rec, req)
	rig.hub.waitShadow()

	obs := charObservation{Status: rec.Code, ArmCalls: map[string]int{}}
	if err := json.Unmarshal(rec.Body.Bytes(), &obs.Body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	rig.log.mu.Lock()
	obs.Calls = append([]string{}, rig.log.lines...)
	rig.log.mu.Unlock()
	cids := map[string]bool{priorAsk: true}
	rig.tr.mu.Lock()
	for _, row := range rig.tr.rows {
		obs.Traces = append(obs.Traces, charTrace{row.traceID, row.stage, row.status, row.peerID, row.fromPeerID, row.detail})
		cids[row.traceID] = true
	}
	rig.tr.mu.Unlock()
	if m, ok := obs.Body.(map[string]any); ok {
		if cid, ok := m["correlation_id"].(string); ok {
			cids[cid] = true
		}
	}
	var keys []string
	for cid := range cids {
		keys = append(keys, cid)
	}
	sort.Strings(keys)
	for _, cid := range keys {
		if a, ok := rig.asks.Get(cid); ok {
			obs.Tracker = append(obs.Tracker, a)
		}
	}
	rig.f.mu.Lock()
	obs.Frame, obs.Delivered = rig.f.lastFrame, append([]proto.PeerID{}, rig.f.sentTargets...)
	if rig.f.lastTarget != "" && len(rig.f.sentTargets) == 0 {
		obs.Delivered = append(obs.Delivered, rig.f.lastTarget)
	}
	rig.f.mu.Unlock()
	fake := rig.reg.askFakeRegistry.fakeRegistry
	fake.mu.Lock()
	for _, e := range fake.events {
		obs.Events = append(obs.Events, map[string]any{"type": e.typ, "payload": e.payload})
	}
	obs.Offlined = fake.offlined
	fake.mu.Unlock()
	for name, arm := range rig.arms {
		obs.ArmCalls[name] = arm.calls
	}
	return obs, rig.extra
}

var (
	charCID       = regexp.MustCompile(`ask-[0-9a-f]{8}`)
	charDelivery  = regexp.MustCompile(`ask-delivery-[0-9a-f]{8}`)
	charTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
)

// charVolatile reports keys whose values depend on the wall clock.
func charVolatile(key string) bool {
	return strings.HasSuffix(key, "_at") || strings.HasSuffix(key, "_age_s") || strings.HasSuffix(key, "_ms") ||
		key == "last_seen" || strings.HasSuffix(key, "At")
}

// normalizeChar renders obs as stable JSON: cids are numbered by first
// appearance, timestamps and clock-derived values are masked. Nothing else
// is touched.
func normalizeChar(t *testing.T, obs charObservation, extra map[string]string) []byte {
	t.Helper()
	raw, err := json.Marshal(obs)
	if err != nil {
		t.Fatal(err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				if charVolatile(k) && val != nil {
					x[k] = "<clock>"
					continue
				}
				x[k] = walk(val)
			}
			return x
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
			return x
		case string:
			for from, to := range extra {
				x = strings.ReplaceAll(x, from, to)
			}
			x = charTimestamp.ReplaceAllString(x, "<ts>")
			x = charDelivery.ReplaceAllString(x, "<delivery-id>")
			return charCID.ReplaceAllStringFunc(x, func(cid string) string {
				if _, ok := ids[cid]; !ok {
					ids[cid] = fmt.Sprintf("<cid%d>", len(ids)+1)
				}
				return ids[cid]
			})
		}
		return v
	}
	// Number cids in a fixed order: calls first, then the rest of the tree.
	m := tree.(map[string]any)
	m["calls"] = walk(m["calls"])
	tree = walk(m)
	out, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func charScenarios() []charScenario {
	var out []charScenario
	ok := AskRequest{FromPeer: "alpha", ToPeer: "beta", Text: "who owns the lexer?"}
	perConfig := []struct {
		name  string
		req   AskRequest
		setup func(t *testing.T, r *charRig)
	}{
		{"ok", ok, nil},
		{"ok_by_peer_id", AskRequest{FromPeer: "repow-a", ToPeer: "repow-b", Text: "by id"}, nil},
		{"bad_reply_delivery", AskRequest{FromPeer: "alpha", ToPeer: "beta", Text: "x", ReplyDelivery: "carrier-pigeon"}, nil},
		{"unknown_target", AskRequest{FromPeer: "alpha", ToPeer: "nobody", Text: "x"}, nil},
		{"empty_target", AskRequest{FromPeer: "alpha", Text: "x"}, nil},
		{"route_target_not_live", AskRequest{FromPeer: "alpha", ToPeer: " @Route ", Text: "x"}, nil},
		{"ambiguous_target", AskRequest{FromPeer: "alpha", ToPeer: "gamma", Text: "x"}, func(_ *testing.T, r *charRig) { r.reg.ambiguous["gamma"] = true }},
		{"access_denied", AskRequest{FromPeer: "alpha", ToPeer: "beta", Text: "x"}, func(_ *testing.T, r *charRig) { r.reg.deny["repow-b"] = true }},
		{"cross_circle", AskRequest{FromPeer: "alpha", ToPeer: "other", Text: "x"}, nil},
		{"cross_circle_bypass", AskRequest{FromPeer: "alpha", ToPeer: "other", Text: "x", BypassCircle: true}, nil},
		{"not_addressable", AskRequest{FromPeer: "alpha", ToPeer: "sub", Text: "x"}, nil},
		{"self", AskRequest{FromPeer: "alpha", ToPeer: "alpha", Text: "x", Circle: ptr("default")}, nil},
		{"unresolved_sender", AskRequest{FromPeer: "ghost", ToPeer: "beta", Text: "x"}, nil},
		{"sender_name_collision_scoped", AskRequest{FromPeer: "alpha", ToPeer: "beta", Text: "x", Circle: ptr("default")}, nil},
		{"sender_ambiguous_unscoped", AskRequest{FromPeer: "alpha", ToPeer: "beta", Text: "x"}, func(_ *testing.T, r *charRig) { r.reg.ambiguous["alpha"] = true }},
		{"quiesced_target", ok, func(t *testing.T, r *charRig) {
			if err := r.asks.BeginQuiesce(context.Background(), "repow-b"); err != nil {
				t.Fatal(err)
			}
		}},
		{"injection_failed", ok, func(_ *testing.T, r *charRig) {
			r.f.ackFrame = map[string]any{"status": "rejected", "detail": "pane not safe"}
		}},
		{"no_connection", ok, func(_ *testing.T, r *charRig) { r.f.ackErr = service.ErrNotConnected }},
		{"delivery_error", ok, func(_ *testing.T, r *charRig) { r.f.ackErr = errors.New("socket closed mid-frame") }},
		{"deferred_busy_target", ok, func(_ *testing.T, r *charRig) {
			r.reg.lookup("beta").Status = proto.StatusBusy
		}},
		{"pull_question_attachments_reply_to", AskRequest{
			FromPeer: "alpha", ToPeer: "beta", Text: "pick one", ReplyDelivery: "pull", ReplyTo: ptr(priorAsk),
			Question:    map[string]any{"prompt": "which?", "options": []any{"a", "b"}},
			Attachments: []map[string]any{{"kind": "file", "path": "notes.txt"}},
		}, registerPrior},
		{"reply_to_kept_when_delivery_fails", AskRequest{FromPeer: "alpha", ToPeer: "beta", Text: "x", ReplyTo: ptr(priorAsk)},
			func(t *testing.T, r *charRig) {
				registerPrior(t, r)
				r.f.ackErr = errors.New("socket closed mid-frame")
			}},
		{"session_bindings", ok, bindSessions},
		{"circle_scoped_target", AskRequest{FromPeer: "alpha", ToPeer: "beta", Text: "x", Circle: ptr("default")}, nil},
	}
	for _, c := range charConfigs {
		for _, p := range perConfig {
			out = append(out, charScenario{name: c.name + "/" + p.name, cfg: c.cfg, req: p.req, setup: p.setup})
		}
	}
	live := []charScenario{
		{name: "pick", req: AskRequest{FromPeer: "alpha", ToPeer: "@route", Text: "who owns the lexer?"}},
		{name: "pick_with_fallback_unused", req: AskRequest{FromPeer: "alpha", ToPeer: "@route", FallbackTo: "beta", Text: "x"}},
		{name: "arm_error_fallback", req: AskRequest{FromPeer: "alpha", ToPeer: "@route", FallbackTo: "beta", Text: "x"},
			setup: func(_ *testing.T, r *charRig) { r.arms["stub"].err = errors.New("boom") }},
		{name: "arm_error_no_fallback", req: AskRequest{FromPeer: "alpha", ToPeer: "@route", Text: "x"},
			setup: func(_ *testing.T, r *charRig) { r.arms["stub"].err = errors.New("boom") }},
		{name: "ineligible_pick_fallback", req: AskRequest{FromPeer: "alpha", ToPeer: "@route", FallbackTo: "beta", Text: "x"},
			setup: func(_ *testing.T, r *charRig) { r.arms["stub"].scores = []routing.Score{{PeerID: "repow-o", Score: 1}} }},
		{name: "stale_pick_fallback", req: AskRequest{FromPeer: "alpha", ToPeer: "@route", FallbackTo: "beta", Text: "x"},
			setup: func(_ *testing.T, r *charRig) { r.reg.deny["repow-c"] = true }},
		{name: "stale_pick_no_fallback", req: AskRequest{FromPeer: "alpha", ToPeer: "@route", Text: "x"},
			setup: func(_ *testing.T, r *charRig) { r.reg.deny["repow-c"] = true }},
		{name: "stale_pick_denied_fallback", req: AskRequest{FromPeer: "alpha", ToPeer: "@route", FallbackTo: "beta", Text: "x"},
			setup: func(_ *testing.T, r *charRig) { r.reg.deny["repow-c"], r.reg.deny["repow-b"] = true, true }},
		{name: "fallback_unknown", req: AskRequest{FromPeer: "alpha", ToPeer: "@route", FallbackTo: "nobody", Text: "x"},
			setup: func(_ *testing.T, r *charRig) { r.arms["stub"].err = errors.New("boom") }},
		{name: "named_target_is_explicit", req: ok},
		{name: "route_target_case_and_space", req: AskRequest{FromPeer: "alpha", ToPeer: "  @ROUTE ", Text: "x"}},
		{name: "quiesced_pick", req: AskRequest{FromPeer: "alpha", ToPeer: "@route", Text: "x"},
			setup: func(t *testing.T, r *charRig) {
				if err := r.asks.BeginQuiesce(context.Background(), "repow-c"); err != nil {
					t.Fatal(err)
				}
			}},
	}
	for _, sc := range live {
		for _, c := range []struct {
			name string
			cfg  func() *config.RouterConfig
		}{{"live", charLive}, {"live_norecord", charLiveNoRecord}} {
			s := sc
			s.name, s.cfg = c.name+"/"+sc.name, c.cfg
			out = append(out, s)
		}
	}
	out = append(out,
		charScenario{name: "not_ready", notReady: true, req: ok},
		charScenario{name: "off/malformed_body", body: `{"to_peer": `},
		charScenario{name: "off/cancelled_request", cfg: charOff, req: ok, cancel: true},
		charScenario{name: "record/cancelled_request", cfg: charRecord, req: ok, cancel: true},
		charScenario{name: "shadow/cancelled_request", cfg: charShadow, req: ok, cancel: true},
		charScenario{name: "off/null_target", cfg: charOff, body: `{"from_peer":"alpha","to_peer":null,"text":"x"}`},
	)
	return out
}

func registerPrior(t *testing.T, r *charRig) {
	t.Helper()
	if _, err := r.asks.Register(context.Background(), service.RegisterAskParams{
		CorrelationID: priorAsk, FromPeerID: "repow-b", FromPeerName: "beta", ToPeerID: "repow-a", ToPeerName: "alpha", Text: "earlier",
	}); err != nil {
		t.Fatal(err)
	}
}

// bindSessions gives alpha and beta repowire sessions in a real state db.
func bindSessions(t *testing.T, r *charRig) {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, id := range []string{"repow-a", "repow-b"} {
		peerID := id
		b, err := store.UpsertObservation(context.Background(), state.Observation{PeerID: &peerID, Backend: "claude-code", Status: state.BindingActive})
		if err != nil {
			t.Fatal(err)
		}
		r.extra[b.RepowireSessionID] = "<session-" + id + ">"
	}
	r.hub.store = store
}

func TestAskCharacterization(t *testing.T) {
	dir := filepath.Join("testdata", "ask_characterization")
	seen := map[string]bool{}
	for _, sc := range charScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			obs, extra := runCharScenario(t, sc)
			got := normalizeChar(t, obs, extra)
			file := filepath.Join(dir, strings.ReplaceAll(sc.name, "/", "__")+".json")
			seen[filepath.Base(file)] = true
			if *updateCharacterization {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("missing golden %s (run with -update-characterization): %v", file, err)
			}
			if string(got) != string(want) {
				t.Fatalf("/ask behaviour changed for %s:\n%s", sc.name, charDiff(string(want), string(got)))
			}
		})
	}
	// A golden with no scenario is a deleted case, not a passing one.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !seen[e.Name()] && !*updateCharacterization {
			t.Errorf("golden %s has no scenario", e.Name())
		}
	}
}

// charDiff shows the first differing lines.
func charDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("line %d\n- %s\n+ %s", i+1, wl, gl)
		}
	}
	return "(identical lines, different bytes)"
}
