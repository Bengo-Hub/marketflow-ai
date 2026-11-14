package library

import (
	"fmt"
	"strconv"
	"strings"
)

// Ad templates are the script approaches a video ad can follow (educate while advertising,
// a day in the life, problem then agitate then solve, myth versus fact and the rest). They
// live in data/ad_templates.yaml with the delivery tags a line may open with and the script
// quality limits, so a new approach or a changed limit is a data change reviewed in git.

// Video kinds an ad template can suit. UGC is the creator style; it is the only kind that
// may read a script with two voices.
const (
	AdKindTemplate  = "template"
	AdKindPresenter = "presenter"
	AdKindUGC       = "ugc"
)

// Facts an ad template may require before it is used.
const (
	NeedRealReviews  = "real_reviews"  // published testimonials with the owner's ad-use consent
	NeedFounderStory = "founder_story" // the founder's own story in the brand context or facts
)

var (
	adKinds = []string{AdKindTemplate, AdKindPresenter, AdKindUGC}
	adNeeds = []string{NeedRealReviews, NeedFounderStory}
)

// AdTemplate is one script approach.
type AdTemplate struct {
	Key       string   `yaml:"key" json:"key"`
	Name      string   `yaml:"name" json:"name"`
	WhenToUse string   `yaml:"when_to_use" json:"when_to_use"`
	Keywords  []string `yaml:"keywords" json:"keywords"`
	Kinds     []string `yaml:"kinds" json:"kinds"`
	// TwoVoice is true when the approach reads well as two people taking turns (UGC only).
	TwoVoice bool     `yaml:"two_voice" json:"two_voice"`
	Requires []string `yaml:"requires" json:"requires,omitempty"`
	Hook     string   `yaml:"hook" json:"hook"`
	Beats    []string `yaml:"beats" json:"beats"`
	CTA      string   `yaml:"cta" json:"cta"`
	Honesty  []string `yaml:"honesty" json:"honesty,omitempty"`
}

// Fits reports whether the template suits a video kind.
func (t AdTemplate) Fits(kind string) bool { return contains(t.Kinds, kind) }

// ScriptQuality are the limits every video script is checked against before the owner sees
// it. A miss is a finding for the owner, never a block.
type ScriptQuality struct {
	HookMaxChars    int      `yaml:"hook_max_chars" json:"hook_max_chars"`
	FirstSceneMaxS  float64  `yaml:"first_scene_max_s" json:"first_scene_max_s"`
	MaxSceneS       float64  `yaml:"max_scene_s" json:"max_scene_s"`
	MaxCaptionChars int      `yaml:"max_caption_chars" json:"max_caption_chars"`
	WeakCTAs        []string `yaml:"weak_ctas" json:"weak_ctas"`
}

type adTemplateDoc struct {
	Version   int               `yaml:"version"`
	Defaults  map[string]string `yaml:"defaults"`
	VoiceTags []string          `yaml:"voice_tags"`
	Quality   ScriptQuality     `yaml:"quality"`
	Templates []AdTemplate      `yaml:"templates"`
}

// loadAdTemplates reads data/ad_templates.yaml into the library.
func (l *Library) loadAdTemplates() error {
	var doc adTemplateDoc
	if err := readYAML("data/ad_templates.yaml", &doc); err != nil {
		return err
	}
	l.adTemplates = map[string]AdTemplate{}
	for _, t := range doc.Templates {
		if _, dup := l.adTemplates[t.Key]; dup {
			return fmt.Errorf("library: ad template %q defined twice", t.Key)
		}
		l.adTemplates[t.Key] = t
		l.adOrder = append(l.adOrder, t.Key)
	}
	l.adDefaults = doc.Defaults
	l.VoiceTags = doc.VoiceTags
	l.Quality = doc.Quality
	return l.validateAdTemplates()
}

func (l *Library) validateAdTemplates() error {
	if len(l.adOrder) == 0 {
		return fmt.Errorf("library: no ad templates")
	}
	for _, key := range l.adOrder {
		t := l.adTemplates[key]
		if t.Key == "" || t.Name == "" || t.WhenToUse == "" || t.Hook == "" || t.CTA == "" || len(t.Beats) < 2 || len(t.Kinds) == 0 {
			return fmt.Errorf("library: ad template %q is incomplete", key)
		}
		for _, k := range t.Kinds {
			if !contains(adKinds, k) {
				return fmt.Errorf("library: ad template %q has unknown kind %q", key, k)
			}
		}
		for _, n := range t.Requires {
			if !contains(adNeeds, n) {
				return fmt.Errorf("library: ad template %q requires unknown fact %q", key, n)
			}
		}
	}
	for _, kind := range adKinds {
		t, ok := l.adTemplates[l.adDefaults[kind]]
		if !ok || !t.Fits(kind) || len(t.Requires) > 0 {
			return fmt.Errorf("library: the default ad template for %s must exist, suit it and need no extra facts", kind)
		}
	}
	q := l.Quality
	if q.HookMaxChars <= 0 || q.FirstSceneMaxS <= 0 || q.MaxSceneS <= 0 || q.MaxCaptionChars <= 0 {
		return fmt.Errorf("library: ad template quality limits must be positive")
	}
	for _, tag := range l.VoiceTags {
		if strings.ContainsAny(tag, "[]") || strings.TrimSpace(tag) == "" {
			return fmt.Errorf("library: voice tag %q must be plain words", tag)
		}
	}
	return nil
}

// AdTemplate returns one template by key.
func (l *Library) AdTemplate(key string) (AdTemplate, bool) {
	t, ok := l.adTemplates[key]
	return t, ok
}

// AdTemplateDefaults returns each video kind's default template key (a copy).
func (l *Library) AdTemplateDefaults() map[string]string {
	out := make(map[string]string, len(l.adDefaults))
	for k, v := range l.adDefaults {
		out[k] = v
	}
	return out
}

// AdTemplates returns every template in file order.
func (l *Library) AdTemplates() []AdTemplate {
	out := make([]AdTemplate, 0, len(l.adOrder))
	for _, k := range l.adOrder {
		out = append(out, l.adTemplates[k])
	}
	return out
}

// AdCriteria is what a template is chosen from.
type AdCriteria struct {
	// Key is a template the owner or the optimiser asked for; it wins when it can be used.
	Key string
	// Text is the request, objective, audience and offering, matched against the keywords.
	Text string
	// Kind is template, presenter or ugc; anything else counts as template.
	Kind string
	// Has names the facts that exist (NeedRealReviews, NeedFounderStory).
	Has map[string]bool
}

// usable reports whether a template suits the kind and every fact it needs exists.
func (c AdCriteria) usable(t AdTemplate) (bool, string) {
	if !t.Fits(c.Kind) {
		return false, fmt.Sprintf("%q does not suit a %s video", t.Name, c.Kind)
	}
	for _, n := range t.Requires {
		if !c.Has[n] {
			return false, fmt.Sprintf("%q needs %s, which this business has not given", t.Name, strings.ReplaceAll(n, "_", " "))
		}
	}
	return true, ""
}

// SelectAdTemplate picks the template for a script: the asked-for one when it can be used,
// else the best keyword match among the usable ones, else the kind's default. note explains,
// in plain words for the owner, why an asked-for template was not used ("" when it was, or
// when none was asked for).
func (l *Library) SelectAdTemplate(c AdCriteria) (t AdTemplate, note string) {
	if !contains(adKinds, c.Kind) {
		c.Kind = AdKindTemplate
	}
	if key := strings.TrimSpace(c.Key); key != "" {
		asked, ok := l.adTemplates[key]
		switch {
		case !ok:
			note = fmt.Sprintf("the approach %q is not known", key)
		default:
			var fits bool
			if fits, note = c.usable(asked); fits {
				return asked, ""
			}
		}
	}
	if best, ok := l.matchAdTemplate(c.Text, func(t AdTemplate) bool { ok, _ := c.usable(t); return ok }); ok {
		return best, note
	}
	return l.adTemplates[l.adDefaults[c.Kind]], note
}

// MatchAdTemplate is the template a request's words point to, if any (the prompt optimiser
// uses it; requirements are left to the writer's brief).
func (l *Library) MatchAdTemplate(text string) (AdTemplate, bool) {
	return l.matchAdTemplate(text, func(AdTemplate) bool { return true })
}

// matchAdTemplate scores each allowed template by its keywords found in text: a phrase as a
// whole-word substring, a single word as a token. The highest score wins; ties go to file
// order. No keyword found is no match.
func (l *Library) matchAdTemplate(text string, allowed func(AdTemplate) bool) (AdTemplate, bool) {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return AdTemplate{}, false
	}
	padded := " " + strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r == '\'' || r == '-' {
			return r
		}
		if r < '0' || (r > '9' && r < 'a') || r > 'z' {
			if r > 127 {
				return r
			}
			return ' '
		}
		return r
	}, text)), " ") + " "
	var best AdTemplate
	bestScore := 0
	for _, key := range l.adOrder {
		t := l.adTemplates[key]
		if !allowed(t) {
			continue
		}
		score := 0
		for _, kw := range t.Keywords {
			kw = strings.ToLower(strings.TrimSpace(kw))
			if kw != "" && strings.Contains(padded, " "+kw+" ") {
				score += len(strings.Fields(kw)) // a phrase counts more than one word
			}
		}
		if score > bestScore {
			best, bestScore = t, score
		}
	}
	return best, bestScore > 0
}

// AdTemplateBrief renders a template as the block the script writer receives.
func (l *Library) AdTemplateBrief(t AdTemplate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "APPROACH: %s (set \"approach\" to %q)\n", t.Name, t.Key)
	fmt.Fprintf(&b, "Use when: %s\n", t.WhenToUse)
	fmt.Fprintf(&b, "Hook, inside the first 2 seconds: %s\n", t.Hook)
	b.WriteString("Beats, in order:\n")
	for i, beat := range t.Beats {
		b.WriteString("  ")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(beat)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Call to action: %s The last scene ends on it; it is specific (the action, what they get, the channel), never \"learn more\" or \"get started\".\n", t.CTA)
	if len(t.Requires) > 0 {
		needs := make([]string, len(t.Requires))
		for i, n := range t.Requires {
			needs[i] = strings.ReplaceAll(n, "_", " ")
		}
		fmt.Fprintf(&b, "Needs: %s. If the facts, brand context or knowledge base do not hold them, do not use this approach; use the closest other one and say why in strategy.fix.\n",
			strings.Join(needs, ", "))
	}
	if len(t.Honesty) > 0 {
		b.WriteString("Honesty for this approach:\n")
		for _, h := range t.Honesty {
			b.WriteString("  - ")
			b.WriteString(h)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// VoiceTagsText tells the writer which delivery tags a line may open with.
func (l *Library) VoiceTagsText() string {
	if len(l.VoiceTags) == 0 {
		return ""
	}
	tags := make([]string, len(l.VoiceTags))
	for i, t := range l.VoiceTags {
		tags[i] = "[" + t + "]"
	}
	return "A voiceover line may open with at most one delivery tag, only from: " + strings.Join(tags, ", ") +
		". Use one only where the feeling changes (a surprise, a reassurance); most lines need none. Tags are never read aloud or captioned.\n"
}

// AllowedVoiceTag reports whether tag (without brackets) is a delivery tag the writer may use.
func (l *Library) AllowedVoiceTag(tag string) bool {
	tag = strings.ToLower(strings.TrimSpace(tag))
	for _, t := range l.VoiceTags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}
