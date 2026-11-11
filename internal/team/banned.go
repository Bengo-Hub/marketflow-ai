package team

import (
	"encoding/json"
	"regexp"
	"strings"
)

// The business's own banned claims (brand context `banned_claims`) are enforced in code:
// a draft that says one is blocked, whatever the model did with the instruction.

// BannedClaims reads the banned claims from a brand context JSON. A context that is not JSON
// has none.
func BannedClaims(marketingContext string) []string {
	var bc struct {
		BannedClaims []string `json:"banned_claims"`
	}
	if json.Unmarshal([]byte(marketingContext), &bc) != nil {
		return nil
	}
	var out []string
	for _, c := range bc.BannedClaims {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// MatchBanned returns the first claim the text makes, matched as whole words, ignoring case
// and extra spaces; "" when none.
func MatchBanned(text string, claims []string) string {
	for _, c := range claims {
		words := strings.Fields(c)
		if len(words) == 0 {
			continue
		}
		for i, w := range words {
			words[i] = regexp.QuoteMeta(w)
		}
		re, err := regexp.Compile(`(?i)(^|[^\pL\pN])` + strings.Join(words, `\s+`) + `($|[^\pL\pN])`)
		if err == nil && re.MatchString(text) {
			return c
		}
	}
	return ""
}
