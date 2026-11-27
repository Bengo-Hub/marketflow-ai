// Package support routes a visitor's support request from the Vera widget to the people who can
// answer it (owner, 2026-11-28):
//
//   - a technical or platform problem (the site or app not working, sign-in, a payment that fails
//     for a technical reason, a bug) becomes a ticket for Codevertex technical support on the
//     platform tenant;
//   - a question about the business itself (for Hadia: a registry, a gift, a payout, a refund, an
//     owner account, a policy) goes to the business's own staff: a ticket in its own queue when
//     its plan includes ticketing, otherwise an email to its support address and a WhatsApp
//     message to its admins sent from the platform number through notifications-api.
//
// Every step has a client timeout, a bounded retry with backoff and jitter, and a fallback that
// starts at once, so the chat never stalls (marketflow docs/agents-architecture.md, "Fallbacks
// that never stall"). The visitor always gets a ticket number, a confirmation of where the
// request went, or the business's own contact details.
package support

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/bengobox/marketflow-ai/internal/decide"
)

// Kind is what a support request is about.
type Kind string

const (
	// KindTechnical goes to Codevertex technical support (a platform ticket).
	KindTechnical Kind = "technical"
	// KindBusiness goes to the business's own staff. It is also the safe default: the business's
	// staff can forward a technical problem to Codevertex, while Codevertex cannot answer a
	// question about a registry or a refund.
	KindBusiness Kind = "business"
)

// Classification sources.
const (
	SourceRules    = "rules"
	SourceJev      = "jev"
	SourceFallback = "llm_fallback"
	SourceDefault  = "default"
)

// Classification is the classifier's answer.
type Classification struct {
	Kind       Kind    `json:"kind"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence"`
	Technical  int     `json:"technical_score"`
	Business   int     `json:"business_score"`
}

// Decider is the decide layer (Jev with the LLM fallback), the same one the WhatsApp gate uses.
type Decider interface {
	Decide(ctx context.Context, name, state string, questions map[string]decide.Question) (decide.Answers, decide.Usage, error)
}

// Thresholds for a model answer. A fallback answer is shrunk toward 0.5 by the decide layer, so
// its bar is lower. Below the bar the request takes the safe default.
const (
	JevThreshold      = 0.65
	FallbackThreshold = 0.58
	// DecideBudget is the most the classifier waits for a model; the decide layer has its own
	// 8 second limit, but a support form should not wait that long for a routing hint.
	DecideBudget = 4 * time.Second
)

// Classifier labels a request technical or business: rules first, the decide layer only when
// the rules cannot tell, and the safe default when neither answers.
type Classifier struct {
	decider Decider
	budget  time.Duration
}

// NewClassifier builds a classifier. A nil decider means rules and the safe default only.
func NewClassifier(d Decider) *Classifier { return &Classifier{decider: d, budget: DecideBudget} }

// technicalPhrases and businessPhrases are matched on the lower-cased text padded with spaces,
// so a leading or trailing space marks a word boundary. Each match counts once; strong technical
// phrases count twice because they name a fault, not a topic. English, Swahili and Sheng.
var (
	strongTechnical = []string{
		"not working", "isn't working", "is not loading", "won't load", "wont load", "doesn't load",
		"not loading", "keeps loading", "blank page", "white screen", "error ", "error:", "an error",
		" bug", "crash", "is down", "site down", "page is broken", "broken link", "404", " 500 ",
		"can't log in", "cannot log in", "can't sign in", "cannot sign in", "can't login", "cannot login",
		"unable to log in", "unable to sign in", "login failed", "sign in failed", "sign-in failed",
		"didn't get the code", "did not get the code", "no code", "code not received", "otp not",
		"stk push", "prompt did not", "prompt didn't", "no prompt", "didn't get a prompt", "did not get a prompt",
		"payment failed", "payment is failing", "payment keeps failing", "transaction failed", "checkout fails",
		"checkout failed", "timed out", "timeout", "glitch", "freez", "hangs", "stuck on",
		"haifanyi kazi", "inakataa", "imekwama", "haifunguki",
	}
	technicalWords = []string{
		" login", " log in", " sign in", " sign-in", " password", " otp", " verification code",
		" app ", " website", " the site", " page ", " browser", " button", " upload", " loading",
		" technical", " link ", " email not", " notification",
	}
	businessWords = []string{
		"registry", "registries", "gift", "gifts", "payout", "pay out", "withdraw", "withdrawal",
		"refund", "reimburse", "owner account", "my account details", "change my account", "close my account",
		"policy", "policies", "terms", "privacy", " fee", " fees", "charges", "commission", "contribution",
		"cash fund", "honeymoon", "wedding", "couple", "guest", "delivery", "deliver", "order status",
		"how much", "pricing", " price", "invoice", "receipt", "bank", "settlement", "verify my identity",
		" kyc", "id number", "partnership", "complaint", "donation",
		"zawadi", "pesa yangu", "harusi", "malipo yangu",
	}
)

// RuleScores counts the technical and business signals in text.
func RuleScores(text string) (technical, business int) {
	t := " " + strings.Join(strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return unicode.IsSpace(r)
	}), " ") + " "
	for _, p := range strongTechnical {
		if strings.Contains(t, p) {
			technical += 2
		}
	}
	for _, p := range technicalWords {
		if strings.Contains(t, p) {
			technical++
		}
	}
	for _, p := range businessWords {
		if strings.Contains(t, p) {
			business++
		}
	}
	return technical, business
}

// Classify labels text. It never errors and never waits longer than the decide budget.
//
//   - Only technical signals, with at least one strong fault phrase or two signals: technical.
//   - Only business signals: business.
//   - Both, neither, or a single weak technical word: the decide layer, then the safe default.
func (c *Classifier) Classify(ctx context.Context, text string) Classification {
	tech, biz := RuleScores(text)
	out := Classification{Technical: tech, Business: biz}
	switch {
	case tech >= 2 && biz == 0:
		out.Kind, out.Source, out.Confidence = KindTechnical, SourceRules, ruleConfidence(tech)
		return out
	case biz > 0 && tech == 0:
		out.Kind, out.Source, out.Confidence = KindBusiness, SourceRules, ruleConfidence(biz+1)
		return out
	}
	if c != nil && c.decider != nil && strings.TrimSpace(text) != "" {
		budget := c.budget
		if budget <= 0 {
			budget = DecideBudget
		}
		dctx, cancel := context.WithTimeout(ctx, budget)
		answers, usage, err := c.decider.Decide(dctx, "support_route", text, routeQuestions)
		cancel()
		if err == nil {
			a := answers["route"]
			bar, src := JevThreshold, SourceJev
			if usage.Fallback {
				bar, src = FallbackThreshold, SourceFallback
			}
			if a.Choice == string(KindTechnical) && a.Confidence >= bar {
				out.Kind, out.Source, out.Confidence = KindTechnical, src, a.Confidence
				return out
			}
			if a.Choice == string(KindBusiness) && a.Confidence >= bar {
				out.Kind, out.Source, out.Confidence = KindBusiness, src, a.Confidence
				return out
			}
		}
	}
	out.Kind, out.Source = KindBusiness, SourceDefault
	return out
}

func ruleConfidence(score int) float64 {
	c := 0.6 + 0.1*float64(score)
	if c > 0.95 {
		c = 0.95
	}
	return c
}

var routeQuestions = map[string]decide.Question{
	"route": decide.Choice("The state is a website visitor's support request to a business that runs its site and app on the Codevertex platform. "+
		"Is it a technical problem with the platform itself, which Codevertex's engineers must fix, or a question for the business's own staff? "+
		"Technical means the site or app not loading or erroring, sign-in or verification codes failing, a payment failing because of an error, or a bug. "+
		"Business means anything about the business's own service, records or rules: registries, gifts, payouts, refunds, the owner's account, fees, orders or policies. "+
		"When it is both or unclear, choose business.", map[string]string{
		string(KindTechnical): "a platform fault Codevertex must fix",
		string(KindBusiness):  "a question the business's staff answer",
	}),
}
