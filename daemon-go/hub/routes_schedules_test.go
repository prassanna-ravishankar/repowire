package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

// fakeScheduleStore is an in-memory scheduleStore for the route tests (no
// SQLite). It is concurrency-safe so route handlers touching it concurrently
// are safe. service/scheduler_test.go keeps its own copy (plus the
// schedulerStore-only methods) for the firing-loop tests — the two packages
// no longer share one straddling fake now that hub and service are split.
type fakeScheduleStore struct {
	mu      sync.Mutex
	byID    map[string]*state.Schedule
	nextSeq int
}

func newFakeScheduleStore() *fakeScheduleStore {
	return &fakeScheduleStore{byID: map[string]*state.Schedule{}}
}

func (f *fakeScheduleStore) CreateSchedule(_ context.Context, fromPeer, toPeer, text string, fireAt time.Time, kind string, circle, cron *string) (*state.Schedule, error) {
	if kind == "" {
		kind = "notify"
	}
	if kind != "ask" && kind != "notify" {
		return nil, errors.New("kind must be one of [ask notify]; got " + kind)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSeq++
	s := &state.Schedule{
		ScheduleID: "sched-" + time.Now().Format("150405.000000") + "-" + string(rune('a'+f.nextSeq)),
		FromPeer:   fromPeer,
		ToPeer:     toPeer,
		Text:       text,
		FireAt:     fireAt.UTC().Format(time.RFC3339Nano),
		Kind:       kind,
		Circle:     circle,
		Cron:       cron,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	f.byID[s.ScheduleID] = s
	return s, nil
}

func (f *fakeScheduleStore) ListSchedules(_ context.Context, fromPeer *string) ([]*state.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*state.Schedule
	for _, s := range f.byID {
		if fromPeer != nil && s.FromPeer != *fromPeer {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeScheduleStore) DeleteSchedule(_ context.Context, id string) (*state.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byID[id]
	if !ok {
		return nil, nil
	}
	delete(f.byID, id)
	return s, nil
}

func (f *fakeScheduleStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byID)
}

// fakeWaker records Wake() calls.
type fakeWaker struct {
	mu    sync.Mutex
	wakes int
}

func (w *fakeWaker) Wake() {
	w.mu.Lock()
	w.wakes++
	w.mu.Unlock()
}

func (w *fakeWaker) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wakes
}

func newScheduleTestRig(t *testing.T) (*httptest.Server, *fakeScheduleStore, *fakeWaker) {
	t.Helper()
	store := newFakeScheduleStore()
	waker := &fakeWaker{}
	h := (&Hub{}).WithSchedules(store, waker)
	mux := http.NewServeMux()
	h.registerScheduleRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store, waker
}

// TestCreateOneShotSchedule is the primary path: POST a one-shot fire_at,
// expect 200 with the echoed schedule, the row persisted, and the scheduler woken.
func TestCreateOneShotSchedule(t *testing.T) {
	srv, store, waker := newScheduleTestRig(t)

	resp := postJSON(t, srv.URL+"/schedules", map[string]any{
		"from_peer": "alpha",
		"to_peer":   "beta",
		"text":      "stand-up",
		"fire_at":   "2030-01-02T15:04:05Z",
		"kind":      "notify",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out scheduleResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.FromPeer != "alpha" || out.ToPeer != "beta" || out.Text != "stand-up" {
		t.Fatalf("unexpected echo: %+v", out)
	}
	if out.Kind != "notify" {
		t.Fatalf("kind = %q, want notify", out.Kind)
	}
	if out.ScheduleID == "" {
		t.Fatal("expected a schedule_id")
	}
	if store.count() != 1 {
		t.Fatalf("store has %d schedules, want 1", store.count())
	}
	if waker.count() != 1 {
		t.Fatalf("waker called %d times, want 1", waker.count())
	}
}

// TestCreateCronSchedule: a cron schedule resolves a concrete first fire_at via
// NextFireAfter and stores the normalized cron.
func TestCreateCronSchedule(t *testing.T) {
	srv, _, _ := newScheduleTestRig(t)
	resp := postJSON(t, srv.URL+"/schedules", map[string]any{
		"from_peer": "alpha",
		"to_peer":   "beta",
		"text":      "hourly check",
		"cron":      "@hourly",
		"kind":      "ask",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out scheduleResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Cron == nil || *out.Cron != "0 * * * *" {
		t.Fatalf("cron = %v, want normalized '0 * * * *'", out.Cron)
	}
	if out.FireAt == "" {
		t.Fatal("expected a resolved fire_at for cron schedule")
	}
	if _, err := time.Parse(time.RFC3339Nano, out.FireAt); err != nil {
		t.Fatalf("fire_at not RFC3339: %q (%v)", out.FireAt, err)
	}
}

// TestCreateRequiresExactlyOne: neither fire_at nor cron → 400; both → 400.
func TestCreateRequiresExactlyOne(t *testing.T) {
	srv, _, _ := newScheduleTestRig(t)

	none := postJSON(t, srv.URL+"/schedules", map[string]any{
		"from_peer": "alpha", "to_peer": "beta", "text": "x",
	})
	none.Body.Close()
	if none.StatusCode != http.StatusBadRequest {
		t.Fatalf("no fire_at/cron: status = %d, want 400", none.StatusCode)
	}

	both := postJSON(t, srv.URL+"/schedules", map[string]any{
		"from_peer": "alpha", "to_peer": "beta", "text": "x",
		"fire_at": "2030-01-02T15:04:05Z", "cron": "@hourly",
	})
	both.Body.Close()
	if both.StatusCode != http.StatusBadRequest {
		t.Fatalf("both fire_at+cron: status = %d, want 400", both.StatusCode)
	}
}

// TestCreateBadCron: an unparseable cron → 400.
func TestCreateBadCron(t *testing.T) {
	srv, _, _ := newScheduleTestRig(t)
	resp := postJSON(t, srv.URL+"/schedules", map[string]any{
		"from_peer": "alpha", "to_peer": "beta", "text": "x", "cron": "not a cron",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestListAndDelete: list reflects creates; delete returns 200 then 404.
func TestListAndDelete(t *testing.T) {
	srv, _, waker := newScheduleTestRig(t)

	create := postJSON(t, srv.URL+"/schedules", map[string]any{
		"from_peer": "alpha", "to_peer": "beta", "text": "x", "fire_at": "2030-01-02T15:04:05Z",
	})
	var created scheduleResponse
	_ = json.NewDecoder(create.Body).Decode(&created)
	create.Body.Close()

	listResp, err := http.Get(srv.URL + "/schedules")
	if err != nil {
		t.Fatalf("GET /schedules: %v", err)
	}
	var list scheduleListResponse
	_ = json.NewDecoder(listResp.Body).Decode(&list)
	listResp.Body.Close()
	if len(list.Schedules) != 1 {
		t.Fatalf("list returned %d, want 1", len(list.Schedules))
	}

	wakesBefore := waker.count()
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/schedules/"+created.ScheduleID, nil)
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", del.StatusCode)
	}
	if waker.count() != wakesBefore+1 {
		t.Fatalf("delete should wake the scheduler")
	}

	req2, _ := http.NewRequest(http.MethodDelete, srv.URL+"/schedules/"+created.ScheduleID, nil)
	del2, _ := http.DefaultClient.Do(req2)
	del2.Body.Close()
	if del2.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", del2.StatusCode)
	}
}

// TestParseFireAtNaiveIsUTC: a naive ISO-8601 datetime is treated as UTC.
func TestParseFireAtNaiveIsUTC(t *testing.T) {
	got, err := parseFireAt("2030-01-02T15:04:05")
	if err != nil {
		t.Fatalf("parseFireAt: %v", err)
	}
	want := time.Date(2030, 1, 2, 15, 4, 5, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("location = %v, want UTC", got.Location())
	}
}
