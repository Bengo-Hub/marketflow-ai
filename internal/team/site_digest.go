package team

import (
	"regexp"
	"strings"
)

// A business's website reaches the team as the crawler's main text. Pasted whole it brings
// menus, cookie banners, footers and the "trusted by" strip of client names that a software
// house shows (2026-11-19 audit: Codevertex's own site lists its clients). DigestSiteText keeps
// the lines that state facts about the business itself and drops the rest.

// siteDigestChars bounds the digest in a prompt.
const siteDigestChars = 1500

// boilerplate matches lines that carry no fact about the business.
var boilerplate = regexp.MustCompile(`(?i)(cookie|privacy policy|terms (and|&) conditions|terms of (use|service)|all rights reserved|©|copyright|` +
	`subscribe|newsletter|sign up|sign in|log ?in|create account|skip to|back to top|toggle (menu|navigation)|follow us|share this|` +
	`powered by|made with|read more|learn more|click here|load more|view all|accept all|javascript)`)

// clientStrip matches the heading of a list of other businesses: clients, partners, logos.
var clientStrip = regexp.MustCompile(`(?i)^(our )?(clients|partners|customers|brands|sponsors)\b|trusted by|they trust us|as seen (on|in)|` +
	`companies (that|who) (trust|use)|our (clients|partners|customers) include|in partnership with|logos?\b`)

// DigestSiteText returns the website's facts: lines of at least four words that are not
// boilerplate, with the list of client or partner names under a "clients" heading left out,
// duplicates dropped, at most siteDigestChars long.
func DigestSiteText(text string) string {
	var b strings.Builder
	seen := map[string]bool{}
	skipping := false
	var pieces []string
	for _, raw := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' || r == '|' }) {
		// Text without line breaks is read sentence by sentence.
		if len(raw) > 300 {
			pieces = append(pieces, sentenceSplit.FindAllString(raw, -1)...)
			continue
		}
		pieces = append(pieces, raw)
	}
	for _, raw := range pieces {
		line := strings.Join(strings.Fields(raw), " ")
		if line == "" {
			continue
		}
		words := len(strings.Fields(line))
		if clientStrip.MatchString(line) {
			// The heading and the short names under it, until a real sentence again.
			skipping = true
			continue
		}
		if skipping && words < 6 {
			continue
		}
		skipping = false
		if words < 4 || boilerplate.MatchString(line) {
			continue
		}
		key := strings.ToLower(line)
		if seen[key] {
			continue
		}
		seen[key] = true
		if b.Len()+len(line)+1 > siteDigestChars {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}
