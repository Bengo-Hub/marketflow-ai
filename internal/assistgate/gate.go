// Package assistgate decides whether a WhatsApp message sent to the platform number is a real
// request for the MarketFlow marketing assistant (Sprint 17). Only then does it reach the
// Marketing Manager and spend a credit; anything else stays in the staff inbox untouched.
//
// Owner rule (2026-11-04): the sender must be a verified MarketFlow tenant user (checked by
// marketflow-api before it calls here), the tenant must have AI credits or an unlimited plan
// (checked by the handler), and the text itself must be a marketing, ads or UGC request. The
// text check runs in layers, cheapest first, and never blocks:
//
//  1. Code rules: length, a marketing vocabulary in English, Swahili and Sheng, and an action
//     word. A message with no marketing word at all stops here, so most chat never costs a call.
//  2. A typed judgment from the decide layer: Jev first, the router's nano (free or lightweight)
//     model when Jev is unset, out of credit, over its monthly ceiling or failing.
//  3. When both models fail, the rules decide alone with a stricter score, so an outage only
//     makes the gate more careful; it never errors.
package assistgate

import (
	"context"
	"strings"
	"unicode"

	"github.com/bengobox/marketflow-ai/internal/decide"
)

// Decider is the decide layer (Jev with the LLM fallback).
type Decider interface {
	Decide(ctx context.Context, name, state string, questions map[string]decide.Question) (decide.Answers, decide.Usage, error)
}

// Thresholds. A fallback answer is shrunk toward 0.5 by the decide layer, so its bar is lower
// than Jev's; an open assistant conversation (a reply to the assistant within the session) is a
// strong signal, so its bar is lower again.
const (
	MinRunes          = 6
	MaxRunes          = 1500
	JevThreshold      = 0.70
	FallbackThreshold = 0.62
	SessionThreshold  = 0.55
	// RulesOnlyScore is the rule score that passes when no model could answer.
	RulesOnlyScore = 3
)

// Source says which layer made the call.
const (
	SourceRules    = "rules"
	SourceJev      = "jev"
	SourceFallback = "llm_fallback"
)

// Verdict is the gate's answer.
type Verdict struct {
	Pass        bool    `json:"pass"`
	Reason      string  `json:"reason"`
	RuleScore   int     `json:"rule_score"`
	Probability float64 `json:"probability,omitempty"`
	Category    string  `json:"category,omitempty"`
	Source      string  `json:"source"`
}

// Gate checks message text.
type Gate struct{ decider Decider }

func New(d Decider) *Gate { return &Gate{decider: d} }

// Check runs the layers. sessionActive is true when the sender talked to the assistant in the
// last 30 minutes, so a short follow-up such as "make it shorter" or "sawa, post" still passes.
func (g *Gate) Check(ctx context.Context, text string, sessionActive bool) Verdict {
	text = strings.TrimSpace(text)
	n := len([]rune(text))
	if n < MinRunes && !sessionActive {
		return Verdict{Reason: "too short", Source: SourceRules}
	}
	if n > MaxRunes {
		return Verdict{Reason: "too long", Source: SourceRules}
	}
	score := RuleScore(text)
	if score == 0 && !sessionActive {
		return Verdict{Reason: "no marketing words", Source: SourceRules}
	}

	if g.decider != nil {
		answers, usage, err := g.decider.Decide(ctx, "whatsapp_gate", text, questions)
		if err == nil {
			a, c := answers["marketing_request"], answers["category"]
			bar := JevThreshold
			src := SourceJev
			if usage.Fallback {
				bar, src = FallbackThreshold, SourceFallback
			}
			if sessionActive && bar > SessionThreshold {
				bar = SessionThreshold
			}
			v := Verdict{RuleScore: score, Probability: a.Prob, Category: c.Choice, Source: src}
			switch {
			case c.Choice == "not_marketing" && c.Confidence >= 0.6 && !sessionActive:
				v.Reason = "not a marketing request"
			case a.Prob >= bar:
				v.Pass, v.Reason = true, "marketing request"
			default:
				v.Reason = "not confident it is a marketing request"
			}
			return v
		}
	}
	// No model answered: the rules decide, more strictly.
	if score >= RulesOnlyScore || (sessionActive && score >= 1) {
		return Verdict{Pass: true, Reason: "marketing words (no model available)", RuleScore: score, Source: SourceRules}
	}
	return Verdict{Reason: "not enough marketing words (no model available)", RuleScore: score, Source: SourceRules}
}

var questions = map[string]decide.Question{
	"marketing_request": decide.Noul("The state is one WhatsApp message a business owner or their staff sent to the MarketFlow assistant. " +
		"Is it asking the assistant to make, change, review, schedule, post or promote marketing for their own business: an ad, a UGC or presenter video, " +
		"a poster, captions or copy, a social post, a campaign, a boost or ad spend, or a marketing plan? " +
		"Greetings, support or billing questions, personal chat, and messages meant for a person are not."),
	"category": decide.Choice("What is the WhatsApp message in the state mainly about?", map[string]string{
		"create_content":  "make or change an ad, video, poster, caption or post",
		"publish_promote": "post, schedule, boost or spend on ads",
		"plan_advice":     "marketing plan, ideas or results",
		"not_marketing":   "anything else: greetings, support, billing, personal or unrelated",
	}),
}

// vocabulary is matched as whole words, lower case. Topic words name marketing things; action
// words ask for something to be done. English, Swahili and Sheng.
var topicWords = map[string]bool{
	"ad": true, "ads": true, "advert": true, "advertise": true, "advertising": true, "marketing": true, "campaign": true,
	"ugc": true, "video": true, "videos": true, "reel": true, "reels": true, "short": true, "shorts": true, "poster": true,
	"posters": true, "flyer": true, "banner": true, "caption": true, "captions": true, "copy": true, "script": true,
	"promo": true, "promotion": true, "offer": true, "sale": true, "discount": true, "brand": true, "logo": true,
	"tiktok": true, "instagram": true, "facebook": true, "youtube": true, "post": true, "boost": true, "audience": true,
	"customers": true, "leads": true, "presenter": true, "voiceover": true, "storyboard": true,
	"tangazo": true, "matangazo": true, "biashara": true, "wateja": true, "ofa": true, "punguzo": true, "bango": true,
}

var actionWords = map[string]bool{
	"make": true, "create": true, "design": true, "write": true, "render": true, "generate": true, "edit": true,
	"change": true, "approve": true, "publish": true, "schedule": true, "boost": true, "promote": true, "post": true,
	"launch": true, "quote": true, "need": true, "want": true,
	"tengeneza": true, "nitengenezee": true, "unda": true, "andika": true, "weka": true, "tuma": true, "nataka": true,
	"nisaidie": true, "piga": true,
}

// RuleScore counts distinct marketing topic words, plus one when an action word is present
// alongside a topic. It is cheap and deterministic, and it is the only gate when models fail.
func RuleScore(text string) int {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	seen := map[string]bool{}
	topics, action := 0, false
	for _, w := range words {
		if topicWords[w] && !seen[w] {
			seen[w] = true
			topics++
		}
		if actionWords[w] {
			action = true
		}
	}
	if topics > 0 && action {
		return topics + 1
	}
	return topics
}
