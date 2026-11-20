package service

import (
	"math/rand"
	"strconv"
	"strings"
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

func TestNextFireAfterLeapDay(t *testing.T) {
	// 2100 is not a leap year, so the next February 29 after 2097 is in 2104.
	for _, tc := range []struct{ after, want time.Time }{
		{time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), time.Date(2028, 2, 29, 9, 30, 0, 0, time.UTC)},
		{time.Date(2097, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2104, 2, 29, 9, 30, 0, 0, time.UTC)},
	} {
		got, err := NextFireAfter("30 9 29 2 *", tc.after)
		if err != nil || !got.Equal(tc.want) {
			t.Fatalf("after %v: got %v, %v; want %v", tc.after, got, err, tc.want)
		}
	}
}

func TestNextFireAfterRejectsImpossibleDate(t *testing.T) {
	if got, err := NextFireAfter("0 0 30 2 *", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatalf("February 30 matched %v", got)
	}
}

// TestNextFireAfterMatchesMinuteScan checks the field-skipping search against
// a plain minute-by-minute scan on random expressions and start times. Days
// stop at 28 so every expression matches within the scan's one-year window.
func TestNextFireAfterMatchesMinuteScan(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	field := func(lo, hi int) string {
		switch rng.Intn(5) {
		case 0:
			return "*"
		case 1:
			return "*/" + itoa(1+rng.Intn(hi-lo))
		case 2:
			a := lo + rng.Intn(hi-lo+1)
			return itoa(a) + "-" + itoa(a+rng.Intn(hi-a+1))
		case 3:
			return itoa(lo+rng.Intn(hi-lo+1)) + "," + itoa(lo+rng.Intn(hi-lo+1))
		default:
			return itoa(lo + rng.Intn(hi-lo+1))
		}
	}
	for i := 0; i < 60; i++ {
		expr := strings.Join([]string{field(0, 59), field(0, 23), field(1, 28), field(1, 12), field(0, 7)}, " ")
		after := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rng.Int63n(int64(4 * 365 * 24 * time.Hour))))
		got, err := NextFireAfter(expr, after)
		want, ok := minuteScan(t, expr, after, 366*24*60)
		if !ok {
			t.Fatalf("%q after %v: no match within a year of scanning", expr, after)
		}
		if err != nil || !got.Equal(want) {
			t.Fatalf("%q after %v: got %v, %v; scan found %v", expr, after, got, err, want)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// minuteScan is the reference: step one minute at a time for up to limit steps.
func minuteScan(t *testing.T, expr string, after time.Time, limit int) (time.Time, bool) {
	t.Helper()
	f := strings.Fields(NormalizeCron(expr))
	parse := func(raw string, lo, hi int) (set [64]bool) {
		v, err := parseCronField(raw, lo, hi, "field")
		if err != nil {
			t.Fatalf("%q: %v", expr, err)
		}
		for n := range v {
			set[n] = true
		}
		return set
	}
	minutes, hours, days, months, weekdays := parse(f[0], 0, 59), parse(f[1], 0, 23), parse(f[2], 1, 31), parse(f[3], 1, 12), parse(f[4], 0, 7)
	weekdays[0] = weekdays[0] || weekdays[7]
	domAny, dowAny := f[2] == "*", f[4] == "*"
	c := after.UTC().Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < limit; i, c = i+1, c.Add(time.Minute) {
		if !minutes[c.Minute()] || !hours[c.Hour()] || !months[c.Month()] {
			continue
		}
		dom, dow := days[c.Day()], weekdays[c.Weekday()]
		if (domAny && dowAny) || (domAny && dow) || (dowAny && dom) || (!domAny && !dowAny && (dom || dow)) {
			return c, true
		}
	}
	return time.Time{}, false
}
