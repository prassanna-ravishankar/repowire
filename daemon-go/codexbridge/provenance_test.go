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
		{"thread_user.json", proto.Provenance{Source: proto.SourceCodexAppServer, Initiator: proto.InitiatorUser, Addressable: true}, "", "user"},
		{"thread_subagent_denied.json", proto.Provenance{Source: proto.SourceCodexAppServer, Initiator: proto.InitiatorAgent, ParentRuntimeID: "01a082bf-c41e-7251-8f70-74017e02fb54", Addressable: false, AddressableReason: reasonDirectInputDenied}, "Pasteur", "subagent"},
		// A depth-1 sub-agent that the runtime says accepts direct input stays
		// addressable even though it is both a child and ephemeral.
		{"thread_subagent_ephemeral_accepting.json", proto.Provenance{Source: proto.SourceCodexAppServer, Initiator: proto.InitiatorAgent, ParentRuntimeID: "01a0b69f-0110-7961-a6a8-2f4fba871217", Ephemeral: true, Addressable: true}, "Pascal", "subagent"},
		// System threads (desktop-app machinery nobody opened) stay addressable
		// but are marked so the default view can hide them.
		{"thread_system_ephemeral.json", proto.Provenance{Source: proto.SourceCodexAppServer, Initiator: proto.InitiatorSystem, Ephemeral: true, Addressable: true}, "", "system"},
	}
	for _, tc := range cases {
		got, nickname, source := classifyThread(loadThreadFixture(t, tc.fixture), nil)
		if got != tc.want {
			t.Errorf("%s: provenance = %+v, want %+v", tc.fixture, got, tc.want)
		}
		if nickname != tc.nickname || source != tc.source {
			t.Errorf("%s: nickname/source = %q/%q, want %q/%q", tc.fixture, nickname, source, tc.nickname, tc.source)
		}
	}
	// A payload without the field (older app-server) starts addressable, but
	// once a denial is known only an explicit true undoes it.
	bare := map[string]any{"id": "x", "cwd": "/p"}
	if got, _, _ := classifyThread(bare, nil); !got.Addressable || got.Source != proto.SourceCodexAppServer {
		t.Fatalf("missing canAcceptDirectInput classified as %+v", got)
	}
	denied := proto.Provenance{Source: proto.SourceCodexAppServer, Addressable: false, AddressableReason: reasonDirectInputDenied}
	if got, _, _ := classifyThread(bare, &denied); got.Addressable || got.AddressableReason != reasonDirectInputDenied {
		t.Fatalf("absent verdict promoted a denied thread: %+v", got)
	}
	if got, _, _ := classifyThread(map[string]any{"id": "x", "cwd": "/p", "canAcceptDirectInput": true}, &denied); !got.Addressable || got.AddressableReason != "" {
		t.Fatalf("explicit true did not restore: %+v", got)
	}
}

// A test daemon that records provenance POSTs in arrival order and can hold
// or fail individual requests.
type provenanceDaemon struct {
	t      *testing.T
	mu     sync.Mutex
	pushes []proto.Provenance
	hold   chan struct{} // when non-nil, the next POST blocks until closed
	fail   int           // number of upcoming POSTs to answer with 500
	srv    *httptest.Server
}

func newProvenanceDaemon(t *testing.T) *provenanceDaemon {
	d := &provenanceDaemon{t: t}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/provenance") {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		var prov proto.Provenance
		_ = json.NewDecoder(r.Body).Decode(&prov)
		d.mu.Lock()
		hold, fail := d.hold, d.fail
		d.hold = nil
		if fail > 0 {
			d.fail--
		}
		d.mu.Unlock()
		if hold != nil {
			<-hold
		}
		if fail > 0 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		d.mu.Lock()
		d.pushes = append(d.pushes, prov)
		d.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *provenanceDaemon) wait(n int) []proto.Provenance {
	deadline := time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		got := append([]proto.Provenance(nil), d.pushes...)
		d.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newThreadPeerForPublish(t *testing.T, d *provenanceDaemon) *threadPeer {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := &Bridge{ctx: ctx, daemonHTTP: d.srv.URL, threads: map[string]*threadPeer{}}
	return &threadPeer{bridge: b, id: "thread-1", peerID: "repow-1-abc"}
}

// Publication is serialized and versioned: an accepting snapshot that is
// still in flight when a denial lands can never overwrite the denial.
func TestProvenancePublicationIsOrderedUnderOverlap(t *testing.T) {
	d := newProvenanceDaemon(t)
	p := newThreadPeerForPublish(t, d)
	release := make(chan struct{})
	d.mu.Lock()
	d.hold = release
	d.mu.Unlock()

	p.reclassify(loadThreadFixture(t, "thread_user.json")) // v1 accepting, held in flight
	time.Sleep(20 * time.Millisecond)
	p.demoteIfDenied(context.Background(), errors.New("map[code:-32600 message:"+directInputDenied+"]")) // v2 denied, owed
	if got := d.wait(1); len(got) != 0 {
		t.Fatalf("denial was published while the accepting request was held: %+v", got)
	}
	close(release)
	got := d.wait(2)
	if len(got) != 2 || !got[0].Addressable || got[1].Addressable {
		t.Fatalf("pushes = %+v, want accepting then denied", got)
	}
	p.mu.Lock()
	if p.publishedVersion != p.provVersion || p.publishing {
		t.Fatalf("publisher left state v%d published=%d publishing=%v", p.provVersion, p.publishedVersion, p.publishing)
	}
	p.mu.Unlock()
}

// A failed POST leaves the version owed; the next reconcile with unchanged
// runtime metadata retries it, as does the next delivery. Generic -32600
// errors never demote, a repeated denial is not re-pushed, and after a
// denial only an explicit runtime verdict restores addressability.
func TestProvenancePublicationRetriesFailedPush(t *testing.T) {
	d := newProvenanceDaemon(t)
	p := newThreadPeerForPublish(t, d)
	d.mu.Lock()
	d.fail = 1
	d.mu.Unlock()

	p.reclassify(loadThreadFixture(t, "thread_user.json"))
	time.Sleep(50 * time.Millisecond)
	if got := d.wait(1); len(got) != 0 {
		t.Fatalf("failed POST was recorded as a push: %+v", got)
	}
	p.mu.Lock()
	owed := p.provVersion != p.publishedVersion && !p.publishing
	p.mu.Unlock()
	if !owed {
		t.Fatal("failed push did not leave the version owed")
	}
	p.reclassify(loadThreadFixture(t, "thread_user.json")) // unchanged payload still retries
	if got := d.wait(1); len(got) != 1 || !got[0].Addressable {
		t.Fatalf("retry after unchanged reclassify = %+v", got)
	}

	p.demoteIfDenied(context.Background(), errors.New("map[code:-32600 message:ephemeral threads do not support includeTurns]"))
	p.demoteIfDenied(context.Background(), nil)
	time.Sleep(30 * time.Millisecond)
	if got := d.wait(2); len(got) != 1 {
		t.Fatalf("generic errors must not demote: %+v", got)
	}
	p.demoteIfDenied(context.Background(), errors.New(directInputDenied))
	p.demoteIfDenied(context.Background(), errors.New(directInputDenied))
	got := d.wait(2)
	if len(got) != 2 || got[1].Addressable || got[1].AddressableReason != reasonDirectInputDenied {
		t.Fatalf("denial did not demote exactly once: %+v", got)
	}

	// Denial -> payload without the verdict -> still denied (P1 regression).
	// The initiator change is pushed, the verdict and reason are kept.
	p.reclassify(map[string]any{"id": "thread-1", "cwd": "/p", "threadSource": "subagent"})
	got = d.wait(3)
	if len(got) != 3 || got[2].Addressable || got[2].AddressableReason != reasonDirectInputDenied || got[2].Initiator != proto.InitiatorAgent {
		t.Fatalf("absent verdict did not preserve the denial: %+v", got)
	}
	p.reclassify(map[string]any{"id": "thread-1", "cwd": "/p", "threadSource": "subagent"})
	time.Sleep(30 * time.Millisecond)
	if got := d.wait(4); len(got) != 3 {
		t.Fatalf("unchanged payload re-pushed: %+v", got)
	}
	// Explicit true restores.
	p.reclassify(loadThreadFixture(t, "thread_user.json"))
	if got := d.wait(4); len(got) != 4 || !got[3].Addressable || got[3].AddressableReason != "" {
		t.Fatalf("explicit verdict did not restore: %+v", got)
	}
}
