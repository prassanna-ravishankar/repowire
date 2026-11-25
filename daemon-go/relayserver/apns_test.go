package relayserver

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/push"
	"github.com/repowire/repowire/daemon-go/relay"
	"github.com/repowire/repowire/daemon-go/state"
)

const (
	liveToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	deadToken = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func testAPNs(t *testing.T) (*apnsSender, chan *http.Request) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	sender, err := newAPNsSender("TEAM123456", "KEY1234567", "io.repowire.app", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan *http.Request, 4)
	apple := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !verifyJWT(&key.PublicKey, strings.TrimPrefix(r.Header.Get("authorization"), "bearer ")) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"reason":"InvalidProviderToken"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		seen <- r
		if strings.HasSuffix(r.URL.Path, deadToken) {
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"reason":"Unregistered"}`))
		}
	}))
	apple.EnableHTTP2 = true
	apple.StartTLS()
	t.Cleanup(apple.Close)
	sender.client = apple.Client()
	sender.hosts = map[string]string{"sandbox": apple.URL, "production": apple.URL}
	return sender, seen
}

func verifyJWT(pub *ecdsa.PublicKey, token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}

func TestRelayPushSendsToAPNsAndReportsDeadTokens(t *testing.T) {
	relay := New("")
	sender, seen := testAPNs(t)
	relay.apns = sender
	bindToken(t, relay, "rw_test-push-key", liveToken, deadToken)
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	conn := connectDaemon(t, server.URL, "rw_test-push-key")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "push", "id": "req-1", "push_id": "evt-1", "title": "@backend asks", "body": "ship it?", "thread_id": "backend", "category": "QUESTION",
		"data":    map[string]any{"correlation_id": "ask-1"},
		"devices": []map[string]any{{"token": liveToken, "environment": "sandbox"}, {"token": deadToken, "environment": "production"}},
	}); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := wsjson.Read(ctx, conn, &result); err != nil {
		t.Fatal(err)
	}
	if result["type"] != "push_result" || result["id"] != "req-1" || result["error"] != nil {
		t.Fatalf("result = %v", result)
	}
	if invalid := result["invalid_tokens"].([]any); len(invalid) != 1 || invalid[0] != deadToken {
		t.Fatalf("invalid tokens = %v", invalid)
	}
	first := <-seen
	if first.Header.Get("apns-topic") != "io.repowire.app" || first.Header.Get("apns-push-type") != "alert" || first.ProtoMajor != 2 {
		t.Fatalf("apns request headers = %v proto=%d", first.Header, first.ProtoMajor)
	}
	if relay.pushOwners.owner(deadToken) != "" {
		t.Fatal("a dead token should lose its binding")
	}
}

// bindToken records tokens as registered by key's user, as a tunneled
// POST /push/devices would.
func bindToken(t *testing.T, s *Server, key string, tokens ...string) {
	t.Helper()
	apiKey, ok := s.tokens.validate(key)
	if !ok {
		t.Fatalf("key %q not valid", key)
	}
	for _, token := range tokens {
		s.pushOwners.bind(token, apiKey.UserID, time.Now())
	}
}

// One tenant's daemon cannot put an alert on a phone another tenant
// registered (review of #607): the relay sends only to tokens its own user
// registered and reports the rest as unbound.
func TestRelayPushSkipsTokensAnotherUserRegistered(t *testing.T) {
	relay := New("")
	sender, seen := testAPNs(t)
	relay.apns = sender
	bindToken(t, relay, "rw_victim-key-with-entropy", liveToken)
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	conn := connectDaemon(t, server.URL, "rw_attacker-key-with-entropy")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = wsjson.Write(ctx, conn, map[string]any{
		"type": "push", "id": "req-1", "title": "@backend needs approval", "category": "APPROVAL",
		"devices": []map[string]any{{"token": liveToken, "environment": "production"}},
	})
	var result map[string]any
	if err := wsjson.Read(ctx, conn, &result); err != nil {
		t.Fatal(err)
	}
	unbound, _ := result["unbound_tokens"].([]any)
	if len(unbound) != 1 || unbound[0] != liveToken || !strings.Contains(stringValue(result["error"]), "no device token") {
		t.Fatalf("result = %v", result)
	}
	select {
	case req := <-seen:
		t.Fatalf("APNs was called for another user's token: %v", req.URL)
	default:
	}
}

// A rate-limited daemon is refused from the read loop, before any work is
// queued (review of #607).
func TestRelayPushRateLimitIsCheckedInline(t *testing.T) {
	relay := New("")
	sender, seen := testAPNs(t)
	relay.apns = sender
	key := "rw_test-push-flood"
	bindToken(t, relay, key, liveToken)
	apiKey, _ := relay.tokens.validate(key)
	for range pushesPerMinute {
		sender.allow(apiKey.UserID, time.Now())
	}
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	conn := connectDaemon(t, server.URL, key)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = wsjson.Write(ctx, conn, map[string]any{"type": "push", "id": "req-9", "devices": []map[string]any{{"token": liveToken, "environment": "production"}}})
	var result map[string]any
	if err := wsjson.Read(ctx, conn, &result); err != nil {
		t.Fatal(err)
	}
	if result["id"] != "req-9" || !strings.Contains(stringValue(result["error"]), "rate limit") {
		t.Fatalf("result = %v", result)
	}
	select {
	case <-seen:
		t.Fatal("a rate-limited push reached APNs")
	default:
	}
}

// phone is one relay user: a daemon that accepts every tunneled request, and
// the app calls that user makes through the relay.
type phone struct {
	t      *testing.T
	server string
	key    string
	status chan int
}

func newPhone(t *testing.T, s *Server, serverURL, key, daemonID string) *phone {
	t.Helper()
	conn := connectDaemonAs(t, serverURL, key, daemonID)
	p := &phone{t: t, server: serverURL, key: key, status: make(chan int, 4)}
	go func() {
		for {
			var request map[string]any
			if wsjson.Read(context.Background(), conn, &request) != nil {
				return
			}
			if request["type"] != "http_request" {
				continue
			}
			_ = wsjson.Write(context.Background(), conn, map[string]any{
				"type": "http_response", "request_id": request["request_id"], "status": <-p.status,
				"headers": map[string]string{"Content-Type": "application/json"}, "body": base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`)),
			})
		}
	}()
	return p
}

// call makes an app request through the relay; daemonStatus is what the
// daemon answers when the request is tunneled (0 for relay-owned routes).
func (p *phone) call(method, path, body string, daemonStatus int) int {
	p.t.Helper()
	if daemonStatus != 0 {
		p.status <- daemonStatus
	}
	req, _ := http.NewRequest(method, p.server+path, strings.NewReader(body))
	req.Header.Set("Authorization", "bearer "+p.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// nonceFrom reads the challenge a background push delivered to the device.
func nonceFrom(t *testing.T, seen chan *http.Request, token string) string {
	t.Helper()
	select {
	case req := <-seen:
		if req.Header.Get("apns-push-type") != "background" || req.Header.Get("apns-priority") != "5" || !strings.HasSuffix(req.URL.Path, token) {
			t.Fatalf("challenge push = %s %v", req.URL.Path, req.Header)
		}
		var payload struct {
			APS      map[string]any    `json:"aps"`
			Repowire map[string]string `json:"repowire"`
		}
		_ = json.NewDecoder(req.Body).Decode(&payload)
		if payload.APS["alert"] != nil || payload.Repowire["verify"] == "" {
			t.Fatalf("challenge payload = %+v", payload)
		}
		return payload.Repowire["verify"]
	case <-time.After(5 * time.Second):
		t.Fatal("no challenge push reached the device")
	}
	return ""
}

// A registration alone never binds: the relay sends a background challenge to
// the token, and only the echo of its nonce, under the registering key, binds.
// A tunneled delete unbinds; a registration the daemon refused does nothing.
func TestTokenBindsOnlyAfterTheDeviceProvesPossession(t *testing.T) {
	relay := New("")
	sender, seen := testAPNs(t)
	relay.apns = sender
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	key := "rw_test-push-register"
	p := newPhone(t, relay, server.URL, key, "laptop")
	apiKey, _ := relay.tokens.validate(key)

	if p.call(http.MethodPost, "/push/devices", `{"token":"`+deadToken+`"}`, http.StatusBadRequest) != http.StatusBadRequest {
		t.Fatal("refused registration status")
	}
	p.call(http.MethodPost, "/push/devices", `{"token":"`+strings.ToUpper(liveToken)+`","environment":"sandbox"}`, http.StatusOK)
	nonce := nonceFrom(t, seen, liveToken)
	if relay.pushOwners.owner(liveToken) != "" {
		t.Fatal("a registration must not bind before the device answers its challenge")
	}
	if p.call(http.MethodPost, "/push/devices/verify", `{"token":"`+liveToken+`","nonce":"pv_wrong"}`, 0) != http.StatusForbidden {
		t.Fatal("a wrong nonce must be refused")
	}
	if p.call(http.MethodPost, "/push/devices/verify", `{"token":"`+liveToken+`","nonce":"`+nonce+`"}`, 0) != http.StatusOK {
		t.Fatal("the device's nonce should verify")
	}
	if relay.pushOwners.owner(liveToken) != apiKey.UserID {
		t.Fatalf("owner = %q, want %q", relay.pushOwners.owner(liveToken), apiKey.UserID)
	}
	if p.call(http.MethodPost, "/push/devices/verify", `{"token":"`+liveToken+`","nonce":"`+nonce+`"}`, 0) != http.StatusForbidden {
		t.Fatal("a nonce is single-use")
	}
	p.call(http.MethodDelete, "/push/devices/"+liveToken, "", http.StatusOK)
	if relay.pushOwners.owner(liveToken) != "" {
		t.Fatal("a tunneled delete should unbind")
	}
	if relay.pushOwners.owner(deadToken) != "" {
		t.Fatal("a refused registration must not bind")
	}
}

// Review of #607: tenant B registers tenant A's token through B's own daemon.
// The challenge goes to A's phone, which B cannot read, and A's app echoing it
// under A's key does not bind it to B. A keeps the binding, A's push still
// reaches the phone, and B's push is refused.
func TestAnotherTenantCannotStealAVerifiedToken(t *testing.T) {
	relay := New("")
	sender, seen := testAPNs(t)
	relay.apns = sender
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	victim := newPhone(t, relay, server.URL, "rw_victim-key-with-entropy", "victim-laptop")
	attacker := newPhone(t, relay, server.URL, "rw_attacker-key-with-entropy", "attacker-laptop")
	victimKey, _ := relay.tokens.validate(victim.key)

	victim.call(http.MethodPost, "/push/devices", `{"token":"`+liveToken+`","environment":"sandbox"}`, http.StatusOK)
	victim.call(http.MethodPost, "/push/devices/verify", `{"token":"`+liveToken+`","nonce":"`+nonceFrom(t, seen, liveToken)+`"}`, 0)

	attacker.call(http.MethodPost, "/push/devices", `{"token":"`+liveToken+`","environment":"sandbox"}`, http.StatusOK)
	stolen := nonceFrom(t, seen, liveToken) // lands on the victim's phone
	if attacker.call(http.MethodPost, "/push/devices/verify", `{"token":"`+liveToken+`","nonce":"pv_guess"}`, 0) != http.StatusForbidden {
		t.Fatal("the attacker cannot verify without the nonce")
	}
	if victim.call(http.MethodPost, "/push/devices/verify", `{"token":"`+liveToken+`","nonce":"`+stolen+`"}`, 0) != http.StatusForbidden {
		t.Fatal("the victim's app echoing the attacker's challenge must not bind the token to the attacker")
	}
	if relay.pushOwners.owner(liveToken) != victimKey.UserID {
		t.Fatalf("owner = %q, want the victim", relay.pushOwners.owner(liveToken))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tc := range []struct {
		key, daemon string
		delivered   bool
	}{{victim.key, "victim-push", true}, {attacker.key, "attacker-push", false}} {
		conn := connectDaemonAs(t, server.URL, tc.key, tc.daemon)
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "push", "id": tc.daemon, "title": "@backend needs approval",
			"devices": []map[string]any{{"token": liveToken, "environment": "sandbox"}}})
		var result map[string]any
		if err := wsjson.Read(ctx, conn, &result); err != nil {
			t.Fatal(err)
		}
		if got := result["error"] == nil; got != tc.delivered {
			t.Fatalf("%s: result = %v", tc.daemon, result)
		}
		if tc.delivered {
			if req := <-seen; req.Header.Get("apns-push-type") != "alert" {
				t.Fatalf("victim push type = %s", req.Header.Get("apns-push-type"))
			}
		}
	}
	select {
	case req := <-seen:
		t.Fatalf("APNs saw an extra request: %s %s", req.Header.Get("apns-push-type"), req.URL.Path)
	default:
	}
}

// Review of #607: before the victim's phone verifies, an attacker registering
// the same token (again and again) must not replace the victim's challenge.
// The victim's nonce still binds the token to the victim.
func TestAnotherTenantCannotBlockAPendingVerification(t *testing.T) {
	relay := New("")
	sender, seen := testAPNs(t)
	relay.apns = sender
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	victim := newPhone(t, relay, server.URL, "rw_victim-key-with-entropy", "victim-laptop")
	attacker := newPhone(t, relay, server.URL, "rw_attacker-key-with-entropy", "attacker-laptop")
	victimKey, _ := relay.tokens.validate(victim.key)

	victim.call(http.MethodPost, "/push/devices", `{"token":"`+liveToken+`","environment":"sandbox"}`, http.StatusOK)
	victimNonce := nonceFrom(t, seen, liveToken)
	for range 3 {
		attacker.call(http.MethodPost, "/push/devices", `{"token":"`+liveToken+`","environment":"sandbox"}`, http.StatusOK)
		_ = nonceFrom(t, seen, liveToken)
	}
	if victim.call(http.MethodPost, "/push/devices/verify", `{"token":"`+liveToken+`","nonce":"`+victimNonce+`"}`, 0) != http.StatusOK {
		t.Fatal("the attacker's registrations replaced the victim's challenge")
	}
	if relay.pushOwners.owner(liveToken) != victimKey.UserID {
		t.Fatalf("owner = %q, want the victim", relay.pushOwners.owner(liveToken))
	}
}

func TestChallengesAreCappedPerUser(t *testing.T) {
	b := newPushBindings()
	now := time.Now()
	for i := range maxChallengesPerUser {
		if _, ok := b.open(fmt.Sprintf("%064x", i), "u1", now); !ok {
			t.Fatalf("challenge %d refused under the cap", i)
		}
	}
	if _, ok := b.open(fmt.Sprintf("%064x", 999), "u1", now); ok {
		t.Fatal("a user past the cap must be refused")
	}
	if _, ok := b.open(fmt.Sprintf("%064x", 0), "u2", now); !ok {
		t.Fatal("another user is not limited by u1's challenges")
	}
	if _, ok := b.open(fmt.Sprintf("%064x", 999), "u1", now.Add(challengeTTL+time.Second)); !ok {
		t.Fatal("expired challenges free the user's share")
	}
}

func TestPushBindingsPersistAndCapPerUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.db")
	b, err := openPushBindings(path)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := range maxTokensPerUser + 2 {
		b.bind(strings.Repeat(string(rune('a'+i%6)), 63)+string(rune('0'+i%10)), "u1", start.Add(time.Duration(i)*time.Second))
	}
	b.bind(liveToken, "u2", start)
	b.unbind(liveToken, "u1") // not u1's: no effect
	b.close()

	reopened, err := openPushBindings(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if reopened.owner(liveToken) != "u2" {
		t.Fatal("binding did not survive a restart")
	}
	count := 0
	for _, o := range reopened.owners {
		if o.user == "u1" {
			count++
		}
	}
	if count != maxTokensPerUser {
		t.Fatalf("u1 holds %d tokens, want the cap %d", count, maxTokensPerUser)
	}
}

func TestRelayPushUnconfiguredAndRateLimited(t *testing.T) {
	relay := New("")
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	conn := connectDaemon(t, server.URL, "rw_test-push-off")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "push", "push_id": "x"})
	var result map[string]any
	if err := wsjson.Read(ctx, conn, &result); err != nil || !strings.Contains(result["error"].(string), "not configured") {
		t.Fatalf("result = %v err = %v", result, err)
	}

	sender, _ := testAPNs(t)
	now := time.Now()
	for i := 0; i < pushesPerMinute; i++ {
		if !sender.allow("u", now) {
			t.Fatalf("push %d unexpectedly limited", i)
		}
	}
	if sender.allow("u", now) || !sender.allow("other", now) || !sender.allow("u", now.Add(pushWindowLength)) {
		t.Fatal("rate limit window misbehaves")
	}
}

func TestAPNsPayloadShape(t *testing.T) {
	sender, seen := testAPNs(t)
	frame := pushFrame{PushID: "p", Title: "t", Body: "b", ThreadID: "peer", Category: "APPROVAL", Data: map[string]any{"correlation_id": "c"}}
	frame.Devices = append(frame.Devices, struct {
		Token       string `json:"token"`
		Environment string `json:"environment"`
	}{liveToken, "sandbox"})
	if _, err := sender.send(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.NewDecoder((<-seen).Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	aps := payload["aps"].(map[string]any)
	if aps["category"] != "APPROVAL" || aps["thread-id"] != "peer" || payload["repowire"].(map[string]any)["correlation_id"] != "c" {
		t.Fatalf("payload = %v", payload)
	}
}

// The daemon's ios inbox surface pushes through a real relay client and gets
// APNs' answer back as its receipt; a dead token leaves the device store.
func TestInboxSurfacePushRoundTripsThroughTheRelay(t *testing.T) {
	relayServer := New("")
	sender, seen := testAPNs(t)
	relayServer.apns = sender
	server := httptest.NewServer(relayServer.Handler())
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, err := state.NewStore(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, d := range []state.PushDevice{{Token: liveToken, Environment: "sandbox"}, {Token: deadToken, Environment: "production"}} {
		if err := store.UpsertPushDevice(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	key := "rw_test-push-round-trip"
	client := relay.NewClient("ws"+strings.TrimPrefix(server.URL, "http"), key, "laptop", "http://127.0.0.1:1")
	client.Start(ctx)
	t.Cleanup(client.Stop)
	waitForDaemon(t, server.URL, key, "laptop")

	bindToken(t, relayServer, key, liveToken, deadToken)
	surface := push.NewSurface(store, client, true)
	if !surface.Live() {
		t.Fatal("surface with a registered device and a connected relay should be live")
	}
	item := &state.InboxItem{ID: "inb-1", Kind: "ask", FromName: "backend", CorrelationID: "ask-1", Text: "ship it?"}
	if err := surface.Push(ctx, item); err != nil {
		t.Fatalf("push = %v", err)
	}
	first := <-seen
	if first.Header.Get("apns-collapse-id") != "inb-1" {
		t.Fatalf("collapse id = %q", first.Header.Get("apns-collapse-id"))
	}
	devices, _ := store.ListPushDevices(ctx)
	if len(devices) != 1 || devices[0].Token != liveToken {
		t.Fatalf("devices after push = %+v", devices)
	}
}
