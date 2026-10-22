// Package library is the Marketing OS know-how, embedded in the binary:
//
//   - playbooks distilled from the marketingskills plugin and the
//     marketing-psychology kit (both MIT), one per marketing job;
//   - the default team roster (Marketing Manager plus specialists);
//   - house rules every specialist follows;
//   - a curated marketing subset of the prompts.chat library (CC0), produced by
//     cmd/curate-prompts.
//
// Attribution is in NOTICE.md. The data is reviewed in git, never downloaded at
// runtime.
package library

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/goccy/go-yaml"
)

//go:embed data/house_rules.yaml data/team.yaml data/playbooks/*.yaml data/prompts.json
var dataFS embed.FS

// Source credits where a playbook was distilled from.
type Source struct {
	Repo    string `yaml:"repo" json:"repo"`
	Skill   string `yaml:"skill" json:"skill"`
	License string `yaml:"license" json:"license"`
}

// Starter is a fill-in request for owners who do not know how to ask.
type Starter struct {
	Title    string `yaml:"title" json:"title"`
	Template string `yaml:"template" json:"template"`
}

// Playbook is one marketing job a specialist knows how to do.
type Playbook struct {
	Key            string    `yaml:"key" json:"key"`
	Name           string    `yaml:"name" json:"name"`
	Version        int       `yaml:"version" json:"version"`
	Source         Source    `yaml:"source" json:"source"`
	Specialist     string    `yaml:"specialist" json:"specialist"`
	ModelClass     string    `yaml:"model_class" json:"model_class"`
	OutputContract string    `yaml:"output_contract" json:"output_contract"`
	WhenToUse      string    `yaml:"when_to_use" json:"when_to_use"`
	Keywords       []string  `yaml:"keywords" json:"keywords"`
	RequiredInputs []string  `yaml:"required_inputs" json:"required_inputs"`
	OptionalInputs []string  `yaml:"optional_inputs" json:"optional_inputs"`
	Method         []string  `yaml:"method" json:"method"`
	Rules          []string  `yaml:"rules" json:"rules"`
	Starters       []Starter `yaml:"starters" json:"starters"`
}

// Member is a default team member.
type Member struct {
	Key         string   `yaml:"key" json:"key"`
	Name        string   `yaml:"name" json:"name"`
	Role        string   `yaml:"role" json:"role"`
	ReportsTo   string   `yaml:"reports_to" json:"reports_to"`
	Description string   `yaml:"description" json:"description"`
	ModelClass  string   `yaml:"model_class" json:"model_class"`
	Playbooks   []string `yaml:"playbooks" json:"playbooks"`
	Tools       []string `yaml:"tools" json:"tools"`
	Guardrails  []string `yaml:"guardrails" json:"guardrails"`
}

// Prompt is one curated prompts.chat entry.
type Prompt struct {
	ID      string   `json:"id"`
	Act     string   `json:"act"`
	Prompt  string   `json:"prompt"`
	Type    string   `json:"type"`
	Tags    []string `json:"tags"`
	Source  string   `json:"source"`
	License string   `json:"license"`
}

// Inputs are the TaskSpec entity names playbooks may require.
var Inputs = []string{"offering", "audience", "objective", "platforms", "budget", "period", "language", "goal", "topic", "url", "competitors", "winners"}

// OutputContracts are the result shapes the team runtime knows how to validate.
var OutputContracts = []string{"answer", "ad_copy_batch", "content_calendar", "social_posts", "video_script", "poster_brief", "brand_context", "review_findings"}

// ModelClasses mirror internal/llm classes (kept as strings to avoid an import cycle).
var ModelClasses = []string{"nano", "standard", "premium", "premium_plus"}

// Library holds the loaded, validated data. It is read-only after Load.
type Library struct {
	HouseRules []string
	playbooks  map[string]Playbook
	order      []string
	members    map[string]Member
	memberKeys []string
	prompts    []Prompt

	idxOnce sync.Once
	pbIdx   map[string][]string // playbook key -> tokens
	prIdx   [][]string          // prompt index -> tokens
}

// Load reads and validates the embedded data.
func Load() (*Library, error) {
	lib := &Library{playbooks: map[string]Playbook{}, members: map[string]Member{}}

	var rules struct {
		Rules []string `yaml:"rules"`
	}
	if err := readYAML("data/house_rules.yaml", &rules); err != nil {
		return nil, err
	}
	lib.HouseRules = rules.Rules

	files, err := fs.Glob(dataFS, "data/playbooks/*.yaml")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for _, f := range files {
		var doc struct {
			Playbooks []Playbook `yaml:"playbooks"`
		}
		if err := readYAML(f, &doc); err != nil {
			return nil, err
		}
		for _, p := range doc.Playbooks {
			if _, dup := lib.playbooks[p.Key]; dup {
				return nil, fmt.Errorf("library: playbook %q defined twice", p.Key)
			}
			lib.playbooks[p.Key] = p
			lib.order = append(lib.order, p.Key)
		}
	}

	var team struct {
		Members []Member `yaml:"members"`
	}
	if err := readYAML("data/team.yaml", &team); err != nil {
		return nil, err
	}
	for _, m := range team.Members {
		lib.members[m.Key] = m
		lib.memberKeys = append(lib.memberKeys, m.Key)
	}

	raw, err := dataFS.ReadFile("data/prompts.json")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &lib.prompts); err != nil {
		return nil, fmt.Errorf("library: prompts.json: %w", err)
	}
	if err := lib.validate(); err != nil {
		return nil, err
	}
	return lib, nil
}

// MustLoad is Load for wiring code; the data is embedded and tested, so a
// failure is a build defect.
func MustLoad() *Library {
	lib, err := Load()
	if err != nil {
		panic(err)
	}
	return lib
}

func readYAML(name string, v any) error {
	data, err := dataFS.ReadFile(name)
	if err != nil {
		return fmt.Errorf("library: read %s: %w", name, err)
	}
	if err := yaml.Unmarshal(data, v); err != nil {
		return fmt.Errorf("library: parse %s: %w", path.Base(name), err)
	}
	return nil
}

func (l *Library) validate() error {
	if len(l.HouseRules) == 0 {
		return fmt.Errorf("library: no house rules")
	}
	if _, ok := l.members["manager"]; !ok {
		return fmt.Errorf("library: the team has no manager")
	}
	for _, key := range l.order {
		p := l.playbooks[key]
		if p.Key == "" || p.Name == "" || p.WhenToUse == "" || len(p.Method) == 0 {
			return fmt.Errorf("library: playbook %q is incomplete", key)
		}
		if _, ok := l.members[p.Specialist]; !ok {
			return fmt.Errorf("library: playbook %q names unknown specialist %q", key, p.Specialist)
		}
		if !contains(ModelClasses, p.ModelClass) {
			return fmt.Errorf("library: playbook %q has unknown model class %q", key, p.ModelClass)
		}
		if !contains(OutputContracts, p.OutputContract) {
			return fmt.Errorf("library: playbook %q has unknown output contract %q", key, p.OutputContract)
		}
		for _, in := range append(append([]string{}, p.RequiredInputs...), p.OptionalInputs...) {
			if !contains(Inputs, in) {
				return fmt.Errorf("library: playbook %q uses unknown input %q", key, in)
			}
		}
		if p.Source.License == "" {
			return fmt.Errorf("library: playbook %q has no source licence", key)
		}
	}
	for _, key := range l.memberKeys {
		m := l.members[key]
		if !contains(ModelClasses, m.ModelClass) {
			return fmt.Errorf("library: member %q has unknown model class %q", key, m.ModelClass)
		}
		if m.ReportsTo != "" {
			if _, ok := l.members[m.ReportsTo]; !ok {
				return fmt.Errorf("library: member %q reports to unknown %q", key, m.ReportsTo)
			}
		}
		for _, pb := range m.Playbooks {
			if _, ok := l.playbooks[pb]; !ok {
				return fmt.Errorf("library: member %q lists unknown playbook %q", key, pb)
			}
		}
	}
	return nil
}

// Playbook returns a playbook by key.
func (l *Library) Playbook(key string) (Playbook, bool) {
	p, ok := l.playbooks[key]
	return p, ok
}

// Playbooks returns every playbook in file order.
func (l *Library) Playbooks() []Playbook {
	out := make([]Playbook, 0, len(l.order))
	for _, k := range l.order {
		out = append(out, l.playbooks[k])
	}
	return out
}

// Member returns a default team member by key.
func (l *Library) Member(key string) (Member, bool) {
	m, ok := l.members[key]
	return m, ok
}

// Members returns the default team, manager first.
func (l *Library) Members() []Member {
	out := make([]Member, 0, len(l.memberKeys))
	for _, k := range l.memberKeys {
		out = append(out, l.members[k])
	}
	return out
}

// Prompts returns the curated prompts.
func (l *Library) Prompts() []Prompt { return l.prompts }

// Template is a quick-start item for the gallery.
type Template struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Template string `json:"template"`
	Playbook string `json:"playbook,omitempty"`
	Category string `json:"category"`
	Source   string `json:"source"`
}

// Templates lists starters (playbook-backed, for owners) then curated prompts
// (for owners who want a ready prompt to adapt), filtered by query and category.
func (l *Library) Templates(query, category string, limit int) []Template {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	var out []Template
	for _, p := range l.Playbooks() {
		cat := l.members[p.Specialist].Role
		for i, s := range p.Starters {
			if category != "" && !strings.EqualFold(category, p.Specialist) {
				continue
			}
			out = append(out, Template{ID: fmt.Sprintf("%s:%d", p.Key, i), Title: s.Title, Template: s.Template, Playbook: p.Key, Category: cat, Source: "marketflow"})
		}
	}
	if q := strings.TrimSpace(query); q != "" {
		tokens := Tokenize(q)
		filtered := out[:0]
		for _, t := range out {
			if overlap(tokens, Tokenize(t.Title+" "+t.Template+" "+t.Playbook)) > 0 {
				filtered = append(filtered, t)
			}
		}
		out = filtered
		for _, hit := range l.SearchPrompts(q, limit) {
			p := l.prompts[hit]
			out = append(out, Template{ID: "prompt:" + p.ID, Title: p.Act, Template: p.Prompt, Category: strings.Join(p.Tags, ", "), Source: p.Source})
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

var varRe = regexp.MustCompile(`\$\{([a-z_]+)\}`)

// Render fills ${var} slots. Unknown or empty variables become "[var]" so a
// half-filled template is visible to the owner rather than silently wrong.
func Render(template string, vars map[string]string) string {
	return varRe.ReplaceAllStringFunc(template, func(m string) string {
		name := varRe.FindStringSubmatch(m)[1]
		if v := strings.TrimSpace(vars[name]); v != "" {
			return v
		}
		return "[" + name + "]"
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
