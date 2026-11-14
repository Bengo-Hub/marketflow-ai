package library

import (
	"fmt"
	"regexp"
	"strings"
)

// The template gallery's facets (Marketing team > Templates): each starter carries who does it
// (the playbook's specialist), its goal and output, its language, the channels it names, the
// inputs it asks for, the date it was added and, for media jobs, the registry tier its price
// comes from. Labels for every facet come from data/template_facets.yaml, so the UI shows them
// as given and never hard-codes them.

// FacetOption is one value of a facet and its label.
type FacetOption struct {
	Key   string   `yaml:"key" json:"key"`
	Label string   `yaml:"label" json:"label"`
	Words []string `yaml:"words" json:"-"`
}

// PlaybookFacets are one playbook's facets.
type PlaybookFacets struct {
	Goal   string `yaml:"goal" json:"goal"`
	Output string `yaml:"output" json:"output"`
	Added  string `yaml:"added" json:"added"` // YYYY-MM-DD the playbook's starters were added
	Tier   string `yaml:"tier" json:"tier,omitempty"`
}

// TemplateFacets are the gallery's filter values and labels.
type TemplateFacets struct {
	Goals       []FacetOption `yaml:"goals" json:"goals"`
	Outputs     []FacetOption `yaml:"outputs" json:"outputs"`
	Languages   []FacetOption `yaml:"languages" json:"languages"`
	Channels    []FacetOption `yaml:"channels" json:"channels"`
	Inputs      []FacetOption `yaml:"inputs" json:"inputs"`
	Specialists []FacetOption `yaml:"-" json:"specialists"`

	Playbooks map[string]PlaybookFacets `yaml:"playbooks" json:"-"`
}

var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// loadFacets reads data/template_facets.yaml and adds the specialists from the team.
func (l *Library) loadFacets() error {
	var f TemplateFacets
	if err := readYAML("data/template_facets.yaml", &f); err != nil {
		return err
	}
	for _, key := range l.memberKeys {
		m := l.members[key]
		f.Specialists = append(f.Specialists, FacetOption{Key: m.Key, Label: m.Role})
	}
	l.facets = f
	return nil
}

func hasOption(list []FacetOption, key string) bool {
	for _, o := range list {
		if o.Key == key {
			return true
		}
	}
	return false
}

// validateFacets checks that every playbook has known facets and every input is labelled.
func (l *Library) validateFacets() error {
	f := l.facets
	for _, key := range l.order {
		pf, ok := f.Playbooks[key]
		if !ok {
			return fmt.Errorf("library: playbook %q has no template facets", key)
		}
		if !hasOption(f.Goals, pf.Goal) || !hasOption(f.Outputs, pf.Output) {
			return fmt.Errorf("library: playbook %q has an unknown goal %q or output %q", key, pf.Goal, pf.Output)
		}
		if !datePattern.MatchString(pf.Added) {
			return fmt.Errorf("library: playbook %q added date %q is not YYYY-MM-DD", key, pf.Added)
		}
		for _, s := range l.playbooks[key].Starters {
			if lang := starterLanguage(s); !hasOption(f.Languages, lang) {
				return fmt.Errorf("library: playbook %q starter %q has unknown language %q", key, s.Title, lang)
			}
		}
	}
	for key := range f.Playbooks {
		if _, ok := l.playbooks[key]; !ok {
			return fmt.Errorf("library: template facets name unknown playbook %q", key)
		}
	}
	for _, in := range Inputs {
		if !hasOption(f.Inputs, in) {
			return fmt.Errorf("library: input %q has no label in the template facets", in)
		}
	}
	return nil
}

// Facets returns the gallery's filter values and labels.
func (l *Library) Facets() TemplateFacets { return l.facets }

// starterLanguage is a starter's language, English unless it says otherwise.
func starterLanguage(s Starter) string {
	if s.Language == "" {
		return "en"
	}
	return s.Language
}

// decorate sets a starter template's facets from its playbook and its own text.
func (l *Library) decorate(t *Template, p Playbook, s Starter) {
	pf := l.facets.Playbooks[p.Key]
	t.Specialist = p.Specialist
	t.Goal, t.Output, t.Added, t.Tier = pf.Goal, pf.Output, pf.Added, pf.Tier
	t.Language = starterLanguage(s)
	t.Channels = l.channelsIn(s.Title + " " + s.Template)
	t.Inputs = templateInputs(p, s.Template)
}

// channelsIn names the channels whose words appear in text (whole words, any case).
func (l *Library) channelsIn(text string) []string {
	words := " " + strings.Join(strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}), " ") + " "
	var out []string
	for _, c := range l.facets.Channels {
		for _, w := range c.Words {
			if strings.Contains(words, " "+strings.ToLower(w)+" ") {
				out = append(out, c.Key)
				break
			}
		}
	}
	return out
}

// templateInputs are the inputs a starter asks for: its ${...} slots and its playbook's
// required inputs, in the order of Inputs.
func templateInputs(p Playbook, template string) []string {
	want := map[string]bool{}
	for _, in := range p.RequiredInputs {
		want[in] = true
	}
	for _, m := range varRe.FindAllStringSubmatch(template, -1) {
		want[m[1]] = true
	}
	var out []string
	for _, in := range Inputs {
		if want[in] {
			out = append(out, in)
		}
	}
	return out
}
