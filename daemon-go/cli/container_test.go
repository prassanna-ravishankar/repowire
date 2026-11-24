package cli

import (
	"testing"
	"time"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC)
	for raw, want := range map[string]time.Time{
		"all": {}, "7d": now.Add(-7 * 24 * time.Hour), "36h": now.Add(-36 * time.Hour),
		"2026-12-01T00:00:00Z": time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseSince(raw, now)
		if err != nil || !got.Equal(want) {
			t.Fatalf("parseSince(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, bad := range []string{"7x", "-1d", "yesterday"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Fatalf("parseSince(%q) accepted", bad)
		}
	}
}
