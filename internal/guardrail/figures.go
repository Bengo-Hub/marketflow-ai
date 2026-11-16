package guardrail

import (
	"regexp"
	"strconv"
	"strings"
)

// Money and percentage figures in an outward reply must come from the business: its catalogue,
// active promotions, business facts, knowledge base or a tool result. The model never decides a
// price, a discount or a deal (owner decision G3, 2026-11-16; the "$1 Tahoe" chatbot and the Air
// Canada ruling).

var (
	// "KES 1,500", "Ksh.1500", "Sh 200", "$1", "USD 20", "1,500/=", "1500 bob", "2k shillings".
	currencyBefore = regexp.MustCompile(`(?i)(?:\bkshs?\.?|\bkes\b|\bsh\.?\b|\busd\b|\$|€|£)\s?(\d[\d,]*(?:\.\d+)?)\s?(k|m|bn)?\b`)
	currencyAfter  = regexp.MustCompile(`(?i)\b(\d[\d,]*(?:\.\d+)?)\s?(k|m|bn)?\s?(?:/=|/-|\bbob\b|\bshillings?\b|\bshilingi\b|\bkes\b|\bksh\b|\busd\b|\bdollars?\b)`)
	// "20%", "20 percent", "asilimia 20".
	percentFig = regexp.MustCompile(`(?i)\b(\d{1,3}(?:\.\d+)?)\s?(?:%|percent\b|per\s+cent\b)|\basilimia\s+(\d{1,3}(?:\.\d+)?)\b`)
	// Every number in a trusted source counts as allowed.
	anyNumber = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)
)

// Figure is one money amount or percentage found in a reply.
type Figure struct {
	Text  string  // as written, for the rewrite prompt
	Value float64 // normalised
}

// Figures lists the money amounts and percentages in text.
func Figures(text string) []Figure {
	var out []Figure
	seen := map[string]bool{}
	add := func(raw, num, mult string) {
		v, ok := parseAmount(num, mult)
		if !ok || seen[raw] {
			return
		}
		seen[raw] = true
		out = append(out, Figure{Text: strings.TrimSpace(raw), Value: v})
	}
	for _, m := range currencyBefore.FindAllStringSubmatch(text, -1) {
		add(m[0], m[1], m[2])
	}
	for _, m := range currencyAfter.FindAllStringSubmatch(text, -1) {
		add(m[0], m[1], m[2])
	}
	for _, m := range percentFig.FindAllStringSubmatch(text, -1) {
		num := m[1]
		if num == "" {
			num = m[2]
		}
		add(m[0], num, "")
	}
	return out
}

func parseAmount(num, mult string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.ReplaceAll(num, ",", ""), 64)
	if err != nil {
		return 0, false
	}
	switch strings.ToLower(mult) {
	case "k":
		v *= 1_000
	case "m":
		v *= 1_000_000
	case "bn":
		v *= 1_000_000_000
	}
	return v, true
}

// AllowedNumbers collects every number in the trusted sources (facts, knowledge base, tool
// results), including k/m forms, so a reply may repeat any of them.
func AllowedNumbers(sources ...string) map[float64]bool {
	out := map[float64]bool{}
	for _, s := range sources {
		for _, f := range Figures(s) {
			out[f.Value] = true
		}
		for _, m := range anyNumber.FindAllString(s, -1) {
			if v, ok := parseAmount(m, ""); ok {
				out[v] = true
			}
		}
	}
	return out
}

// UnsupportedFigures lists the figures in reply that no trusted source carries. A zero or a 100%
// is never allowed unless a source has it: "free" and "100% off" are the classic manipulations.
func UnsupportedFigures(reply string, allowed map[float64]bool) []Figure {
	var out []Figure
	for _, f := range Figures(reply) {
		if allowed[f.Value] {
			continue
		}
		// A rounded form of an allowed amount ("about KES 1,500" for 1,499) is still the model's
		// own number, so it is reported too.
		out = append(out, f)
	}
	return out
}

// commitmentOutputPatterns match a reply that commits the business to something: a binding offer,
// a guarantee, a promise of a refund or something free.
var commitmentOutputPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\blegally\s+binding\b`),
	regexp.MustCompile(`(?i)\b(this|that|it)\s+is\s+a\s+(binding|firm)\s+(offer|deal|agreement)\b`),
	regexp.MustCompile(`(?i)\bno\s+takesies?\s+backsies\b`),
	regexp.MustCompile(`(?i)\bi\s+(promise|guarantee)\s+(you\s+)?(a\s+|that\s+|we\s+will\s+|you\s+will\s+)?(get\s+)?(a\s+)?(refund|discount|free|full\s+refund|money\s+back)`),
	regexp.MustCompile(`(?i)\b(you('ll| will)\s+get|you\s+can\s+have)\s+(it|this|one)\s+for\s+free\b`),
	regexp.MustCompile(`(?i)\b(i|we)\s+(agree|accept)\s+(to\s+)?(sell|give|offer)\s+(it|this|you)\b.{0,30}\bfor\b`),
	regexp.MustCompile(`(?i)\bni\s+makubaliano\s+ya\s+kisheria\b`),
}

// Commitments lists the phrases in reply that commit the business to an offer, a refund, a
// guarantee or a binding agreement.
func Commitments(reply string) []string {
	n := Normalize(reply)
	var out []string
	for _, re := range commitmentOutputPatterns {
		if m := re.FindString(n); m != "" {
			out = append(out, strings.TrimSpace(m))
		}
	}
	return out
}

// OutputViolations lists, in words a model can act on, every figure and commitment in an outward
// reply that the business did not give. Empty means the reply may go out.
func OutputViolations(reply string, allowed map[float64]bool) []string {
	var out []string
	for _, f := range UnsupportedFigures(reply, allowed) {
		out = append(out, "Price, amount or percentage not in the business facts, knowledge base or tool results (you wrote \""+f.Text+"\"). Use only figures you were given, or say you do not have that detail")
	}
	for _, c := range Commitments(reply) {
		out = append(out, "Commitment the business never made (you wrote \""+c+"\"). Never agree to binding offers, refunds, free items or guarantees")
	}
	return out
}
