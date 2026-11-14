package compliance

import (
	"embed"
	"fmt"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
)

// The business types and their rules are data (data/business_types.yaml), reviewed in git and
// embedded in the binary like the library's playbooks. Sprint 7 #32 and #34.

//go:embed data/business_types.yaml
var dataFS embed.FS

// Type keys the code refers to by name. Every other key lives only in the YAML.
const (
	TypeGeneric = "generic"
	TypeFLP     = "flp"
)

// TypeInfo is one business type as owners see it: the key stored on the tenant, a label and a
// one line description.
type TypeInfo struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Block is one hard rule: text matching Pattern is blocked, and Reason says why.
type Block struct {
	Pattern string `yaml:"pattern"`
	Reason  string `yaml:"reason"`
	re      *regexp.Regexp
}

type typeRules struct {
	Key         string   `yaml:"key"`
	Label       string   `yaml:"label"`
	Description string   `yaml:"description"`
	Aliases     []string `yaml:"aliases"`
	// Strict names a built-in filter that replaces the data rules ("flp": Filter.Check).
	Strict   string   `yaml:"strict"`
	Guidance []string `yaml:"guidance"`
	Blocks   []Block  `yaml:"blocks"`
}

type catalogDoc struct {
	Baseline struct {
		Guidance []string `yaml:"guidance"`
		Blocks   []Block  `yaml:"blocks"`
	} `yaml:"baseline"`
	Types []typeRules `yaml:"types"`
}

type catalog struct {
	baselineGuidance []string
	baselineBlocks   []Block
	types            []typeRules
	byKey            map[string]int // key and alias -> index in types
}

// rules is loaded once at start. The data is embedded, so a broken file fails the package tests
// (TestCatalogLoads) long before it can reach a running service.
var rules = mustLoad()

func mustLoad() *catalog {
	raw, err := dataFS.ReadFile("data/business_types.yaml")
	if err != nil {
		panic(fmt.Sprintf("compliance: read business types: %v", err))
	}
	c, err := parseCatalog(raw)
	if err != nil {
		panic(err.Error())
	}
	return c
}

// parseCatalog reads and validates the business types file.
func parseCatalog(raw []byte) (*catalog, error) {
	var doc catalogDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("compliance: parse business types: %w", err)
	}
	c := &catalog{baselineGuidance: doc.Baseline.Guidance, types: doc.Types, byKey: map[string]int{}}
	if err := compileBlocks(doc.Baseline.Blocks, "baseline"); err != nil {
		return nil, err
	}
	c.baselineBlocks = doc.Baseline.Blocks
	for i := range c.types {
		t := &c.types[i]
		t.Key = strings.TrimSpace(t.Key)
		if t.Key == "" || t.Key != strings.ToLower(t.Key) || strings.TrimSpace(t.Label) == "" || strings.TrimSpace(t.Description) == "" {
			return nil, fmt.Errorf("compliance: business type %d needs a lower case key, a label and a description", i+1)
		}
		if t.Strict != "" && t.Strict != TypeFLP {
			return nil, fmt.Errorf("compliance: business type %s names an unknown strict filter %q", t.Key, t.Strict)
		}
		if err := compileBlocks(t.Blocks, t.Key); err != nil {
			return nil, err
		}
		for _, name := range append([]string{t.Key}, t.Aliases...) {
			n := normalizeName(name)
			if _, dup := c.byKey[n]; dup {
				return nil, fmt.Errorf("compliance: business type name %q is used twice", n)
			}
			c.byKey[n] = i
		}
	}
	for _, need := range []string{TypeGeneric, TypeFLP} {
		if _, ok := c.byKey[need]; !ok {
			return nil, fmt.Errorf("compliance: business type %q is missing", need)
		}
	}
	return c, nil
}

func compileBlocks(blocks []Block, owner string) error {
	for i := range blocks {
		b := &blocks[i]
		if strings.TrimSpace(b.Reason) == "" {
			return fmt.Errorf("compliance: %s block %d needs a reason", owner, i+1)
		}
		re, err := regexp.Compile(b.Pattern)
		if err != nil {
			return fmt.Errorf("compliance: %s block %d: %w", owner, i+1, err)
		}
		b.re = re
	}
	return nil
}

func normalizeName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// lookup returns the type for a key or alias; empty, unknown and legacy values ("codevertex")
// get the generic type.
func (c *catalog) lookup(name string) *typeRules {
	if i, ok := c.byKey[normalizeName(name)]; ok {
		return &c.types[i]
	}
	return &c.types[c.byKey[TypeGeneric]]
}

// Types lists every business type in file order, for pickers and API validation.
func Types() []TypeInfo {
	out := make([]TypeInfo, 0, len(rules.types))
	for _, t := range rules.types {
		out = append(out, TypeInfo{Key: t.Key, Label: t.Label, Description: t.Description})
	}
	return out
}

// Known reports whether the value names a business type, by key or alias.
func Known(businessType string) bool {
	_, ok := rules.byKey[normalizeName(businessType)]
	return ok
}

// Normalize maps a key or alias to its key; empty or unknown values become "generic".
func Normalize(businessType string) string {
	return rules.lookup(businessType).Key
}

// PromptRules is the rules block added to every chat, caption and script prompt: the shared
// honesty baseline plus the business type's own guidance. It starts with a blank line so it
// can be appended to any prompt.
func PromptRules(businessType string) string {
	t := rules.lookup(businessType)
	var b strings.Builder
	fmt.Fprintf(&b, "\nBUSINESS RULES (%s). Every answer, caption and script you write must follow them:\n", t.Label)
	for _, g := range t.Guidance {
		b.WriteString("- ")
		b.WriteString(g)
		b.WriteString("\n")
	}
	for _, g := range rules.baselineGuidance {
		b.WriteString("- ")
		b.WriteString(g)
		b.WriteString("\n")
	}
	return b.String()
}

// checkBlocks returns the reason for the first baseline or type block the text matches.
func checkBlocks(t *typeRules, text string) (string, bool) {
	for _, b := range rules.baselineBlocks {
		if b.re.MatchString(text) {
			return b.Reason, true
		}
	}
	for _, b := range t.Blocks {
		if b.re.MatchString(text) {
			return t.Label + " rule: " + b.Reason, true
		}
	}
	return "", false
}
