package team

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/bengobox/marketflow-ai/internal/compliance"
	"github.com/bengobox/marketflow-ai/internal/library"
	"github.com/bengobox/marketflow-ai/internal/promptopt"
)

// The owner's marketing laws (2026-11-17) in the review. The guidance reaches the writer through
// compliance.PromptRules; here the review checks the parts code can check:
//   - pressure without a fact (compliance.LawFindings): urgency, scarcity or a "was" price the
//     facts do not back is handled like a banned claim (one rewrite, then blocked); an unproven
//     "best" or "No. 1" is a finding for the owner;
//   - format (lawFormatFindings): every draft opens with a hook and ends with one specific call
//     to action on one channel; a poster headline is short. These are advice (severity low).
// The rest (relevance, emotion, identity, perceived value, repetition) is scored by the decide
// call in review.go and only moves the ranking.

// copyPlaybooks are plain-text playbooks whose answer is copy the owner sends as it is, so the
// marketing laws' blocking checks apply to them like to the structured drafts.
var copyPlaybooks = map[string]bool{"copywriting": true, "copy-editing": true, "emails": true, "whatsapp-sms": true, "sales-enablement": true}

// lawFacts is what a draft may rely on for a deadline, a stock figure, an earlier price or a
// ranking: the brand context and the owner's request as the optimiser wrote it (which carries the
// Studio's offering facts and promotion end date).
func lawFacts(tc TenantContext, spec *promptopt.TaskSpec) string {
	var b strings.Builder
	b.WriteString(tc.MarketingContext)
	for _, o := range tc.Offerings {
		b.WriteString("\n")
		b.WriteString(o)
	}
	if spec != nil {
		b.WriteString("\n")
		b.WriteString(spec.OptimisedBrief)
		b.WriteString("\n")
		b.WriteString(spec.Original)
	}
	return b.String()
}

// lawChecked reports whether a step's drafts get the marketing laws' blocking checks.
func lawChecked(st *Step) bool {
	return outward[st.Contract] || copyPlaybooks[st.Playbook]
}

// lawFindingsFor turns the pressure checks on one draft into review findings. blocked is true when
// one of them must block the draft.
func lawFindingsFor(id, text, facts string) (out []Finding, blocked bool) {
	for _, f := range compliance.LawFindings(text, facts) {
		sev := "review"
		if f.Block {
			sev, blocked = "high", true
		}
		out = append(out, Finding{Quote: id + ": " + f.Quote, Issue: "marketing law (" + f.Law + "): " + f.Reason, Severity: sev, Fix: f.Fix})
	}
	return out, blocked
}

// channelRes caches the compiled channel patterns per list entry (reviews run concurrently).
var (
	channelMu  sync.Mutex
	channelRes = map[string]*regexp.Regexp{}
)

func channelRe(entry string) *regexp.Regexp {
	channelMu.Lock()
	defer channelMu.Unlock()
	if re, ok := channelRes[entry]; ok {
		return re
	}
	var alts []string
	for _, w := range strings.Split(entry, "|") {
		if w = strings.TrimSpace(w); w != "" {
			alts = append(alts, regexp.QuoteMeta(strings.ToLower(w)))
		}
	}
	if len(alts) == 0 {
		return nil
	}
	re := regexp.MustCompile(`(^|[^\pL\pN])(` + strings.Join(alts, "|") + `)($|[^\pL\pN])`)
	channelRes[entry] = re
	return re
}

// ctaChannels lists the distinct channels a call to action names, by their first word in the list.
func ctaChannels(cta string, channels []string) []string {
	text := strings.ToLower(cta)
	var out []string
	for _, entry := range channels {
		if re := channelRe(entry); re != nil && re.MatchString(text) {
			out = append(out, strings.TrimSpace(strings.Split(entry, "|")[0]))
		}
	}
	return out
}

// multiCTAFinding is a finding when a call to action asks for two or more channels at once.
func multiCTAFinding(cta string, channels []string) (Finding, bool) {
	if got := ctaChannels(cta, channels); len(got) > 1 {
		return Finding{Quote: cta, Severity: "low", Issue: "the call to action asks for more than one next step (" + strings.Join(got, ", ") + ")",
			Fix: "Keep one channel: one WhatsApp message, one M-Pesa payment or one form."}, true
	}
	return Finding{}, false
}

// ctaFinding checks one call to action: present, specific and on one channel.
func ctaFinding(id, cta string, q library.ScriptQuality) (Finding, bool) {
	cta = strings.TrimSpace(cta)
	switch {
	case cta == "":
		return Finding{Quote: id, Severity: "low", Issue: "no call to action",
			Fix: "End on one specific action, what they get and the channel."}, true
	case weakCTA(cta, q.WeakCTAs):
		return Finding{Quote: id + ": " + cta, Severity: "low", Issue: "the call to action is generic",
			Fix: "Name the action, what they get and the channel, for example the WhatsApp word to send."}, true
	}
	if f, ok := multiCTAFinding(cta, q.CTAChannels); ok {
		f.Quote = id + ": " + cta
		return f, true
	}
	return Finding{}, false
}

// maxPosterHeadlineWords keeps a poster headline readable at a glance (the poster-brief playbook).
const maxPosterHeadlineWords = 8

// lawFormatFindings checks the structured drafts for a hook and one specific call to action. The
// video script gets the same checks in QualityFindings. Advice only (severity low).
func lawFormatFindings(st *Step, q library.ScriptQuality) []Finding {
	var out []Finding
	noHook := func(id string) Finding {
		return Finding{Quote: id, Severity: "low", Issue: "no hook in the first line",
			Fix: "Open with the buyer's specific problem, a surprising true number from the facts, or the result."}
	}
	switch st.Contract {
	case "ad_copy_batch":
		var v AdCopyBatch
		if json.Unmarshal(st.Output, &v) != nil {
			return nil
		}
		for _, a := range v.Variants {
			if strings.TrimSpace(a.Hook) == "" && strings.TrimSpace(firstLine(a.PrimaryText)) == "" {
				out = append(out, noHook(a.ID))
			}
			if f, ok := ctaFinding(a.ID, a.CTA, q); ok {
				out = append(out, f)
			}
		}
	case "poster_brief":
		var v PosterBrief
		if json.Unmarshal(st.Output, &v) != nil {
			return nil
		}
		if n := len(strings.Fields(v.Headline)); n > maxPosterHeadlineWords {
			out = append(out, Finding{Quote: v.Headline, Severity: "low", Issue: fmt.Sprintf("the headline has %d words, too many to read at a glance", n),
				Fix: fmt.Sprintf("Lead with the outcome the buyer gets in %d words or fewer.", maxPosterHeadlineWords)})
		}
		if f, ok := ctaFinding("poster", v.CTA, q); ok {
			out = append(out, f)
		}
	case "social_posts":
		var v SocialPosts
		if json.Unmarshal(st.Output, &v) != nil {
			return nil
		}
		for i, p := range v.Posts {
			if strings.TrimSpace(p.Hook) == "" && strings.TrimSpace(firstLine(p.Text)) == "" {
				out = append(out, noHook(fmt.Sprintf("p%d", i+1)))
			}
		}
	case "content_calendar":
		var v ContentCalendar
		if json.Unmarshal(st.Output, &v) != nil {
			return nil
		}
		var missing []string
		for i, it := range v.Items {
			if strings.TrimSpace(it.CTA) == "" {
				missing = append(missing, fmt.Sprintf("c%d", i+1))
			}
		}
		if len(missing) > 0 {
			out = append(out, Finding{Quote: strings.Join(missing, ", "), Severity: "low", Issue: "these slots have no call to action",
				Fix: "Give every slot one next step on one channel."})
		}
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
