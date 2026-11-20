package team

import (
	"fmt"
	"regexp"
	"strings"
)

// Offers in a script must come from the brief (2026-11-19 audit, finding 10): the writer once
// closed a POS ad on a "free demo session" the business never offered. GuardOffers finds offers
// and calls to action the brief's offering text does not back, takes them out of the script and
// reports each one, so the owner reviews the change before anything is rendered.

// offerRule is one kind of offer and the words in the brief that would back it.
type offerRule struct {
	re   *regexp.Regexp
	back []string
	name string
}

var offerRules = []offerRule{
	{regexp.MustCompile(`(?i)\bfree\s+(?:\w+\s+){0,2}?(?:demo|demos|trial|trials|consultation|delivery|shipping|session|sessions|class|classes|lesson|lessons|installation|setup|training|month|week|sample|samples|gift|quote|assessment|audit|test drive|account)\b`),
		[]string{"free"}, "a free offer"},
	{regexp.MustCompile(`(?i)\b\d{1,2}\s?%\s?(?:off|discount)\b|\bpercent off\b|\bdiscount(?:s|ed)?\b|\bhalf[- ]price\b|\bpunguzo\b`),
		[]string{"discount", "% off", "%off", "percent off", "half price", "half-price", "punguzo"}, "a discount"},
	{regexp.MustCompile(`(?i)\bbuy one,? get one\b|\bbogof?\b|\b2 for 1\b|\btwo for one\b`),
		[]string{"buy one", "bogo", "2 for 1", "two for one"}, "a bundle offer"},
	{regexp.MustCompile(`(?i)\bmoney[- ]back\b|\bfull refund\b|\bno questions asked\b`),
		[]string{"money back", "money-back", "refund"}, "a refund promise"},
	{regexp.MustCompile(`(?i)\b(?:special|limited|launch|exclusive)\s+offer\b|\blimited time\b|\bwhile stocks last\b`),
		[]string{"offer", "limited", "while stocks last"}, "a time-limited offer"},
	{regexp.MustCompile(`(?i)\bpromo(?:tion)?\s+code\b|\bcoupon\b|\bvoucher\b|\bcashback\b`),
		[]string{"promo", "promotion code", "coupon", "voucher", "cashback"}, "a promotion"},
	{regexp.MustCompile(`(?i)\bbure\b`), []string{"bure", "free"}, "a free offer"},
	// Scarcity the brief does not state is invented urgency (marketing laws: urgency only when true).
	{regexp.MustCompile(`(?i)\bspots?\s+(?:fill|are filling|go)\s+fast\b|\blimited\s+(?:spots|places|seats|slots)\b|\bonly\s+\d+\s+(?:spots|places|seats|slots)\s+left\b|\bselling\s+fast\b|\bhurry\b`),
		[]string{"limited", "spots left", "places left", "seats left"}, "scarcity"},
	// A phone number in the voice or captions is never the writer's to give: the end card draws the
	// business's real contacts (media/contacts.go), and a written number may be made up.
	{regexp.MustCompile(`(?:\+?254[\s-]?|\b0)[17]\d{2}[\s-]?\d{3}[\s-]?\d{3}\b`), []string{"\x00"}, "a phone number"},
}

// backed reports whether the brief's text holds any of the words.
func backed(facts string, words []string) bool {
	for _, w := range words {
		if strings.Contains(facts, w) {
			return true
		}
	}
	return false
}

// sentenceSplit cuts a line into sentences, keeping their end marks.
var sentenceSplit = regexp.MustCompile(`[^.!?]+[.!?]*`)

// dropOffer removes every sentence of s that makes the offer; it returns the rest and whether
// anything was removed.
func dropOffer(s string, re *regexp.Regexp) (string, bool) {
	if !re.MatchString(s) {
		return s, false
	}
	var kept []string
	for _, sent := range sentenceSplit.FindAllString(s, -1) {
		if t := strings.TrimSpace(sent); t != "" && !re.MatchString(t) {
			kept = append(kept, t)
		}
	}
	return strings.Join(kept, " "), true
}

// GuardOffers removes the offers the brief does not back from the script's voiceover, captions,
// hooks and CTA, and returns a finding for each. facts is the brief's offering text (name,
// description, price, promotion code and end date) and anything the owner wrote in the request.
func GuardOffers(v *VideoScript, facts string) []Finding {
	if v == nil {
		return nil
	}
	facts = strings.ToLower(facts)
	var out []Finding
	report := func(where, quote, name string) {
		out = append(out, Finding{Quote: quote, Severity: "review",
			Issue: fmt.Sprintf("The %s promised %s that the brief does not offer.", where, name),
			Fix:   "Removed. Add the offer to the brief first if the business really makes it."})
	}
	for _, r := range offerRules {
		if backed(facts, r.back) {
			continue
		}
		for i := range v.Scenes {
			sc := &v.Scenes[i]
			if m := r.re.FindString(sc.Voiceover); m != "" {
				sc.Voiceover, _ = dropOffer(sc.Voiceover, r.re)
				report(fmt.Sprintf("voiceover of scene %d", i+1), m, r.name)
			}
			if m := r.re.FindString(sc.Caption); m != "" {
				sc.Caption, _ = dropOffer(sc.Caption, r.re)
				report(fmt.Sprintf("caption of scene %d", i+1), m, r.name)
			}
		}
		hooks := v.Hooks[:0]
		for _, h := range v.Hooks {
			if m := r.re.FindString(h); m != "" {
				report("hook", m, r.name)
				if h, _ = dropOffer(h, r.re); strings.TrimSpace(h) == "" {
					continue
				}
			}
			hooks = append(hooks, h)
		}
		v.Hooks = hooks
		if m := r.re.FindString(v.CTA); m != "" {
			v.CTA, _ = dropOffer(v.CTA, r.re)
			report("call to action", m, r.name)
		}
	}
	return out
}
