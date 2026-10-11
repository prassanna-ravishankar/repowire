package routing

import (
	"regexp"
	"strconv"
	"strings"
)

// A provider error body is untrusted: it can echo our request (the ask text)
// and our headers (the API key). Router results are scrubbed before they are
// persisted, and again on export so rows written before scrubbing existed are
// covered too.

// credentialShapes matches known API key formats and auth headers.
var credentialShapes = regexp.MustCompile(`apikey_[A-Za-z0-9_]+|sk-ant-[A-Za-z0-9_-]+|(?i:bearer)\s+[^\s"]+|(?i:x-api-key)["']?\s*[:=]\s*["']?[^\s"',}]+`)

// minLiteral is the shortest literal (configured key or ask text) redacted
// verbatim; shorter strings would match too much innocent text.
const minLiteral = 8

const redacted = "<redacted>"

// Redact removes credential shapes and each literal (configured keys, the
// ask text) from s.
func Redact(s string, literals ...string) string {
	for _, lit := range literals {
		if len(lit) >= minLiteral {
			s = strings.ReplaceAll(s, lit, redacted)
		}
	}
	return credentialShapes.ReplaceAllString(s, redacted)
}

var statusInError = regexp.MustCompile(`status (\d{3})`)

// statusOf is the failure's HTTP status, 0 if none. Rows recorded before
// http_status existed carry it only in the error text.
func statusOf(res RouterResult) int {
	if res.HTTPStatus != 0 {
		return res.HTTPStatus
	}
	if m := statusInError.FindStringSubmatch(res.Error); m != nil {
		status, _ := strconv.Atoi(m[1])
		return status
	}
	return 0
}

// Summary is a failure's structured description without any body:
// "<status> <reason>", or the reason alone when no status is known.
func Summary(res RouterResult) string {
	status := statusOf(res)
	if status == 0 {
		return res.FallbackReason
	}
	return strconv.Itoa(status) + " " + res.FallbackReason
}

// Scrub prepares a router result for persistence or export. With the ask text
// not recorded, the body is dropped entirely: it may echo the ask, which is
// no longer available to redact. Otherwise credentials and literals are
// redacted.
func Scrub(res RouterResult, textRecorded bool, literals ...string) RouterResult {
	if res.Error == "" {
		return res
	}
	if !textRecorded {
		res.Error = Summary(res)
		return res
	}
	res.Error = Redact(res.Error, literals...)
	return res
}
