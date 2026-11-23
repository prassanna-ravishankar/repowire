package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/relay"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

func newRetentionServer(t *testing.T, withRetention bool) (*httptest.Server, *state.Store, *peer.Registry) {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	transport := service.NewWebSocketTransport()
	reg, err := peer.NewRegistry(context.Background(), memStore{}, deadLive{}, transport)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHubWithTransport(reg, transport, "tok")
	if withRetention {
		r := service.NewRetention(store, config.Defaults().Retention, "").WithJournal(reg.AddEvent).WithSpawn(reg.Track)
		h.WithRetention(r)
		reg.AddRepairPass(r.MaybeSweep)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store, reg
}

func retentionCall(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func seedOldTranscript(t *testing.T, store *state.Store) {
	t.Helper()
	if err := store.AppendEvent(context.Background(), peer.Event{Type: "chat_turn", Timestamp: time.Now().Add(-8 * 24 * time.Hour),
		Payload: map[string]any{"text": "old transcript"}}); err != nil {
		t.Fatal(err)
	}
}

func transcriptCount(t *testing.T, store *state.Store) int {
	t.Helper()
	rows, err := store.EventsOfTypes(context.Background(), "chat_turn")
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func TestRetentionRoutesDryRunThenSweep(t *testing.T) {
	srv, store, _ := newRetentionServer(t, true)
	seedOldTranscript(t, store)

	code, body := retentionCall(t, srv, http.MethodGet, "/retention", "")
	if code != http.StatusOK {
		t.Fatalf("GET /retention = %d %v", code, body)
	}
	policy, _ := body["policy"].(map[string]any)
	next, _ := body["next_sweep"].(map[string]any)
	if policy["content_days"] != 7.0 || policy["records_days"] != 180.0 || next["events.delete_transcripts"] != 1.0 {
		t.Fatalf("GET /retention body = %v", body)
	}

	code, body = retentionCall(t, srv, http.MethodPost, "/retention/sweep", `{"dry_run":true}`)
	if code != http.StatusOK || transcriptCount(t, store) != 1 {
		t.Fatalf("dry run = %d %v, transcripts %d", code, body, transcriptCount(t, store))
	}
	code, body = retentionCall(t, srv, http.MethodPost, "/retention/sweep", "")
	counts, _ := body["counts"].(map[string]any)
	if code != http.StatusOK || counts["events.delete_transcripts"] != 1.0 || transcriptCount(t, store) != 0 {
		t.Fatalf("sweep = %d %v", code, body)
	}
	if code, _ := retentionCall(t, srv, http.MethodPost, "/retention/sweep", `{"vacuum":true}`); code != http.StatusOK {
		t.Fatalf("vacuum = %d", code)
	}
	code, body = retentionCall(t, srv, http.MethodGet, "/retention", "")
	if policy, _ := body["policy"].(map[string]any); code != http.StatusOK || policy["last_sweep_at"] == nil {
		t.Fatalf("last sweep not reported: %v", body)
	}
}

func TestRetentionRoutesNeedAuthAndASweeper(t *testing.T) {
	srv, _, _ := newRetentionServer(t, false)
	resp, err := http.Post(srv.URL+"/retention/sweep", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated sweep = %d", resp.StatusCode)
	}
	if code, _ := retentionCall(t, srv, http.MethodGet, "/retention", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("no sweeper = %d", code)
	}
}

func TestLazyRepairStartsADueSweep(t *testing.T) {
	_, store, reg := newRetentionServer(t, true)
	seedOldTranscript(t, store)
	reg.LazyRepair(context.Background())
	reg.Close() // joins the tracked sweep
	if n := transcriptCount(t, store); n != 0 {
		t.Fatalf("transcripts after lazy repair = %d", n)
	}
}

type countingRetentionStore struct{ sweeps atomic.Int32 }

func (c *countingRetentionStore) Sweep(context.Context, state.RetentionPolicy, time.Time, bool) (state.SweepReport, error) {
	c.sweeps.Add(1)
	return state.SweepReport{}, nil
}
func (c *countingRetentionStore) Vacuum(context.Context) error { return nil }

// A daemon whose only traffic is other machines' remote requests still runs
// a due sweep (cross-machine rows expire through it).
func TestRemoteRequestStartsADueSweep(t *testing.T) {
	_, _, beta := twoMachines(t)
	store := &countingRetentionStore{}
	beta.h.WithRetention(service.NewRetention(store, config.Defaults().Retention, "").WithSpawn(func(f func()) { f() }))
	req := httptest.NewRequestWithContext(relay.WithRemoteMachine(context.Background(), "alpha"), http.MethodPost, "/remote/peers", strings.NewReader(`{}`))
	beta.h.RemoteHandler().ServeHTTP(httptest.NewRecorder(), req)
	if got := store.sweeps.Load(); got != 1 {
		t.Fatalf("sweeps after a remote request = %d, want 1", got)
	}
}

type blockingRetentionStore struct {
	started, release chan struct{}
}

func (b *blockingRetentionStore) Sweep(context.Context, state.RetentionPolicy, time.Time, bool) (state.SweepReport, error) {
	close(b.started)
	<-b.release
	return state.SweepReport{}, nil
}
func (b *blockingRetentionStore) Vacuum(context.Context) error { return nil }

// While a sweep runs, the status route says so instead of reporting that the
// next sweep has nothing to do.
func TestRetentionStatusDuringASweepIsAConflict(t *testing.T) {
	store := &blockingRetentionStore{started: make(chan struct{}), release: make(chan struct{})}
	transport := service.NewWebSocketTransport()
	reg, err := peer.NewRegistry(context.Background(), memStore{}, deadLive{}, transport)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHubWithTransport(reg, transport, "tok")
	r := service.NewRetention(store, config.Defaults().Retention, "")
	h.WithRetention(r)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	done := make(chan struct{})
	go func() { defer close(done); _, _ = r.Sweep(context.Background(), false, false) }()
	<-store.started
	code, body := retentionCall(t, srv, http.MethodGet, "/retention", "")
	close(store.release)
	<-done
	if code != http.StatusConflict {
		t.Fatalf("status during a sweep = %d %v", code, body)
	}
}
