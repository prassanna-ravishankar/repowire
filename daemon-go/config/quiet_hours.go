package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// QuietHours holds a surface's pushes in a daily window (#476): items stored
// then wait, durable in the inbox, and are pushed when the window ends.
type QuietHours struct {
	// Start and End are wall-clock HH:MM in Timezone. End before Start
	// crosses midnight (22:30 to 07:30).
	Start string `yaml:"start"`
	End   string `yaml:"end"`
	// Timezone is an IANA zone; empty means the daemon's local zone.
	Timezone string `yaml:"timezone"`
	// Bypass lists the item classes pushed even in the window: "blocking"
	// (a question with a deadline, which holding would decide) and "ask".
	// Absent means [blocking]; an empty list holds everything.
	Bypass []string `yaml:"bypass"`
}

// Bypass classes.
const (
	BypassBlocking = "blocking"
	BypassAsk      = "ask"
)

// QuietWindow is a parsed QuietHours.
type QuietWindow struct {
	start, end int // minutes after midnight
	loc        *time.Location
	bypass     map[string]bool
}

// Parse validates q. A window must have distinct start and end times, a
// known zone and known bypass classes.
func (q QuietHours) Parse() (QuietWindow, error) {
	start, err := clockMinutes(q.Start)
	if err != nil {
		return QuietWindow{}, fmt.Errorf("start: %w", err)
	}
	end, err := clockMinutes(q.End)
	if err != nil {
		return QuietWindow{}, fmt.Errorf("end: %w", err)
	}
	if start == end {
		return QuietWindow{}, fmt.Errorf("start and end are the same time")
	}
	loc := time.Local
	if q.Timezone != "" {
		if loc, err = time.LoadLocation(q.Timezone); err != nil {
			return QuietWindow{}, fmt.Errorf("timezone: not a known zone")
		}
	}
	bypass := map[string]bool{BypassBlocking: true}
	if q.Bypass != nil {
		bypass = map[string]bool{}
		for _, class := range q.Bypass {
			if class != BypassBlocking && class != BypassAsk {
				return QuietWindow{}, fmt.Errorf("bypass: unknown class (use %s or %s)", BypassBlocking, BypassAsk)
			}
			bypass[class] = true
		}
	}
	return QuietWindow{start: start, end: end, loc: loc, bypass: bypass}, nil
}

func clockMinutes(hhmm string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(hhmm), ":")
	if !ok || len(h) == 0 || len(h) > 2 || len(m) != 2 {
		return 0, fmt.Errorf("not an HH:MM time")
	}
	hours, herr := strconv.Atoi(h)
	minutes, merr := strconv.Atoi(m)
	if herr != nil || merr != nil || hours < 0 || hours > 23 || minutes < 0 || minutes > 59 {
		return 0, fmt.Errorf("not an HH:MM time")
	}
	return hours*60 + minutes, nil
}

// Bypasses reports whether items of class are pushed even in the window.
func (w QuietWindow) Bypasses(class string) bool { return w.bypass[class] }

// Quiet reports whether now falls in the window, in the window's zone.
func (w QuietWindow) Quiet(now time.Time) bool {
	local := now.In(w.loc)
	m := local.Hour()*60 + local.Minute()
	if w.start < w.end {
		return m >= w.start && m < w.end
	}
	return m >= w.start || m < w.end // crosses midnight
}

// End is when the window containing now ends, as an absolute instant: the
// next End wall-clock time in the window's zone. Meaningful only while
// Quiet(now). Computed through the zone, so a bridge in another zone, or a
// DST change during the night, cannot shift it.
func (w QuietWindow) End(now time.Time) time.Time {
	local := now.In(w.loc)
	end := time.Date(local.Year(), local.Month(), local.Day(), w.end/60, w.end%60, 0, 0, w.loc)
	if !end.After(local) {
		end = time.Date(local.Year(), local.Month(), local.Day()+1, w.end/60, w.end%60, 0, 0, w.loc)
	}
	return end.UTC()
}
