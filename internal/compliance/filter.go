package compliance

import (
	"strings"
)

// Filter checks AI responses for FLP compliance violations and injects required disclaimers.
type Filter struct {
	blockedPhrases     []string
	nutritionalDiscl   string
	skinCareDiscl      string
	incomeDiscl        string
	weightDiscl        string
	nutritionalTriggers []string
	skinCareTriggers   []string
	incomeTriggers     []string
	weightTriggers     []string
}

// NewFilter creates a compliance filter with FLP rules.
func NewFilter() *Filter {
	return &Filter{
		blockedPhrases: []string{
			"cures", "treats disease", "heals disease", "prevents disease", "prevents cancer",
			"fda approved", "miracle", "magic", "wonder drug",
			"guaranteed income", "get rich", "financial freedom guaranteed",
			"no work required", "passive income guaranteed", "easy money",
			"make money fast",
		},
		nutritionalDiscl: "\n\n*These statements have not been evaluated by the Food and Drug Administration. These products are not intended to diagnose, treat, cure, or prevent any disease.*",
		skinCareDiscl:    "\n\n*These products are not intended to cure or prevent any diseases and are not a substitute for medical advice or therapy.*",
		incomeDiscl:      "\n\n*Profits and bonuses shown are illustrations and do not reflect typical earnings. Results vary. See the FLP Income Disclosure Statement at foreverliving.com.*",
		weightDiscl:      "\n\n*Individual results may vary. Combined with proper diet and exercise.*",
		nutritionalTriggers: []string{"health", "nutrition", "supplement", "vitamin", "immune", "digestive", "energy", "wellness", "aloe"},
		skinCareTriggers:   []string{"skin", "wrinkle", "anti-aging", "moisturize", "complexion", "sonya", "infinite"},
		incomeTriggers:     []string{"earn", "income", "money", "salary", "commission", "bonus", "profit", "financial", "manager", "supervisor"},
		weightTriggers:     []string{"weight", "slim", "fat", "diet", "cleanse", "detox", "c9", "f15", "clean 9", "fit 15"},
	}
}

// Check validates an AI response and returns the filtered response with disclaimers.
// Returns (filteredResponse, blocked, reason). Applies FLP compliance rules.
// For non-FLP tenants (business_type='generic'), use CheckGeneric instead.
func (f *Filter) Check(response string) (string, bool, string) {
	lower := strings.ToLower(response)

	// Check for blocked phrases
	for _, phrase := range f.blockedPhrases {
		if strings.Contains(lower, phrase) {
			return "", true, "blocked phrase: " + phrase
		}
	}

	// Apply approved language substitutions
	response = f.substituteLanguage(response)

	// Inject required disclaimers based on content
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
	lower := strings.ToLower(response)
	hardBlocks := []string{
		"cures", "miracle", "magic", "wonder drug",
		"guaranteed income", "get rich", "easy money", "make money fast",
	}
	for _, phrase := range hardBlocks {
		if strings.Contains(lower, phrase) {
			return "", true, "blocked phrase: " + phrase
		}
	}
	return response, false, ""
}

// CheckForTenant routes to the appropriate filter based on business_type.
func (f *Filter) CheckForTenant(response, businessType string) (string, bool, string) {
	if businessType == "generic" || businessType == "" {
		return f.CheckGeneric(response)
	}
	return f.Check(response)
}

// substituteLanguage replaces prohibited medical claims with approved language.
func (f *Filter) substituteLanguage(response string) string {
	replacements := map[string]string{
		"cures":             "supports",
		"treats":            "promotes",
		"heals":             "helps maintain",
		"prevents disease":  "supports healthy",
		"guaranteed income": "potential earnings (results vary)",
	}
	for old, replacement := range replacements {
		response = caseInsensitiveReplace(response, old, replacement)
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

func caseInsensitiveReplace(s, old, replacement string) string {
	lower := strings.ToLower(s)
	oldLower := strings.ToLower(old)
	idx := strings.Index(lower, oldLower)
	if idx < 0 {
		return s
	}
	return s[:idx] + replacement + s[idx+len(old):]
}
