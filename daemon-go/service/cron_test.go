package service

import (
	"testing"
	"time"
)

func TestNextFireAfterEveryHour(t *testing.T) {
	// "0 * * * *" fires at the top of every hour.
	after := time.Date(2030, 1, 2, 15, 30, 0, 0, time.UTC)
	got, err := NextFireAfter("0 * * * *", after)
	if err != nil {
		t.Fatalf("NextFireAfter: %v", err)
	}
	want := time.Date(2030, 1, 2, 16, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNextFireAfterStep(t *testing.T) {
	// "*/15 * * * *" fires at :00 :15 :30 :45.
	after := time.Date(2030, 1, 2, 15, 7, 0, 0, time.UTC)
	got, err := NextFireAfter("*/15 * * * *", after)
	if err != nil {
		t.Fatalf("NextFireAfter: %v", err)
	}
	if got.Minute() != 15 || got.Hour() != 15 {
		t.Fatalf("got %v, want 15:15", got)
	}
}

func TestNextFireAfterDayOfWeek(t *testing.T) {
	// "0 0 * * 0" = midnight on Sunday. 2030-01-02 is a Wednesday → next Sunday.
	after := time.Date(2030, 1, 2, 12, 0, 0, 0, time.UTC)
	got, err := NextFireAfter("0 0 * * 0", after)
	if err != nil {
		t.Fatalf("NextFireAfter: %v", err)
	}
	if got.Weekday() != time.Sunday || got.Hour() != 0 || got.Minute() != 0 {
		t.Fatalf("got %v, want a Sunday midnight", got)
	}
}

func TestNextFireAfterSunday7Alias(t *testing.T) {
	// dow 7 is also Sunday; must match the same as 0.
	after := time.Date(2030, 1, 2, 12, 0, 0, 0, time.UTC)
	got, err := NextFireAfter("0 0 * * 7", after)
	if err != nil {
		t.Fatalf("NextFireAfter: %v", err)
	}
	if got.Weekday() != time.Sunday {
		t.Fatalf("got %v, want Sunday", got)
	}
}

func TestValidateCronRejectsBad(t *testing.T) {
	cases := []string{
		"",            // empty
		"* * * *",     // four fields
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"*/0 * * * *", // step < 1
		"5-1 * * * *", // reversed range
		"x * * * *",   // non-integer
	}
	for _, c := range cases {
		if _, err := ValidateCron(c); err == nil {
			t.Errorf("ValidateCron(%q) = nil err, want error", c)
		}
	}
}

func TestNormalizeCronAliases(t *testing.T) {
	if got := NormalizeCron("@daily"); got != "0 0 * * *" {
		t.Fatalf("@daily = %q", got)
	}
	if got := NormalizeCron("  0   *  *  *  * "); got != "0 * * * *" {
		t.Fatalf("whitespace collapse = %q", got)
	}
}
