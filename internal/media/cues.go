package media

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Local cues (owner, 2026-11-24, plan item 66 d): a short, true piece of writing that makes a scene
// feel local, such as a plain LIPA HAPA card on a counter. It comes only from the configured list
// (service config creative.local_cues, platform default with a tenant override), is drawn only on
// routes the platform says render text well (RoutePolicy.TextRoutes), and stays only when the
// self-review reads it as written; otherwise the scene is drawn without it. Every other still keeps
// the ban on writing.
//
// No third party's name or logo is ever asked of an image model: the configured trade marks
// (M-Pesa and the like) are put in neutral words before a generated scene is drawn. A tenant that
// wants the real sign supplies its own picture of it, which a matching scene uses as its frame.

// LocalCue is one configured cue: a scene whose visual has one of the Match words may carry Text on
// the Object.
type LocalCue struct {
	Match  []string `json:"match"`
	Object string   `json:"object"`
	Text   string   `json:"text"`
}

// CueRules are the local cue settings.
type CueRules struct {
	Enabled   bool       `json:"enabled"`
	MaxScenes int        `json:"max_scenes"`
	Cues      []LocalCue `json:"cues"`
	// Marks are trade marks and the neutral words a generated scene says instead.
	Marks map[string]string `json:"marks"`
}

// Limits on a cue, whatever the setting says: short writing only, with no digits, so a model never
// draws an invented till, phone or price number.
const (
	maxCueScenes = 2
	maxCueChars  = 20
)

var cueText = regexp.MustCompile(`^[\p{L}][\p{L} '&-]*$`)

func (c LocalCue) ok() bool {
	t := strings.TrimSpace(c.Text)
	return strings.TrimSpace(c.Object) != "" && len([]rune(t)) >= 2 && len([]rune(t)) <= maxCueChars && cueText.MatchString(t) && len(c.Match) > 0
}

// CapTextStill is a storyboard still with a few words in it.
const CapTextStill Capability = "text_still"

// TextStillRoutes can draw a still with words; which of them render text well enough to use is the
// platform's setting (RoutePolicy.TextRoutes).
var TextStillRoutes = []Route{
	{Key: "fal/nano-banana-pro", Provider: "fal", Capability: CapTextStill, PriceUSD: 0.15, Quality: 5, PAYG: true, Timeout: 90 * time.Second, MinTier: TierPremium},
	{Key: "fal/seedream-v5-lite", Provider: "fal", Capability: CapTextStill, PriceUSD: 0.035, Quality: 4, PAYG: true, Timeout: 60 * time.Second},
	{Key: "higgsfield/soul-v2", Provider: "higgsfield", Capability: CapTextStill, PriceUSD: 0.0031, Quality: 3.5, PAYG: true, Timeout: 90 * time.Second},
}

// textRoutes are the routes a cue may be drawn on now, for the storyboard's tier.
func (s *Storyboards) textRoutes(ctx context.Context, tier string) []Route {
	if tier != TierPremium {
		tier = TierStandard
	}
	allowed := Policy(ctx).TextRoutes
	return s.router.OrderPreferred(CapTextStill, tier, 0, func(rt Route) bool {
		return slices.Contains(allowed, rt.Key) && usable(ctx, s.keys, rt, rt.Cost(0))
	}, allowed)
}

// prepareCues plans which scenes carry a cue: generated b-roll scenes with no placement.
func (s *Storyboards) prepareCues(ctx context.Context, req *StoryboardRequest, bp *brandPlan, generated func(string) bool) {
	if req.Cues == nil || !req.Cues.Enabled || len(req.Cues.Cues) == 0 || len(s.textRoutes(ctx, req.Tier)) == 0 {
		return
	}
	bp.cues = planCues(req.Scenes, *req.Cues, func(id string) bool {
		_, placed := bp.placements[id]
		return generated(id) && !placed
	})
}

func planCues(scenes []ScriptScene, rules CueRules, free func(sceneID string) bool) map[string]LocalCue {
	limit := rules.MaxScenes
	if limit <= 0 || limit > maxCueScenes {
		limit = 1
	}
	out := map[string]LocalCue{}
	used := map[string]bool{}
	for _, sc := range scenes {
		if len(out) >= limit {
			break
		}
		if sc.Shot != ShotBRoll || !free(sc.ID) {
			continue
		}
		for _, c := range rules.Cues {
			if c.ok() && !used[c.Text] && mentions(sc.Visual, c.Match) {
				out[sc.ID], used[c.Text] = c, true
				break
			}
		}
	}
	return out
}

// mentions reports whether text has one of the words or phrases as whole words.
func mentions(text string, words []string) bool {
	for _, w := range words {
		if w = strings.TrimSpace(w); w != "" && regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(w)+`\b`).MatchString(text) {
			return true
		}
	}
	return false
}

// noWriting is the ban on writing a cue's prompt narrows to everything else.
var noWriting = regexp.MustCompile(`(?i)no text, no letters, no words,`)

// withCue asks for exactly the cue's words and no other writing.
func withCue(prompt string, c LocalCue) string {
	return noWriting.ReplaceAllString(prompt, "no other text, no other letters,") +
		fmt.Sprintf(" The only writing in the picture is %q, in plain print on %s, spelled exactly so.", strings.TrimSpace(c.Text), strings.TrimSpace(c.Object))
}

// cueExpect is what the reviewer checks a cue against.
func cueExpect(text string) string {
	return fmt.Sprintf("the exact words %q once, spelled correctly and natural in the scene; any other writing or a misspelling is a high text_artefact. ", text)
}

// sceneStill draws one planned still; a cue goes on a text route only, and when none works the scene
// is drawn without it.
func (s *Storyboards) sceneStill(ctx context.Context, req StoryboardRequest, name string, st *Still) (url, key, route string, cost float64, err error) {
	if st.Cue != "" {
		prompt := st.Prompt
		url, key, route, cost, err = s.draw(ctx, req, name, s.textRoutes(ctx, req.Tier), func(ctx context.Context, rt Route) (string, error) {
			return s.generate(ctx, rt, req.Format, prompt)
		})
		if err == nil {
			return url, key, route, cost, nil
		}
		st.Cue, st.Prompt = "", st.plainPrompt
	}
	return s.still(ctx, req, name, st.Prompt)
}

// settleCues keeps a cue only when the review answered and found nothing wrong with its scene; any
// other is drawn again without it. A scene that cannot be drawn again fails the storyboard, as any
// still does.
func (s *Storyboards) settleCues(ctx context.Context, req StoryboardRequest, res *StoryboardResult, passed func(sceneID string) bool) error {
	for i := range res.Stills {
		st := &res.Stills[i]
		if st.Cue == "" || st.Reused || passed(st.SceneID) {
			continue
		}
		s.logger.Info("local cue not kept", zap.String("scene", st.SceneID))
		st.Cue, st.Prompt = "", st.plainPrompt
		url, key, route, cost, err := s.still(ctx, req, "sb-"+st.SceneID+"-nocue", st.Prompt)
		if err != nil {
			return fmt.Errorf("media: scene %s: %w", st.SceneID, err)
		}
		st.URL, st.Key, st.Route = url, key, route
		st.CostUSD += cost
		res.CostUSD += cost
	}
	return nil
}

// NeutralScenes puts the trade marks in a generated or presenter scene's visual and setting in
// neutral words. Scenes that start from the tenant's own photo or footage are left as written, so a
// scene naming a sign still matches the tenant's real picture of it. The spoken words never change.
func NeutralScenes(scenes []ScriptScene, rules *CueRules, photos, footage []string, info []PhotoInfo) []ScriptScene {
	if rules == nil || len(rules.Marks) == 0 {
		return scenes
	}
	marks := make([]string, 0, len(rules.Marks))
	for m := range rules.Marks {
		if strings.TrimSpace(m) != "" {
			marks = append(marks, m)
		}
	}
	// The longest first, so "lipa na m-pesa" goes before "m-pesa".
	sort.Slice(marks, func(i, j int) bool {
		if len(marks[i]) != len(marks[j]) {
			return len(marks[i]) > len(marks[j])
		}
		return marks[i] < marks[j]
	})
	frames := newFrameChooser(photos, footage, info)
	out := slices.Clone(scenes)
	for i := range out {
		if src := frames.choose(out[i]).Source; src != FrameGenerated && src != FrameActor {
			continue
		}
		for _, m := range marks {
			re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(strings.TrimSpace(m)) + `\b`)
			out[i].Visual = re.ReplaceAllLiteralString(out[i].Visual, rules.Marks[m])
			out[i].Setting = re.ReplaceAllLiteralString(out[i].Setting, rules.Marks[m])
		}
	}
	return out
}
