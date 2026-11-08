package team

import (
	"strings"
	"unicode"
)

// The Find step learns structure from other ads, never words. Competitor text never reaches the
// writer, but an owner can paste an ad's copy into a Reference ad's notes, so every outward draft
// is compared with those notes and blocked if it repeats them.
const (
	// copyRun is the longest run of words a draft may share with a reference in order.
	copyRun = 6
	// shingleSize and shingleLimit catch a lightly reworded copy: the share of the draft's
	// 3-word sequences that also appear in one reference.
	shingleSize  = 3
	shingleLimit = 0.5
)

// copiedFrom reports whether draft repeats any reference closely enough to count as a copy, and
// the words it shares, for the finding.
func copiedFrom(draft string, references []string) (bool, string) {
	d := words(draft)
	if len(d) < copyRun {
		return false, ""
	}
	for _, ref := range references {
		r := words(ref)
		if len(r) < copyRun {
			continue
		}
		if run := longestSharedRun(d, r); len(run) >= copyRun {
			return true, strings.Join(run, " ")
		}
		if overlap(d, r) >= shingleLimit {
			return true, strings.Join(r[:min(len(r), copyRun)], " ")
		}
	}
	return false, ""
}

// words lowercases and splits on anything that is not a letter or digit, so punctuation and case
// changes do not hide a copy. Works the same for English and Swahili.
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// longestSharedRun is the longest run of consecutive words both texts contain in the same order.
func longestSharedRun(a, b []string) []string {
	best, end := 0, 0
	prev := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
				if cur[j] > best {
					best, end = cur[j], i
				}
			}
		}
		prev = cur
	}
	return a[end-best : end]
}

// overlap is the share of the draft's 3-word sequences that also appear in the reference.
func overlap(draft, ref []string) float64 {
	if len(draft) < shingleSize || len(ref) < shingleSize {
		return 0
	}
	in := map[string]bool{}
	for i := 0; i+shingleSize <= len(ref); i++ {
		in[strings.Join(ref[i:i+shingleSize], " ")] = true
	}
	hit, total := 0, 0
	for i := 0; i+shingleSize <= len(draft); i++ {
		total++
		if in[strings.Join(draft[i:i+shingleSize], " ")] {
			hit++
		}
	}
	return float64(hit) / float64(total)
}

// referenceTexts are the owner's notes on reference ads, the only outside ad text the team sees.
func referenceTexts(m *MarketSignals) []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.References))
	for _, r := range m.References {
		if strings.TrimSpace(r.Notes) != "" {
			out = append(out, r.Notes)
		}
	}
	return out
}
