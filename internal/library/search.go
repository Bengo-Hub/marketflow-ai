package library

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// stopwords are dropped before matching (English and common Swahili fillers).
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "for": true, "to": true, "of": true,
	"in": true, "on": true, "my": true, "me": true, "i": true, "is": true, "it": true, "with": true,
	"this": true, "that": true, "you": true, "your": true, "our": true, "we": true, "be": true,
	"na": true, "ya": true, "wa": true, "kwa": true, "la": true, "za": true, "ni": true, "hii": true,
	"want": true, "need": true, "please": true, "help": true, "make": true, "nataka": true,
}

// Tokenize lowercases and splits text into searchable words.
func Tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := fields[:0]
	for _, f := range fields {
		if len(f) > 1 && !stopwords[f] {
			out = append(out, f)
		}
	}
	return out
}

func overlap(a, b []string) int {
	set := make(map[string]bool, len(b))
	for _, t := range b {
		set[t] = true
	}
	n := 0
	for _, t := range a {
		if set[t] {
			n++
		}
	}
	return n
}

func (l *Library) buildIndex() {
	l.idxOnce.Do(func() {
		l.pbIdx = make(map[string][]string, len(l.playbooks))
		for k, p := range l.playbooks {
			l.pbIdx[k] = Tokenize(p.Name + " " + p.WhenToUse + " " + strings.Join(p.Keywords, " "))
		}
		l.prIdx = make([][]string, len(l.prompts))
		for i, p := range l.prompts {
			l.prIdx[i] = Tokenize(p.Act + " " + strings.Join(p.Tags, " "))
		}
	})
}

// Match is a ranked playbook hit.
type Match struct {
	Key   string  `json:"key"`
	Score float64 `json:"score"`
}

// SearchPlaybooks ranks playbooks for a request. Keyword phrases found in the
// request weigh most (they carry Swahili and slang synonyms), then word overlap
// with the name and when-to-use text. It feeds the decide layer's candidate list
// and the fallback router when no decision adapter answers.
func (l *Library) SearchPlaybooks(query string, k int) []Match {
	l.buildIndex()
	q := " " + strings.ToLower(query) + " "
	tokens := Tokenize(query)
	var out []Match
	for _, key := range l.order {
		p := l.playbooks[key]
		score := float64(overlap(tokens, l.pbIdx[key]))
		for _, kw := range p.Keywords {
			if strings.Contains(q, " "+strings.ToLower(kw)) {
				// A matched phrase is stronger evidence than a matched word.
				score += 2 * float64(len(strings.Fields(kw)))
			}
		}
		if score > 0 {
			out = append(out, Match{Key: key, Score: score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out
}

// SearchPrompts returns indexes of curated prompts ranked by overlap with query.
func (l *Library) SearchPrompts(query string, k int) []int {
	l.buildIndex()
	tokens := Tokenize(query)
	type hit struct{ i, score int }
	var hits []hit
	for i, idx := range l.prIdx {
		if s := overlap(tokens, idx); s > 0 {
			hits = append(hits, hit{i, s})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	out := make([]int, 0, k)
	for _, h := range hits {
		if k > 0 && len(out) >= k {
			break
		}
		out = append(out, h.i)
	}
	return out
}

// Brief renders a playbook as the instruction block a specialist receives,
// after the house rules. Stable output (no timestamps) keeps it cacheable.
func (l *Library) Brief(p Playbook) string {
	var b strings.Builder
	b.WriteString("PLAYBOOK: ")
	b.WriteString(p.Name)
	b.WriteString("\nUse when: ")
	b.WriteString(p.WhenToUse)
	b.WriteString("\nMethod:\n")
	for i, m := range p.Method {
		b.WriteString("  ")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(m)
		b.WriteString("\n")
	}
	if len(p.Rules) > 0 {
		b.WriteString("Playbook rules:\n")
		for _, r := range p.Rules {
			b.WriteString("  - ")
			b.WriteString(r)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// HouseRulesText renders the house rules block.
func (l *Library) HouseRulesText() string {
	var b strings.Builder
	b.WriteString("HOUSE RULES (always apply):\n")
	for _, r := range l.HouseRules {
		b.WriteString("- ")
		b.WriteString(r)
		b.WriteString("\n")
	}
	return b.String()
}
