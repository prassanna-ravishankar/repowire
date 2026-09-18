package codexbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// Fixtures are real thread/read payloads captured from the Codex App Server
// (paths sanitized). They pin the field names the classifier depends on.
func loadThreadFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var thread map[string]any
	if err := json.Unmarshal(raw, &thread); err != nil {
		t.Fatal(err)
	}
	return thread
}

func TestClassifyThreadUsesRuntimeVerdictNotAncestry(t *testing.T) {
	cases := []struct {
		fixture  string
		want     proto.Provenance
		nickname string
		source   string
	}{
		{"thread_user.json", proto.Provenance{Source: proto.SourceCodexAppServer, Addressable: true}, "", "user"},
		{"thread_subagent_denied.json", proto.Provenance{Source: proto.SourceCodexAppServer, ParentRuntimeID: "01a082bf-c41e-7251-8f70-74017e02fb54", Addressable: false, AddressableReason: reasonDirectInputDenied}, "Pasteur", "subagent"},
		// A depth-1 sub-agent that the runtime says accepts direct input stays
		// addressable even though it is both a child and ephemeral.
		{"thread_subagent_ephemeral_accepting.json", proto.Provenance{Source: proto.SourceCodexAppServer, ParentRuntimeID: "01a0b69f-0110-7961-a6a8-2f4fba871217", Ephemeral: true, Addressable: true}, "Pascal", "subagent"},
	}
	for _, tc := range cases {
		got, nickname, source := classifyThread(loadThreadFixture(t, tc.fixture))
		if got != tc.want {
			t.Errorf("%s: provenance = %+v, want %+v", tc.fixture, got, tc.want)
		}
		if nickname != tc.nickname || source != tc.source {
			t.Errorf("%s: nickname/source = %q/%q, want %q/%q", tc.fixture, nickname, source, tc.nickname, tc.source)
		}
	}
	// A payload without the field (older app-server) is treated as addressable.
	if got, _, _ := classifyThread(map[string]any{"id": "x", "cwd": "/p"}); !got.Addressable || got.Source != proto.SourceCodexAppServer {
		t.Fatalf("missing canAcceptDirectInput classified as %+v", got)
	}
}

// A registered thread pushes changed provenance to the daemon on re-read and
// on the exact denial; generic -32600 errors never demote, and only a fresh
// read restores addressability.
func TestReclassifyAndDenialPushProvenance(t *testing.T) {
	var mu sync.Mutex
	var pushes []proto.Provenance
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/provenance") {
			var prov proto.Provenance
			_ = json.NewDecoder(r.Body).Decode(&prov)
			mu.Lock()
			pushes = append(pushes, prov)
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer daemon.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &Bridge{ctx: ctx, daemonHTTP: daemon.URL, threads: map[string]*threadPeer{}}
	p := &threadPeer{bridge: b, id: "thread-1", peerID: "repow-1-abc"}
	p.reclassify(loadThreadFixture(t, "thread_user.json"))
	waitPushes := func(n int) []proto.Provenance {
		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			got := append([]proto.Provenance(nil), pushes...)
			mu.Unlock()
			if len(got) >= n || time.Now().After(deadline) {
				return got
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if got := waitPushes(1); len(got) != 1 || !got[0].Addressable {
		t.Fatalf("first classification pushes = %+v", got)
	}

	p.demoteIfDenied(ctx, errors.New("map[code:-32600 message:ephemeral threads do not support includeTurns]"))
	p.demoteIfDenied(ctx, nil)
	if got := waitPushes(2); len(got) != 1 {
		t.Fatalf("generic errors must not demote, pushes = %+v", got)
	}

	p.demoteIfDenied(ctx, errors.New("map[code:-32600 message:"+directInputDenied+"]"))
	got := waitPushes(2)
	if len(got) != 2 || got[1].Addressable || got[1].AddressableReason != reasonDirectInputDenied {
		t.Fatalf("denial did not demote: %+v", got)
	}
	p.demoteIfDenied(ctx, errors.New(directInputDenied))
	if got := waitPushes(3); len(got) != 2 {
		t.Fatalf("repeated denial re-pushed: %+v", got)
	}

	// Registration after demotion still carries the denial.
	p.mu.Lock()
	kept := p.provenance
	p.mu.Unlock()
	if kept.Addressable {
		t.Fatal("demotion was not retained on the thread peer")
	}

	// Only a fresh runtime verdict restores addressability.
	p.reclassify(loadThreadFixture(t, "thread_user.json"))
	if got := waitPushes(3); len(got) != 3 || !got[2].Addressable {
		t.Fatalf("fresh classification did not restore: %+v", got)
	}
}
