package team

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/bengobox/marketflow-ai/internal/decide"
	"github.com/bengobox/marketflow-ai/internal/promptopt"
)

// The Reviewer runs on every outward-facing step:
//  1. deterministic rules - the tenant's compliance filter, AI tells, and for
//     presenter scripts any first-person claim of using the product;
//  2. one decide call scoring every variant (fit) and checking unsupported claims
//     and, for UGC, personal-use claims;
//  3. ranking: blocked items drop, the rest sort by fit, the top K are the shortlist.

var (
	aiTells = []*regexp.Regexp{
		regexp.MustCompile("—"), // em dash
		regexp.MustCompile(`(?i)\bit'?s not (just )?[^.,;]{1,40}, it'?s\b`),
		regexp.MustCompile(`(?i)\bsay goodbye to\b|\bunlock the power\b|\btake it to the next level\b|\bgame[- ]changing\b|\brevolutionary\b|\bhere'?s the thing\b`),
		regexp.MustCompile(`(?i)\bstop scrolling\b|\bget started today\b`),
	}
	// First-person use or result claims (English and Swahili) a synthetic
	// presenter must never make.
	personalUse = regexp.MustCompile(`(?i)\bi (have )?(tried|used|tested|bought|been using)\b|\bmy (skin|hair|results?|body|business|sales) (changed|improved|is|are|grew)\b|\bi'?ve been using\b|\bnimejaribu\b|\bnilitumia\b|\bnimetumia\b|\bngozi yangu\b`)
)

// outward contracts get the full review; others only the compliance filter.
var outward = map[string]bool{"ad_copy_batch": true, "video_script": true, "poster_brief": true, "social_posts": true, "content_calendar": true}

type reviewItem struct {
	id     string
	text   string
	speech string // presenter voiceover, checked for personal-use claims
}

func (r *Runtime) review(ctx context.Context, st *Step, spec *promptopt.TaskSpec, tc TenantContext, k int) ([]string, []Finding) {
	if st.Error != "" {
		return nil, nil
	}
	var findings []Finding
	if !outward[st.Contract] {
		if st.Text != "" {
			if _, blocked, reason := r.filter.CheckForTenant(st.Text, tc.BusinessType); blocked {
				findings = append(findings, Finding{Quote: "", Issue: "compliance: " + reason, Severity: "high", Fix: "Rewrite without the blocked claim."})
				st.Text = ""
				st.Error = "blocked by the compliance rules"
			}
		}
		return nil, findings
	}

	items := reviewItems(st)
	ugc := st.Playbook == "ugc-video" || st.Playbook == "video-script"
	blocked := map[string]bool{}
	for _, it := range items {
		if _, b, reason := r.filter.CheckForTenant(it.text, tc.BusinessType); b {
			blocked[it.id] = true
			findings = append(findings, Finding{Quote: it.id, Issue: "compliance: " + reason, Severity: "high", Fix: "Remove the claim or replace it with a supported one."})
		}
		if ugc {
			if m := personalUse.FindString(it.speech); m != "" {
				blocked[it.id] = true
				findings = append(findings, Finding{Quote: m, Issue: "a synthetic presenter claims personal use", Severity: "high", Fix: "Have the presenter explain or recommend, or quote a real published review with attribution."})
			}
		}
		for _, re := range aiTells {
			if m := re.FindString(it.text); m != "" {
				findings = append(findings, Finding{Quote: it.id + ": " + m, Issue: "AI tell", Severity: "low", Fix: "Rewrite the phrase plainly."})
				break
			}
		}
	}

	// One decide call for fit, unsupported claims and (UGC) personal claims.
	fit := map[string]float64{}
	if r.decider != nil && len(items) > 0 {
		qs := map[string]decide.Question{}
		var state strings.Builder
		state.WriteString("FACTS (brand context and request):\n")
		state.WriteString(trimTo(tc.MarketingContext, 3000))
		state.WriteString("\n")
		state.WriteString(spec.OptimisedBrief)
		state.WriteString("\nDRAFTS:\n")
		for _, it := range items {
			if blocked[it.id] {
				continue
			}
			fmt.Fprintf(&state, "[%s] %s\n", it.id, trimTo(it.text, 900))
			qs["fit_"+it.id] = decide.Score(fmt.Sprintf("How well does draft %s fit the facts and how specific is it?", it.id), "weak", "fair", "strong")
			qs["unsupported_"+it.id] = decide.Noul(fmt.Sprintf("Does draft %s state a price, number, result or claim that the facts do not support?", it.id))
			if ugc {
				qs["personal_"+it.id] = decide.Noul(fmt.Sprintf("In draft %s, does the speaker claim to have used, tried or tested the product, or describe their own result?", it.id))
			}
		}
		if len(qs) > 0 {
			answers, _, err := r.decider.Decide(ctx, "qc", state.String(), qs)
			if err == nil {
				for _, it := range items {
					if blocked[it.id] {
						continue
					}
					if a, ok := answers["fit_"+it.id]; ok {
						fit[it.id] = a.Score * a.Confidence
					}
					if a, ok := answers["unsupported_"+it.id]; ok && a.Yes(0.75) {
						findings = append(findings, Finding{Quote: it.id, Issue: "possibly unsupported claim", Severity: "review", Fix: "Check the claim against the catalog or brand context, or remove it."})
						fit[it.id] -= 1
					}
					if a, ok := answers["personal_"+it.id]; ok && a.Yes(0.75) {
						blocked[it.id] = true
						findings = append(findings, Finding{Quote: it.id, Issue: "presenter appears to claim personal use", Severity: "high", Fix: "Rewrite as explanation or a quoted real review."})
					}
				}
			}
		}
	}

	var ranked []string
	for _, it := range items {
		if !blocked[it.id] {
			ranked = append(ranked, it.id)
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return fit[ranked[i]] > fit[ranked[j]] })
	if k > 0 && len(ranked) > k {
		ranked = ranked[:k]
	}
	if len(blocked) > 0 {
		st.Output = dropBlocked(st, blocked)
	}
	return ranked, findings
}

func reviewItems(st *Step) []reviewItem {
	var items []reviewItem
	switch st.Contract {
	case "ad_copy_batch":
		var v AdCopyBatch
		if json.Unmarshal(st.Output, &v) == nil {
			for _, a := range v.Variants {
				text := strings.Join([]string{a.Hook, a.Headline, a.PrimaryText, a.CTA}, "\n")
				items = append(items, reviewItem{id: a.ID, text: text, speech: a.Hook + "\n" + a.PrimaryText})
			}
		}
	case "video_script":
		var v VideoScript
		if json.Unmarshal(st.Output, &v) == nil {
			var all, speech strings.Builder
			for _, h := range v.Hooks {
				all.WriteString(h + "\n")
				speech.WriteString(h + "\n")
			}
			for _, s := range v.Scenes {
				all.WriteString(s.Voiceover + "\n" + s.Caption + "\n")
				if s.Shot == "presenter" {
					speech.WriteString(s.Voiceover + "\n")
				}
			}
			items = append(items, reviewItem{id: "script", text: all.String(), speech: speech.String()})
		}
	case "poster_brief":
		var v PosterBrief
		if json.Unmarshal(st.Output, &v) == nil {
			items = append(items, reviewItem{id: "poster", text: v.Headline + "\n" + v.Subline + "\n" + v.CTA})
		}
	case "social_posts":
		var v SocialPosts
		if json.Unmarshal(st.Output, &v) == nil {
			for i, p := range v.Posts {
				items = append(items, reviewItem{id: fmt.Sprintf("p%d", i+1), text: p.Hook + "\n" + p.Text})
			}
		}
	case "content_calendar":
		var v ContentCalendar
		if json.Unmarshal(st.Output, &v) == nil {
			for i, it := range v.Items {
				items = append(items, reviewItem{id: fmt.Sprintf("c%d", i+1), text: it.Hook + "\n" + it.CTA})
			}
		}
	}
	return items
}

// dropBlocked removes blocked variants from an ad copy batch so a blocked draft
// is never shown as usable; other contracts keep their content with findings.
func dropBlocked(st *Step, blocked map[string]bool) json.RawMessage {
	if st.Contract != "ad_copy_batch" {
		if st.Contract == "video_script" && blocked["script"] {
			st.Error = "the script was blocked by the review; see findings"
			return nil
		}
		return st.Output
	}
	var v AdCopyBatch
	if json.Unmarshal(st.Output, &v) != nil {
		return st.Output
	}
	kept := v.Variants[:0]
	for _, a := range v.Variants {
		if !blocked[a.ID] {
			kept = append(kept, a)
		}
	}
	v.Variants = kept
	out, _ := json.Marshal(v)
	return out
}
