package compliance

import (
	"fmt"
	"regexp"
	"strings"
)

// Filter checks AI responses for compliance violations and injects required disclaimers.
type Filter struct {
	blockedPatterns     []*regexp.Regexp
	substituteRules     []substituteRule
	nutritionalDiscl    string
	skinCareDiscl       string
	incomeDiscl         string
	weightDiscl         string
	nutritionalTriggers []string
	skinCareTriggers    []string
	incomeTriggers      []string
	weightTriggers      []string
}

// The rules every tenant that is not FLP gets (the baseline blocks) and each business type's own
// blocks live in data/business_types.yaml (business_types.go). The baseline covers medical cure
// claims and financial promises that are misleading for any business (Consumer Protection Act
// 2012). "treat" and "heal" are deliberately not in it: everyday copy such as "treat yourself"
// would be blocked for cafes and shops.

type substituteRule struct {
	pattern     *regexp.Regexp
	replacement string
}

// NewFilter creates a compliance filter with FLP rules.
func NewFilter() *Filter {
	return &Filter{
		// Regex patterns — cover all tense/form variants so "curing" bypasses are blocked.
		blockedPatterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)\bcure[sd]?\b|\bcuring\b`),
			regexp.MustCompile(`(?i)\btreat[sed]?\b|\btreating\b|\btreatment\b`),
			regexp.MustCompile(`(?i)\bheal[sed]?\b|\bhealing\b`),
			regexp.MustCompile(`(?i)\bprevent[sed]?\b|\bpreventing\b`),
			regexp.MustCompile(`(?i)\bfda[\s-]approved\b`),
			regexp.MustCompile(`(?i)\bmiracle\b|\bmagic\b|\bwonder\s+drug\b`),
			regexp.MustCompile(`(?i)\bguaranteed?\s+income\b`),
			regexp.MustCompile(`(?i)\bget\s+rich\b`),
			regexp.MustCompile(`(?i)\bfinancial\s+freedom\s+guaranteed\b`),
			regexp.MustCompile(`(?i)\bno\s+work\s+required\b`),
			regexp.MustCompile(`(?i)\bpassive\s+income\s+guaranteed\b`),
			regexp.MustCompile(`(?i)\beasy\s+money\b`),
			regexp.MustCompile(`(?i)\bmake\s+money\s+fast\b`),
		},
		substituteRules: []substituteRule{
			{regexp.MustCompile(`(?i)\bcure[sd]?\b`), "supports"},
			{regexp.MustCompile(`(?i)\bcuring\b`), "supporting"},
			{regexp.MustCompile(`(?i)\btreats?\b`), "promotes"},
			{regexp.MustCompile(`(?i)\bheals?\b`), "helps maintain"},
			{regexp.MustCompile(`(?i)\bhealing\b`), "helping maintain"},
			{regexp.MustCompile(`(?i)\bprevents?\s+disease\b`), "supports healthy"},
			{regexp.MustCompile(`(?i)\bguaranteed?\s+income\b`), "potential earnings (results vary)"},
		},
		nutritionalDiscl: "\n\n*These statements have not been evaluated by the Food and Drug Administration. These products are not intended to diagnose, treat, cure, or prevent any disease.*",
		skinCareDiscl:    "\n\n*These products are not intended to cure or prevent any diseases and are not a substitute for medical advice or therapy.*",
		incomeDiscl:      "\n\n*Profits and bonuses shown are illustrations and do not reflect typical earnings. Results vary. See the FLP Income Disclosure Statement at foreverliving.com.*",
		weightDiscl:      "\n\n*Individual results may vary. Combined with proper diet and exercise.*",
		nutritionalTriggers: []string{"health", "nutrition", "supplement", "vitamin", "immune", "digestive", "energy", "wellness", "aloe"},
		skinCareTriggers:    []string{"skin", "wrinkle", "anti-aging", "moisturize", "complexion", "sonya", "infinite"},
		incomeTriggers:      []string{"earn", "income", "money", "salary", "commission", "bonus", "profit", "financial", "manager", "supervisor"},
		weightTriggers:      []string{"weight", "slim", "fat", "diet", "cleanse", "detox", "c9", "f15", "clean 9", "fit 15"},
	}
}

// Check validates an AI response and returns the filtered response with disclaimers.
// Returns (filteredResponse, blocked, reason). Applies FLP compliance rules.
// For non-FLP tenants use CheckGeneric instead.
func (f *Filter) Check(response string) (string, bool, string) {
	for _, re := range f.blockedPatterns {
		if re.MatchString(response) {
			return "", true, "blocked pattern: " + re.String()
		}
	}

	response = f.substituteLanguage(response)

	lower := strings.ToLower(response)
	disclaimers := ""
	if f.containsAny(lower, f.nutritionalTriggers) {
		disclaimers += f.nutritionalDiscl
	}
	if f.containsAny(lower, f.skinCareTriggers) {
		disclaimers += f.skinCareDiscl
	}
	if f.containsAny(lower, f.incomeTriggers) {
		disclaimers += f.incomeDiscl
	}
	if f.containsAny(lower, f.weightTriggers) {
		disclaimers += f.weightDiscl
	}

	if disclaimers != "" {
		response += disclaimers
	}

	return response, false, ""
}

// CheckGeneric validates an AI response for generic (non-FLP) tenants.
// Applies the shared baseline (no medical claims, no guaranteed income) but
// skips FLP-specific disclaimers and product-name substitutions.
func (f *Filter) CheckGeneric(response string) (string, bool, string) {
	return f.CheckForTenant(response, TypeGeneric)
}

// CheckForTenant applies the tenant's business type rules (Sprint 7 #34). FLP keeps its full
// compliance rules (Check: disclaimers, substitutions and blocked phrases). Every other type gets
// the shared baseline blocks plus its own blocks from data/business_types.yaml; an empty,
// unknown or legacy type ("codevertex") gets the generic rules. The text is never rewritten for
// these types: it passes as it is or is blocked with a reason the owner can read.
func (f *Filter) CheckForTenant(response, businessType string) (string, bool, string) {
	t := rules.lookup(businessType)
	if t.Strict == TypeFLP {
		return f.Check(response)
	}
	if reason, blocked := checkBlocks(t, response); blocked {
		return "", true, reason
	}
	return response, false, ""
}

// Violations lists every rule the text breaks for the business type, each with the words that
// broke it, in language a model can act on; empty when the text passes. The safe rewrite (one
// more model try) adds them to the prompt (owner, 2026-11-14).
func (f *Filter) Violations(response, businessType string) []string {
	t := rules.lookup(businessType)
	var out []string
	add := func(reason string, re *regexp.Regexp) {
		if m := re.FindString(response); m != "" {
			out = append(out, fmt.Sprintf("%s (you wrote %q)", reason, m))
		}
	}
	if t.Strict == TypeFLP {
		for _, re := range f.blockedPatterns {
			add("Forever Living rule: no medical cure, treatment or prevention claims, no income promises", re)
		}
		return out
	}
	for _, b := range rules.baselineBlocks {
		add(b.Reason, b.re)
	}
	for _, b := range t.Blocks {
		add(t.Label+" rule: "+b.Reason, b.re)
	}
	return out
}

// substituteLanguage replaces prohibited medical claims with approved language.
func (f *Filter) substituteLanguage(response string) string {
	for _, rule := range f.substituteRules {
		response = rule.pattern.ReplaceAllString(response, rule.replacement)
	}
	return response
}

func (f *Filter) containsAny(text string, triggers []string) bool {
	for _, t := range triggers {
		if strings.Contains(text, t) {
			return true
		}
	}
	return false
}
