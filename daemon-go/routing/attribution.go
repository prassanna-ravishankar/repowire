package routing

import (
	"regexp"
	"strconv"
)

// The ledger never stores a provider error body. A body is untrusted: it can
// echo the ask or our credentials in any encoding, and no redaction of free
// text can be made safe. A failure is recorded by structure only: the HTTP
// status, the provider, our fallback reason, and the provider's declared
// machine code if it is one we know. Debugging a live failure goes through
// reproducing it, not the ledger.

// providerCodes is the allowlist of provider-declared error codes. Any other
// code is recorded as "other".
var providerCodes = map[string]bool{
	// Anthropic error.type values (platform.claude.com/docs/en/api/errors)
	// and error.details.error_code for the spend cap (…/api/rate-limits).
	"invalid_request_error": true, "authentication_error": true, "billing_error": true,
	"permission_error": true, "not_found_error": true, "conflict_error": true,
	"request_too_large": true, "rate_limit_error": true, "api_error": true,
	"timeout_error": true, "overloaded_error": true, "enforced_spend_limit_reached": true,
	// TypeSafe (Jev) documents no error body. detail.error_type
	// "billing_error", observed live on a 402, is listed above.
	// Workers AI (Clef) errors[].code values, as decimal strings
	// (developers.cloudflare.com/workers-ai/platform/errors/).
	"5007": true, "5004": true, "3039": true, "3003": true, "5018": true, "5016": true,
	"3023": true, "3041": true, "5035": true, "5019": true, "5005": true, "3042": true,
	"3006": true, "3007": true, "3008": true, "3036": true, "3040": true,
}

// errorCode returns code if it is allowlisted, else "other".
func errorCode(code string) string {
	if providerCodes[code] {
		return code
	}
	return "other"
}

// Rows recorded before #420 kept the raw body in "error". Only the status
// number and allowlisted codes are recovered from it, never the text: Jev
// wrote "status NNN", the Anthropic SDK `"<url>": NNN Status Text`.
var (
	legacyStatus = regexp.MustCompile(`status (\d{3})|": (\d{3}) [A-Z]`)
	legacyToken  = regexp.MustCompile(`[a-z_]+`)
)

// Attribution is a recorded failure's HTTP status and error code: from the
// structured fields, or recovered from a legacy row's text.
func Attribution(res RouterResult) (status int, code string) {
	status, code = res.HTTPStatus, res.ErrorCode
	if status == 0 && res.Error != "" {
		if m := legacyStatus.FindStringSubmatch(res.Error); m != nil {
			status, _ = strconv.Atoi(m[1] + m[2])
		}
		for _, token := range legacyToken.FindAllString(res.Error, -1) {
			if providerCodes[token] {
				code = token // the first allowlisted code wins
				break
			}
		}
		if code == "" && status != 0 {
			code = "other"
		}
	}
	return status, code
}
