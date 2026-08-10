package service

// Small dependency-free five-field cron parser for repowire schedules.
//
// Supported syntax is standard five-field cron:
//
//	minute hour day-of-month month day-of-week
//
// Each field supports "*", comma lists, ranges, and steps (e.g. "*/15",
// "1-5/2"). Day-of-week uses cron numbering where 0 or 7 is Sunday.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func cronErr(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

var cronAliases = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
}

// NormalizeCron normalizes supported aliases and whitespace.
func NormalizeCron(expr string) string {
	raw := strings.Join(strings.Fields(strings.TrimSpace(expr)), " ")
	if alias, ok := cronAliases[strings.ToLower(raw)]; ok {
		return alias
	}
	return raw
}

// ValidateCron validates and returns the normalized cron expression.
func ValidateCron(expr string) (string, error) {
	norm := NormalizeCron(expr)
	fields := strings.Fields(norm)
	if len(fields) != 5 {
		return "", cronErr("cron must have 5 fields: minute hour day-of-month month day-of-week")
	}
	specs := []struct {
		raw      string
		min, max int
		label    string
	}{
		{fields[0], 0, 59, "minute"},
		{fields[1], 0, 23, "hour"},
		{fields[2], 1, 31, "day-of-month"},
		{fields[3], 1, 12, "month"},
		{fields[4], 0, 7, "day-of-week"},
	}
	for _, s := range specs {
		if _, err := parseCronField(s.raw, s.min, s.max, s.label); err != nil {
			return "", err
		}
	}
	return norm, nil
}

// NextFireAfter returns the next UTC minute matching expr strictly after the given time.
func NextFireAfter(expr string, after time.Time) (time.Time, error) {
	norm, err := ValidateCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	fields := strings.Fields(norm)
	minutes, _ := parseCronField(fields[0], 0, 59, "minute")
	hours, _ := parseCronField(fields[1], 0, 23, "hour")
	days, _ := parseCronField(fields[2], 1, 31, "day-of-month")
	months, _ := parseCronField(fields[3], 1, 12, "month")
	weekdays, _ := parseCronField(fields[4], 0, 7, "day-of-week")
	if weekdays[7] {
		delete(weekdays, 7)
		weekdays[0] = true
	}
	domAny := fields[2] == "*"
	dowAny := fields[4] == "*"

	base := after.UTC()
	// Truncate to the minute, then step forward at least one minute.
	candidate := time.Date(base.Year(), base.Month(), base.Day(), base.Hour(), base.Minute(), 0, 0, time.UTC).Add(time.Minute)
	deadline := candidate.Add(366 * 24 * time.Hour)
	for !candidate.After(deadline) {
		// Go: Sunday=0. cron: Sunday=0 too, so Weekday() maps directly.
		cronWeekday := int(candidate.Weekday())
		if minutes[candidate.Minute()] &&
			hours[candidate.Hour()] &&
			months[int(candidate.Month())] &&
			cronDayMatches(candidate.Day(), cronWeekday, days, weekdays, domAny, dowAny) {
			return candidate, nil
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, cronErr("cron expression did not match within one year")
}

// cronDayMatches matches cron day fields. When both day-of-month and
// day-of-week are restricted, cron treats them as OR; when either is "*", the
// restricted field alone controls.
func cronDayMatches(day, weekday int, days, weekdays map[int]bool, domAny, dowAny bool) bool {
	if domAny && dowAny {
		return true
	}
	if domAny {
		return weekdays[weekday]
	}
	if dowAny {
		return days[day]
	}
	return days[day] || weekdays[weekday]
}

// parseCronField parses one field into the set of integers it matches.
func parseCronField(raw string, minValue, maxValue int, label string) (map[int]bool, error) {
	values := map[int]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, cronErr("empty %s field", label)
		}
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			stepRaw := part[i+1:]
			part = part[:i]
			s, err := strconv.Atoi(stepRaw)
			if err != nil {
				return nil, cronErr("%s step must be an integer", label)
			}
			if s < 1 {
				return nil, cronErr("%s step must be >= 1", label)
			}
			step = s
		}
		var start, end int
		switch {
		case part == "*":
			start, end = minValue, maxValue
		case strings.Contains(part, "-"):
			bounds := strings.SplitN(part, "-", 2)
			var err error
			if start, err = parseCronInt(bounds[0], label); err != nil {
				return nil, err
			}
			if end, err = parseCronInt(bounds[1], label); err != nil {
				return nil, err
			}
			if start > end {
				return nil, cronErr("%s range start must be <= end", label)
			}
		default:
			v, err := parseCronInt(part, label)
			if err != nil {
				return nil, err
			}
			start, end = v, v
		}
		if start < minValue || end > maxValue {
			return nil, cronErr("%s values must be between %d and %d", label, minValue, maxValue)
		}
		for v := start; v <= end; v += step {
			values[v] = true
		}
	}
	return values, nil
}

func parseCronInt(raw, label string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, cronErr("%s value must be an integer", label)
	}
	return v, nil
}
