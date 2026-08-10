package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewStoreImportsLegacyStateOnce(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"sessions.json":  `{"repow-legacy":{"display_name":"legacy","circle":"default","backend":"codex","path":"/repo","role":"agent","updated_at":"2026-07-10T10:00:00Z","description":"old"}}`,
		"events.json":    `[{"id":"event-legacy","type":"chat_turn","timestamp":"2026-07-10T10:00:00Z","peer":"legacy","text":"hello"}]`,
		"schedules.json": `{"sched-legacy":{"schedule_id":"sched-legacy","from_peer":"legacy","to_peer":"other","text":"ping","fire_at":"2026-07-11T10:00:00Z","kind":"notify","created_at":"2026-07-10T10:00:00Z"}}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "state.db")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	mappings, _ := store.LoadMappings(context.Background())
	if len(mappings) != 1 || mappings[0].SessionID != "repow-legacy" {
		t.Fatalf("legacy mappings = %#v", mappings)
	}
	if schedule, _ := store.GetSchedule(context.Background(), "sched-legacy"); schedule == nil {
		t.Fatal("legacy schedule was not imported")
	}
	var eventText string
	if err := store.db.QueryRow(`SELECT json_extract(payload_json, '$.text') FROM events WHERE event_id='event-legacy'`).Scan(&eventText); err != nil || eventText != "hello" {
		t.Fatalf("legacy event text = %q, %v", eventText, err)
	}
	var audits int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM legacy_imports WHERE status='ok'`).Scan(&audits); err != nil || audits != 3 {
		t.Fatalf("legacy audits = %d, %v", audits, err)
	}
	_ = store.Close()

	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	mappings, _ = reopened.LoadMappings(context.Background())
	if len(mappings) != 1 {
		t.Fatalf("legacy sessions were re-imported/destructively replaced: %#v", mappings)
	}
}

func TestLegacySchedulesImportAlongsideExistingRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSchedule(context.Background(), "existing", "peer", "keep", time.Now().Add(time.Hour), "notify", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	legacy := `{"sched-legacy":{"schedule_id":"sched-legacy","from_peer":"legacy","to_peer":"other","text":"ping","fire_at":"2026-07-11T10:00:00Z","kind":"notify","created_at":"2026-07-10T10:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dir, "schedules.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if schedule, err := reopened.GetSchedule(context.Background(), "sched-legacy"); err != nil || schedule == nil {
		t.Fatalf("legacy schedule was not merged: %#v, %v", schedule, err)
	}
	var count int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM schedules`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("schedule count = %d, %v", count, err)
	}
}
