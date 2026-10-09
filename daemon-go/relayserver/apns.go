package relayserver

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	apnsProduction   = "https://api.push.apple.com"
	apnsSandbox      = "https://api.sandbox.push.apple.com"
	apnsJWTLifetime  = 50 * time.Minute // Apple rejects tokens older than an hour.
	maxPushDevices   = 10
	pushesPerMinute  = 30
	pushWindowLength = time.Minute
)

// apnsSender sends alert pushes with token-based (.p8) APNs auth. The relay owns
// the key; daemons name the device tokens in each push frame.
type apnsSender struct {
	teamID, keyID, topic string
	key                  *ecdsa.PrivateKey
	hosts                map[string]string
	client               *http.Client

	mu     sync.Mutex
	jwt    string
	jwtAt  time.Time
	window map[string]pushWindow
}

type pushWindow struct {
	start time.Time
	count int
}

// apnsFromEnv returns nil when push is not configured on this relay.
func apnsFromEnv() (*apnsSender, error) {
	team, keyID, topic := os.Getenv("REPOWIRE_RELAY_APNS_TEAM_ID"), os.Getenv("REPOWIRE_RELAY_APNS_KEY_ID"), os.Getenv("REPOWIRE_RELAY_APNS_TOPIC")
	keyPath := os.Getenv("REPOWIRE_RELAY_APNS_KEY_PATH")
	if team == "" && keyID == "" && topic == "" && keyPath == "" {
		return nil, nil
	}
	if team == "" || keyID == "" || topic == "" || keyPath == "" {
		return nil, errors.New("APNs needs REPOWIRE_RELAY_APNS_TEAM_ID, _KEY_ID, _TOPIC and _KEY_PATH together")
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read APNs key: %w", err)
	}
	return newAPNsSender(team, keyID, topic, raw)
}

func newAPNsSender(teamID, keyID, topic string, pemKey []byte) (*apnsSender, error) {
	block, _ := pem.Decode(pemKey)
	if block == nil {
		return nil, errors.New("APNs key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse APNs key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("APNs key is not an EC private key")
	}
	return &apnsSender{
		teamID: teamID, keyID: keyID, topic: topic, key: key,
		hosts:  map[string]string{"production": apnsProduction, "sandbox": apnsSandbox},
		client: &http.Client{Timeout: 15 * time.Second},
		window: map[string]pushWindow{},
	}, nil
}

// allow is a fixed-window limit per relay user so one key cannot flood APNs.
func (a *apnsSender) allow(userID string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.window[userID]
	if now.Sub(w.start) >= pushWindowLength {
		w = pushWindow{start: now}
	}
	if w.count >= pushesPerMinute {
		return false
	}
	w.count++
	a.window[userID] = w
	return true
}

func (a *apnsSender) bearer(now time.Time) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jwt != "" && now.Sub(a.jwtAt) < apnsJWTLifetime {
		return a.jwt, nil
	}
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": a.keyID})
	claims, _ := json.Marshal(map[string]any{"iss": a.teamID, "iat": now.Unix()})
	unsigned := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, a.key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	a.jwt, a.jwtAt = unsigned+"."+enc.EncodeToString(sig), now
	return a.jwt, nil
}

// pushFrame is the daemon's push message (see daemon-go/push).
type pushFrame struct {
	PushID   string         `json:"push_id"`
	Title    string         `json:"title"`
	Body     string         `json:"body"`
	ThreadID string         `json:"thread_id"`
	Category string         `json:"category"`
	Data     map[string]any `json:"data"`
	Devices  []struct {
		Token       string `json:"token"`
		Environment string `json:"environment"`
	} `json:"devices"`
}

// send delivers one frame and returns the tokens APNs says are dead.
func (a *apnsSender) send(ctx context.Context, frame pushFrame) ([]string, error) {
	if len(frame.Devices) > maxPushDevices {
		frame.Devices = frame.Devices[:maxPushDevices]
	}
	payload, err := json.Marshal(map[string]any{
		"aps": map[string]any{
			"alert":     map[string]string{"title": frame.Title, "body": frame.Body},
			"sound":     "default",
			"thread-id": frame.ThreadID,
			"category":  frame.Category,
		},
		"repowire": frame.Data,
	})
	if err != nil {
		return nil, err
	}
	bearer, err := a.bearer(time.Now())
	if err != nil {
		return nil, err
	}
	var invalid []string
	var errs []error
	for _, device := range frame.Devices {
		host, ok := a.hosts[device.Environment]
		if !ok || !validDeviceToken(device.Token) {
			invalid = append(invalid, device.Token)
			continue
		}
		dead, err := a.post(ctx, host, device.Token, bearer, frame.PushID, payload)
		if dead {
			invalid = append(invalid, device.Token)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return invalid, errors.Join(errs...)
}

func (a *apnsSender) post(ctx context.Context, host, token, bearer, pushID string, payload []byte) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/3/device/"+token, bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("authorization", "bearer "+bearer)
	req.Header.Set("apns-topic", a.topic)
	req.Header.Set("apns-push-type", "alert")
	if collapse := strings.TrimSpace(pushID); collapse != "" && len(collapse) <= 64 {
		req.Header.Set("apns-collapse-id", collapse)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return false, nil
	}
	var reason struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&reason)
	switch reason.Reason {
	case "BadDeviceToken", "Unregistered", "DeviceTokenNotForTopic":
		return true, nil
	}
	return false, fmt.Errorf("APNs %d %s", resp.StatusCode, reason.Reason)
}

func validDeviceToken(token string) bool {
	if len(token) < 32 || len(token) > 200 {
		return false
	}
	_, ok := new(big.Int).SetString(token, 16)
	return ok
}
