package compliance

import (
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
// Applies minimal safety rules (no medical claims, no guaranteed income) but
// skips FLP-specific disclaimers and product-name substitutions.
func (f *Filter) CheckGeneric(response string) (string, bool, string) {
	hardBlocks := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bcure[sd]?\b|\bcuring\b`),
		regexp.MustCompile(`(?i)\bmiracle\b|\bmagic\b|\bwonder\s+drug\b`),
		regexp.MustCompile(`(?i)\bguaranteed?\s+income\b`),
		regexp.MustCompile(`(?i)\bget\s+rich\b`),
		regexp.MustCompile(`(?i)\beasy\s+money\b`),
		regexp.MustCompile(`(?i)\bmake\s+money\s+fast\b`),
	}
	for _, re := range hardBlocks {
		if re.MatchString(response) {
			return "", true, "blocked pattern: " + re.String()
		}
	}
	return response, false, ""
}

// CheckForTenant routes to the appropriate filter based on business_type.
// FLP uses full compliance rules (disclaimers + blocked phrases).
// Codevertex and generic use minimal safety rules only.
func (f *Filter) CheckForTenant(response, businessType string) (string, bool, string) {
	if businessType == "flp" {
		return f.Check(response)
	}
	return f.CheckGeneric(response)
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
