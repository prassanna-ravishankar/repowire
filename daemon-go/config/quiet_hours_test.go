package config

import (
	"strings"
	"testing"
	"time"
)

func mustWindow(t *testing.T, q QuietHours) QuietWindow {
	t.Helper()
	w, err := q.Parse()
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestQuietWindowAcrossMidnight(t *testing.T) {
	w := mustWindow(t, QuietHours{Start: "22:30", End: "07:30", Timezone: "Europe/London"})
	london, _ := time.LoadLocation("Europe/London")
	at := func(h, m int) time.Time { return time.Date(2026, 11, 25, h, m, 0, 0, london) }
	for _, c := range []struct {
		h, m  int
		quiet bool
	}{{22, 29, false}, {22, 30, true}, {23, 59, true}, {0, 0, true}, {7, 29, true}, {7, 30, false}, {12, 0, false}} {
		if got := w.Quiet(at(c.h, c.m)); got != c.quiet {
			t.Errorf("%02d:%02d quiet=%v, want %v", c.h, c.m, got, c.quiet)
		}
	}
	if end := w.End(at(23, 0)); !end.Equal(time.Date(2026, 11, 26, 7, 30, 0, 0, london)) || end.Location() != time.UTC {
		t.Fatalf("end from 23:00 = %v, want the next morning's 07:30 London, in UTC", end)
	}
	if end := w.End(at(3, 0)); !end.Equal(time.Date(2026, 11, 25, 7, 30, 0, 0, london)) {
		t.Fatalf("end from 03:00 = %v, want this morning's 07:30", end)
	}
	day := mustWindow(t, QuietHours{Start: "12:00", End: "13:00", Timezone: "Europe/London"})
	if !day.Quiet(at(12, 30)) || day.Quiet(at(13, 0)) || day.Quiet(at(11, 59)) {
		t.Fatal("a same-day window is half-open [start, end)")
	}
}

// End is an absolute instant computed in the window's zone: a DST night
// keeps the wall-clock end, and a caller in another zone reads the same
// instant.
func TestQuietWindowEndAcrossDST(t *testing.T) {
	w := mustWindow(t, QuietHours{Start: "22:00", End: "07:00", Timezone: "Europe/London"})
	london, _ := time.LoadLocation("Europe/London")
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	// Clocks go back at 02:00 on 2026-10-25: the night is an hour longer.
	before := time.Date(2026, 10, 24, 23, 0, 0, 0, london)
	end := w.End(before)
	if got := end.In(london); got.Hour() != 7 || got.Minute() != 0 || got.Day() != 25 {
		t.Fatalf("end across DST = %v London, want 07:00 on the 25th", got)
	}
	if !w.End(before.In(tokyo)).Equal(end) {
		t.Fatal("the end depends on the caller's zone")
	}
}

func TestQuietHoursParseRejects(t *testing.T) {
	for name, q := range map[string]QuietHours{
		"bad start":    {Start: "25:00", End: "07:00"},
		"bad end":      {Start: "22:00", End: "7"},
		"same time":    {Start: "22:00", End: "22:00"},
		"zone":         {Start: "22:00", End: "07:00", Timezone: "Mars/Olympus"},
		"bypass class": {Start: "22:00", End: "07:00", Bypass: []string{"everything"}},
	} {
		if _, err := q.Parse(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if w := mustWindow(t, QuietHours{Start: "22:00", End: "07:00"}); !w.Bypasses(BypassBlocking) || w.Bypasses(BypassAsk) {
		t.Fatal("default bypass must be blocking only")
	}
	if w := mustWindow(t, QuietHours{Start: "22:00", End: "07:00", Bypass: []string{}}); w.Bypasses(BypassBlocking) {
		t.Fatal("an empty bypass list holds everything")
	}
}

// A window that cannot be read is dropped with a warning naming the key, not
// its value, and the daemon starts: the surface is pushed as before.
func TestLoadDropsAnInvalidQuietWindow(t *testing.T) {
	cfg, err := tryLoadYAML(t, "human:\n  surfaces:\n    telegram:\n      quiet_hours: {start: \"22:00\", end: \"22:00\", timezone: Secret/Zone}\n      digest: true\n    slack:\n      quiet_hours: {start: \"23:00\", end: \"06:00\"}\n")
	if err != nil {
		t.Fatalf("an invalid window failed the load: %v", err)
	}
	if _, ok := cfg.Human.QuietWindow("telegram"); ok || cfg.Human.Surfaces["telegram"].QuietHours != nil || !cfg.Human.Digests("telegram") {
		t.Fatalf("telegram = %+v", cfg.Human.Surfaces["telegram"])
	}
	if _, ok := cfg.Human.QuietWindow("slack"); !ok {
		t.Fatal("a valid window was dropped")
	}
	joined := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(joined, "human.surfaces.telegram.quiet_hours") || strings.Contains(joined, "Secret") {
		t.Fatalf("warnings = %q", cfg.Warnings)
	}
	if _, ok := loadYAML(t, "daemon:\n  port: 9100\n").Human.QuietWindow("telegram"); ok {
		t.Fatal("no quiet_hours must mean no window")
	}
}
