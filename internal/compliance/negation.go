package compliance

import (
	"regexp"
	"strings"
)

// Refusals that name a medicine (owner, 2026-11-14). A pharmacy may say "We can't offer discounts
// on Ozempic" or "Hatutoi punguzo kwa dawa za prescription"; "Big discount on Ozempic today" stays
// blocked. A block with allow_negated marks its promotion phrase with (?P<promo>...) groups, and a
// match is let through when a negation stands before that phrase in the same clause. Go's RE2 has
// no lookbehind, so the check is done here in code.

// promoGroup is the subexpression name that marks a block's promotion phrase.
const promoGroup = "promo"

// negationRe lists the negations that turn a promotion phrase into a refusal: English and the
// Swahili forms a pharmacy writes ("hatuwezi" we cannot, "hatutoi" we do not give, "hatuna" we do
// not have, "hakuna" there is no, "si" and "sio" is not). "can not", "do not" and "is not" are
// covered by "not".
var negationRe = regexp.MustCompile(`(?i)\b(can['’]?t|cannot|don['’]?t|doesn['’]?t|won['’]?t|isn['’]?t|aren['’]?t|no|never|not|unable\s+to|hatuwezi|hatutoi|hatuna|hakuna|si|sio)\b`)

// negationIdiomRe is what may follow a negation and still make a sales line: "don't miss",
// "never been cheaper", "not only", "no better time". A negation followed by one of these does
// not count.
var negationIdiomRe = regexp.MustCompile(`(?i)^\s+(miss(ing)?|wait(ing)?|delay|hesitate|forget|worry|better|been|only|cheaper|lower)\b`)

// clauseBreaks end a clause: a negation in an earlier clause ("No prescription? 20% off
// Panadol") does not reach the promotion phrase.
const clauseBreaks = ".!?;:,\n"

// find returns the first match of the block that is a violation. For an allow_negated block a
// match whose promotion phrase is negated is skipped and the search goes on from the next word, so
// a refusal earlier in the text does not hide an offer later in it. A match whose medicine word
// starts an exempt phrase ("dawa ya meno") is searched again with that phrase blanked out, so the
// same promotion phrase still catches a later real medicine.
func (b *Block) find(text string) (string, bool) {
	if !b.AllowNegated && b.exemptRe == nil {
		loc := b.re.FindStringIndex(text)
		if loc == nil {
			return "", false
		}
		return text[loc[0]:loc[1]], true
	}
	// work is the text with exempt phrases blanked out (same length, so positions stay valid): after
	// "dawa ya meno" is blanked, the same promotion phrase is tried again against a later real
	// medicine, so "Punguzo kwa dawa ya meno na Panadol" is still caught.
	work := text
	for pos := 0; pos <= len(work); {
		loc := b.re.FindStringSubmatchIndex(work[pos:])
		if loc == nil {
			return "", false
		}
		start, end := pos+loc[0], pos+loc[1]
		if b.exemptRe != nil {
			if med := groupStart(loc, b.med, pos, -1); med >= 0 {
				if ex := b.exemptRe.FindStringIndex(work[med:]); ex != nil {
					work = work[:med] + strings.Repeat(" ", ex[1]) + work[med+ex[1]:]
					continue // search again from the same place without the household phrase
				}
			}
		}
		if b.AllowNegated && negatedBefore(text, groupStart(loc, b.promo, pos, start)) {
			pos = nextWordStart(work, start)
			continue
		}
		return text[start:end], true
	}
	return "", false
}

// medGroup is the subexpression name that marks a block's medicine word (Block.Exempt).
const medGroup = "med"

// groupStart is the start in text of the first of groups that took part in the match (loc is
// relative to pos), or def when none did.
func groupStart(loc []int, groups []int, pos, def int) int {
	for _, g := range groups {
		if loc[2*g] >= 0 {
			return pos + loc[2*g]
		}
	}
	return def
}

// negatedBefore reports whether a negation stands before position p in the same clause and is
// not part of a sales idiom ("don't miss").
func negatedBefore(text string, p int) bool {
	from := strings.LastIndexAny(text[:p], clauseBreaks) + 1
	clause := text[from:p]
	for _, m := range negationRe.FindAllStringIndex(clause, -1) {
		if !negationIdiomRe.MatchString(text[from+m[1]:]) {
			return true
		}
	}
	return false
}

// nextWordStart is the first position after i where a new word starts (the byte before it is not
// an ASCII word character, the same rule RE2's \b uses), so searching text[next:] treats \b the
// way it would in the whole text.
func nextWordStart(text string, i int) int {
	next := i + 1
	for next < len(text) && isWordByte(text[next-1]) && isWordByte(text[next]) {
		next++
	}
	return next
}

func isWordByte(c byte) bool {
	return c == '_' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}
