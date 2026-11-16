package guardrail

import (
	"regexp"
	"strings"
)

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// Phone-like runs: optional +, then 9 to 15 digits with spaces, dots or dashes.
	phoneRe = regexp.MustCompile(`\+?\d[\d\s.\-]{7,18}\d`)
	// Keys and tokens in URLs, for error text that may reach a model or a log.
	secretParamRe = regexp.MustCompile(`(?i)([?&](?:key|api_key|apikey|token|access_token|secret|signature|sig)=)[^&\s"']+`)
)

// MaskPII replaces emails and phone numbers, so lead contact details never reach a model
// provider. Names, amounts and short numbers stay.
func MaskPII(s string) string {
	s = emailRe.ReplaceAllString(s, "[email]")
	return phoneRe.ReplaceAllStringFunc(s, func(m string) string {
		digits := 0
		for _, r := range m {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		if digits < 9 {
			return m
		}
		return "[phone]"
	})
}

// RedactSecrets hides keys and tokens carried in URL query strings (a Google API error wraps the
// request URL, key included).
func RedactSecrets(s string) string {
	if !strings.ContainsAny(s, "?&") {
		return s
	}
	return secretParamRe.ReplaceAllString(s, "${1}[redacted]")
}
