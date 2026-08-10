package service

import (
	"testing"
	"time"
)

func TestParseRunnerISOUsesRFC3339(t *testing.T) {
	cases := map[string]time.Time{
		"2026-07-28T12:34:56.123456Z": time.Date(2026, 7, 28, 12, 34, 56, 123456000, time.UTC),
		"2026-07-28T13:34:56+01:00":   time.Date(2026, 7, 28, 12, 34, 56, 0, time.UTC),
	}
	for value, want := range cases {
		got, err := parseRunnerISO(value)
		if err != nil {
			t.Fatalf("parse %q: %v", value, err)
		}
		if !got.Equal(want) {
			t.Fatalf("parse %q = %s, want %s", value, got, want)
		}
	}
	if _, err := parseRunnerISO("not-a-time"); err == nil {
		t.Fatal("invalid timestamp accepted")
	}
}
