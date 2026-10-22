package promptopt

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Parsed holds what deterministic parsing found in the owner's text.
type Parsed struct {
	BudgetKES  float64  `json:"budget_kes,omitempty"`
	BudgetText string   `json:"budget_text,omitempty"`
	PeriodDays int      `json:"period_days,omitempty"`
	PeriodText string   `json:"period_text,omitempty"`
	Platforms  []string `json:"platforms,omitempty"`
	URLs       []string `json:"urls,omitempty"`
	Language   string   `json:"language"` // en | sw | mixed
}

var (
	// "KES 3,000", "ksh 3k", "3k", "sh. 2500", "budget 5000", "3.5k", "10m".
	amountRe = regexp.MustCompile(`(?i)(?:\b(?:kes|kshs?|shs?|sh\.|shillings?|shilingi|budget|bajeti)\s*\.?\s*)?(\d[\d,]*(?:\.\d+)?)\s*(k|m|thousand|million|elfu|milioni)?\b`)
	// Swahili number words after "elfu" (thousand): "elfu tatu" = 3,000.
	swNumbers = map[string]float64{
		"moja": 1, "mbili": 2, "tatu": 3, "nne": 4, "tano": 5, "sita": 6, "saba": 7, "nane": 8, "tisa": 9, "kumi": 10,
		"ishirini": 20, "thelathini": 30, "arobaini": 40, "hamsini": 50, "mia": 100,
	}
	elfuRe = regexp.MustCompile(`(?i)\belfu\s+([a-z]+)(?:\s+na\s+([a-z]+))?`)
	urlRe  = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"']+|\bwww\.[^\s<>"']+`)

	periodPatterns = []struct {
		re   *regexp.Regexp
		days func([]string) int
	}{
		{regexp.MustCompile(`(?i)\b(\d{1,3})\s*(days?|siku)\b`), func(m []string) int { return atoi(m[1]) }},
		{regexp.MustCompile(`(?i)\bsiku\s+(\d{1,3})\b`), func(m []string) int { return atoi(m[1]) }},
		{regexp.MustCompile(`(?i)\b(\d{1,2})\s*(weeks?|wiki)\b`), func(m []string) int { return 7 * atoi(m[1]) }},
		{regexp.MustCompile(`(?i)\bwiki\s+(\d{1,2})\b`), func(m []string) int { return 7 * atoi(m[1]) }},
		{regexp.MustCompile(`(?i)\b(\d{1,2})\s*(months?|miezi|mwezi)\b`), func(m []string) int { return 30 * atoi(m[1]) }},
		{regexp.MustCompile(`(?i)\b(this|next|one|a)\s+week\b|\bwiki\s+(hii|ijayo|moja)\b|\bweekly\b`), func([]string) int { return 7 }},
		{regexp.MustCompile(`(?i)\b(this|next|one|a)\s+month\b|\bmwezi\s+(huu|ujao|mmoja)\b|\bmonthly\b`), func([]string) int { return 30 }},
		{regexp.MustCompile(`(?i)\b(this\s+)?weekend\b|\bwikendi\b`), func([]string) int { return 2 }},
		{regexp.MustCompile(`(?i)\btoday\b|\bleo\b`), func([]string) int { return 1 }},
		{regexp.MustCompile(`(?i)\btomorrow\b|\bkesho\b`), func([]string) int { return 1 }},
	}

	platformWords = map[string][]string{
		"facebook":  {"facebook", "fb", "meta"},
		"instagram": {"instagram", "ig", "insta"},
		"tiktok":    {"tiktok", "tik tok"},
		"youtube":   {"youtube", "yt", "shorts"},
		"google":    {"google ads", "google search", "adwords"},
		"whatsapp":  {"whatsapp", "wasap", "watsapp", "status"},
		"linkedin":  {"linkedin"},
		"x":         {"twitter", "x.com"},
	}

	swMarkers = map[string]bool{
		"nataka": true, "naomba": true, "wiki": true, "hii": true, "tangazo": true, "matangazo": true, "bei": true,
		"wateja": true, "biashara": true, "yangu": true, "wangu": true, "kwa": true, "na": true, "ya": true,
		"la": true, "za": true, "ni": true, "kuna": true, "gani": true, "nini": true, "sana": true, "bidhaa": true,
		"huduma": true, "mauzo": true, "leo": true, "kesho": true, "siku": true, "mwezi": true, "elfu": true,
		"tafadhali": true, "nisaidie": true, "tengeneza": true, "andika": true, "picha": true, "ujumbe": true,
	}
)

// Parse runs the deterministic extractors.
func Parse(text string) Parsed {
	p := Parsed{Language: detectLanguage(text)}
	lower := strings.ToLower(text)
	p.URLs = urlRe.FindAllString(text, 5)
	textNoURL := urlRe.ReplaceAllString(text, " ")

	if v, raw := parseBudget(textNoURL); v > 0 {
		p.BudgetKES, p.BudgetText = v, raw
	}
	for _, pp := range periodPatterns {
		if m := pp.re.FindStringSubmatch(textNoURL); m != nil {
			if d := pp.days(m); d > 0 && d <= 366 {
				p.PeriodDays, p.PeriodText = d, strings.TrimSpace(m[0])
				break
			}
		}
	}
	padded := " " + strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' {
			return r
		}
		return ' '
	}, lower) + " "
	for _, name := range []string{"facebook", "instagram", "tiktok", "youtube", "google", "whatsapp", "linkedin", "x"} {
		for _, w := range platformWords[name] {
			if strings.Contains(padded, " "+w+" ") {
				p.Platforms = append(p.Platforms, name)
				break
			}
		}
	}
	return p
}

// parseBudget finds the money amount. Amounts tied to a currency or budget word
// win; a bare number counts only when it has a size suffix ("3k") or is at least
// 100, so "30 seconds" or "2 posters" are never read as money.
func parseBudget(text string) (float64, string) {
	if m := elfuRe.FindStringSubmatch(text); m != nil {
		if n, ok := swNumbers[strings.ToLower(m[1])]; ok {
			v := n * 1000
			if m[2] != "" {
				if extra, ok := swNumbers[strings.ToLower(m[2])]; ok {
					v += extra * 1000
				}
			}
			return v, m[0]
		}
	}
	best, bestRaw, bestStrong := 0.0, "", false
	for _, m := range amountRe.FindAllStringSubmatchIndex(text, -1) {
		full := text[m[0]:m[1]]
		num := text[m[2]:m[3]]
		suffix := ""
		if m[4] >= 0 {
			suffix = strings.ToLower(text[m[4]:m[5]])
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(num, ",", ""), 64)
		if err != nil || v <= 0 {
			continue
		}
		switch suffix {
		case "k", "thousand", "elfu":
			v *= 1000
		case "m", "million", "milioni":
			v *= 1_000_000
		}
		strong := hasCurrencyWord(full) || suffix != ""
		if !strong && v < 100 {
			continue
		}
		if follows(text, m[1], "seconds", "secs", "sec", "s ", "posts", "posters", "videos", "ads", "days", "siku", "weeks", "percent", "%") {
			continue
		}
		if strong && !bestStrong || (strong == bestStrong && v > best) {
			best, bestRaw, bestStrong = math.Round(v), strings.TrimSpace(full), strong
		}
	}
	return best, bestRaw
}

func hasCurrencyWord(s string) bool {
	l := strings.ToLower(s)
	for _, w := range []string{"kes", "ksh", "sh", "shilling", "shilingi", "budget", "bajeti"} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

// follows reports whether the text right after end starts with one of the words.
func follows(text string, end int, words ...string) bool {
	rest := strings.ToLower(strings.TrimLeft(text[end:], " "))
	for _, w := range words {
		if strings.HasPrefix(rest, w) {
			return true
		}
	}
	return false
}

func detectLanguage(text string) string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) })
	if len(words) == 0 {
		return "en"
	}
	sw := 0
	for _, w := range words {
		if swMarkers[w] {
			sw++
		}
	}
	ratio := float64(sw) / float64(len(words))
	switch {
	case ratio >= 0.3:
		return "sw"
	case sw >= 2 || ratio >= 0.12:
		return "mixed"
	default:
		return "en"
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
