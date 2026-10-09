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
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
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
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	conn := connectDaemon(t, server.URL, "rw_test-push-key")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "push", "push_id": "evt-1", "title": "@backend asks", "body": "ship it?", "thread_id": "backend", "category": "QUESTION",
		"data":    map[string]any{"correlation_id": "ask-1"},
		"devices": []map[string]any{{"token": liveToken, "environment": "sandbox"}, {"token": deadToken, "environment": "production"}},
	}); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := wsjson.Read(ctx, conn, &result); err != nil {
		t.Fatal(err)
	}
	if result["type"] != "push_result" || result["error"] != nil {
		t.Fatalf("result = %v", result)
	}
	if invalid := result["invalid_tokens"].([]any); len(invalid) != 1 || invalid[0] != deadToken {
		t.Fatalf("invalid tokens = %v", invalid)
	}
	first := <-seen
	if first.Header.Get("apns-topic") != "io.repowire.app" || first.Header.Get("apns-push-type") != "alert" || first.ProtoMajor != 2 {
		t.Fatalf("apns request headers = %v proto=%d", first.Header, first.ProtoMajor)
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
