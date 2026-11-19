package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/library"
	"github.com/bengobox/marketflow-ai/internal/llm"
	"github.com/bengobox/marketflow-ai/internal/promptopt"
)

// The Creative Studio asks for a video ad script directly: the brief already
// names the offering and the owner already chose the video type, so there is
// no optimiser, plan or clarifying question. The step is the same specialist
// work as in Ask (the video-script playbook, contract validation with one
// repair, then the compliance review), so the rules are enforced once.

// VideoScriptRequest asks the Content Creator for one video ad script.
type VideoScriptRequest struct {
	TenantID uuid.UUID
	Offering string // the offering's name; the facts go in Facts
	Facts    string // price, description, promotion code and end, all true
	// Format is the ad's format ("1920x1080" and so on); "" is written as a vertical ad.
	Format    string
	Objective string
	Audience  string
	Language  string // en (default) or sw
	Seconds   int    // the target length, 15 to 60
	Photos    int    // how many product photos the owner gave
	Footage   int    // how many video clips the owner gave
	// Presenter allows presenter scenes: an AI presenter talking to camera (presenter ads).
	Presenter bool
	// BrandPhotos is how many of the Photos (the last ones) are the business's brand images.
	BrandPhotos int
	// ActorNameStyle and ActorNames limit the names given to on-screen people (owner,
	// 2026-11-12): only names from the list, or names in the style, never others.
	ActorNameStyle string
	ActorNames     []string
	// Delivery is the voice delivery the owner chose (ad, ugc, kids, story, info). A UGC ad is
	// written on the ugc-video playbook, the only one whose scripts may use two voices.
	Delivery string
	// Approach is the ad template the owner asked for (library ad_templates.yaml); "" lets the
	// objective, audience and offering choose it.
	Approach string
	// Reviews are real reviews already published on the business's public page (owner,
	// 2026-11-14), each as "quote | who said it". Only with them can the testimonial approach be
	// used; they are quoted, never invented or reworded into new claims.
	Reviews []string
	// PresenterSpeaker is the gender of the actor already saved on the brief ("male" or
	// "female"). The script follows the actor (owner, 2026-11-14): every presenter line goes to
	// that voice, so the voice always matches the face and no new actor is drawn.
	PresenterSpeaker string
}

// DeliveryUGC is the creator-style delivery (media.DeliveryUGC; kept as a string here so the
// team package does not import media).
const DeliveryUGC = "ugc"

// adKind is the ad template kind of a script request.
func (in VideoScriptRequest) adKind() string {
	switch {
	case in.Delivery == DeliveryUGC:
		return library.AdKindUGC
	case in.Presenter:
		return library.AdKindPresenter
	}
	return library.AdKindTemplate
}

// founderWords hint that the brand context or the facts tell the founder's story.
var founderWords = []string{"founder", "founded", "started the business", "mwanzilishi", "alianzisha"}

// adFacts are the facts an ad template may need, as far as the request shows them.
func adFacts(in VideoScriptRequest, marketingContext string) map[string]bool {
	has := map[string]bool{library.NeedRealReviews: len(cleanReviews(in.Reviews)) > 0}
	text := strings.ToLower(marketingContext + "\n" + in.Facts)
	for _, w := range founderWords {
		if strings.Contains(text, w) {
			has[library.NeedFounderStory] = true
			break
		}
	}
	return has
}

// cleanReviews keeps at most five non-empty reviews of reasonable length.
func cleanReviews(list []string) []string {
	var out []string
	for _, r := range list {
		if r = strings.TrimSpace(r); r != "" && len(r) <= 400 {
			out = append(out, r)
		}
		if len(out) == 5 {
			break
		}
	}
	return out
}

// actorNameStyles are the naming styles a business can choose, as the writer reads them.
var actorNameStyles = map[string]string{
	"kikuyu": "Kikuyu", "luo": "Luo", "kalenjin": "Kalenjin", "kamba": "Kamba", "maasai": "Maasai",
	"swahili_coast": "Swahili coast", "neutral": "neutral, widely used Kenyan", "mixed": "a mix of Kenyan",
}

// actorNameRule tells the writer which names on-screen people may have. Names are cleaned to
// letters, spaces, apostrophes, hyphens and dots before they reach the prompt.
func actorNameRule(style string, names []string) string {
	var clean []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || len(n) > 40 || strings.ContainsFunc(n, func(r rune) bool {
			return !(unicode.IsLetter(r) || r == ' ' || r == '\'' || r == '-' || r == '.')
		}) {
			continue
		}
		clean = append(clean, n)
		if len(clean) == 20 {
			break
		}
	}
	if len(clean) > 0 {
		return fmt.Sprintf("If anyone on screen or in the voiceover is named, use only these names: %s. Never invent another name.\n",
			strings.Join(clean, ", "))
	}
	if s, ok := actorNameStyles[strings.ToLower(strings.TrimSpace(style))]; ok {
		return fmt.Sprintf("If anyone on screen or in the voiceover is named, give them %s names only.\n", s)
	}
	return ""
}

// VideoScriptResult is the reviewed script.
type VideoScriptResult struct {
	Script   *VideoScript `json:"script"`
	Findings []Finding    `json:"findings,omitempty"`
	CostUSD  float64      `json:"cost_usd"`
	Calls    []llm.Call   `json:"calls,omitempty"`
}

// ErrScriptBlocked is a script the review blocked; its findings say why.
var ErrScriptBlocked = errors.New("team: the script was blocked by the review")

// scriptBudget sits under marketflow-api's 100 s wait for the script.
var scriptBudget = 90 * time.Second

// templateShots are the shots a template video ad can render.
var templateShots = map[string]bool{"product_clip": true, "b_roll": true, "owner_footage": true, "end_card": true}

// WriteVideoScript writes and reviews one template video ad script.
func (r *Runtime) WriteVideoScript(ctx context.Context, in VideoScriptRequest) (*VideoScriptResult, error) {
	if strings.TrimSpace(in.Offering) == "" {
		return nil, fmt.Errorf("team: the offering is required")
	}
	if in.Seconds < 15 || in.Seconds > 60 {
		in.Seconds = 30
	}
	if in.Photos < 1 && in.Footage < 1 {
		return nil, fmt.Errorf("team: a video ad needs at least one product photo or clip")
	}
	ctx, cancel := context.WithTimeout(ctx, scriptBudget)
	defer cancel()
	meter := llm.MeterFrom(ctx)
	if meter == nil {
		meter = &llm.Meter{}
		ctx = llm.WithMeter(ctx, meter)
	}
	tc := TenantContext{}
	if r.source != nil {
		if loaded, err := r.source.Load(ctx, in.TenantID); err == nil {
			tc = loaded
		} else {
			r.logger.Warn("tenant context unavailable", zap.Error(err))
		}
	}
	playbook := "video-script"
	if in.Delivery == DeliveryUGC {
		playbook = "ugc-video"
	}
	pb, ok := r.lib.Playbook(playbook)
	if !ok {
		return nil, fmt.Errorf("team: the %s playbook is missing", playbook)
	}
	m, ok := r.member("content_creator", tc)
	if !ok {
		m, _ = r.member("manager", tc)
	}
	tpl, note := r.lib.SelectAdTemplate(library.AdCriteria{Key: in.Approach, Kind: in.adKind(),
		Text: strings.Join([]string{in.Objective, in.Audience, in.Offering}, " "), Has: adFacts(in, tc.MarketingContext)})
	twoVoice := r.twoVoice[pb.Key] && tpl.TwoVoice
	task := scriptTask(in) + approachTask(r.lib, tpl, in.Reviews, twoVoice)
	spec := &promptopt.TaskSpec{Playbook: pb.Key, Specialist: m.Key, OptimisedBrief: task, Original: task, Approach: tpl.Key,
		Entities: promptopt.Entities{Offering: in.Offering, Objective: in.Objective, Audience: in.Audience, Language: in.Language}}
	step := r.runStepSafe(ctx, in.TenantID, plannedStep{member: m, playbook: pb, task: task}, spec, tc, "", 0)
	out := &VideoScriptResult{}
	if note != "" {
		out.Findings = append(out.Findings, Finding{Quote: in.Approach, Severity: "low", Issue: note,
			Fix: fmt.Sprintf("The script follows %q instead.", tpl.Name)})
	}
	defer func() {
		out.Calls = meter.Calls()
		_, out.CostUSD = meter.Totals()
	}()
	if step.Error != "" || step.Output == nil {
		return out, fmt.Errorf("team: no usable script: %s", step.Error)
	}
	_, findings := r.review(ctx, &step, spec, tc, 0)
	out.Findings = append(out.Findings, findings...)
	if step.Error != "" || step.Output == nil {
		return out, ErrScriptBlocked
	}
	var v VideoScript
	if err := json.Unmarshal(step.Output, &v); err != nil {
		return out, fmt.Errorf("team: unreadable script: %w", err)
	}
	fitTemplate(&v, in.Photos, in.Footage, in.Presenter)
	// Offers and calls to action the brief does not back are taken out and reported for review.
	out.Findings = append(out.Findings, GuardOffers(&v, in.Offering+"\n"+in.Facts)...)
	if _, ok := r.lib.AdTemplate(v.Approach); !ok {
		v.Approach = tpl.Key
	}
	// fitTemplate may have turned presenter scenes into narration, so the speakers are made
	// consistent again for the shots as they now are.
	r.polishScript(&v, pb.Key, tpl.Key, NormalizeSpeaker(in.PresenterSpeaker))
	out.Findings = append(out.Findings, QualityFindings(v, r.lib.Quality)...)
	out.Script = &v
	return out, nil
}

// finishVideoScript polishes a parsed video_script reply (runStep): delivery tags cleaned,
// an unknown approach cleared and the speakers made consistent for the playbook.
func (r *Runtime) finishVideoScript(raw json.RawMessage, playbook, approach string) json.RawMessage {
	var v VideoScript
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	r.polishScript(&v, playbook, approach, "")
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}

// polishScript cleans each line's delivery tags and assigns speakers. Two voices are kept only
// on a two-voice playbook (MF_AD_TWO_VOICE_PLAYBOOKS) and when the approach reads well as two
// people; approach is the one the task asked for, else the one the writer named.
func (r *Runtime) polishScript(v *VideoScript, playbook, approach, presenter string) {
	if r.lib == nil {
		return
	}
	if _, ok := r.lib.AdTemplate(v.Approach); !ok {
		v.Approach = ""
	}
	if approach == "" {
		approach = v.Approach
	}
	tpl, known := r.lib.AdTemplate(approach)
	for i := range v.Scenes {
		v.Scenes[i].Voiceover = CleanVoiceTags(v.Scenes[i].Voiceover, r.lib.AllowedVoiceTag)
	}
	AssignSpeakers(v, r.twoVoice[playbook] && (!known || tpl.TwoVoice), presenter)
}

// shapeOf names an ad format's shape for the writer: landscape, square, 4:5 or vertical.
func shapeOf(format string) string {
	switch format {
	case "1920x1080":
		return "landscape (16:9)"
	case "1080x1080":
		return "square"
	case "1080x1350":
		return "4:5 feed"
	}
	return "vertical"
}

// scriptTask tells the writer what this ad can show.
func scriptTask(in VideoScriptRequest) string {
	lang := "English"
	if in.Language == "sw" {
		lang = "Swahili"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Write a %d-second %s video ad script in %s for: %s.\n", in.Seconds, shapeOf(in.Format), lang, in.Offering)
	if f := strings.TrimSpace(in.Facts); f != "" {
		fmt.Fprintf(&b, "Facts you may use (use no other prices or claims):\n%s\n", trimTo(f, 1500))
	}
	if in.Objective != "" {
		fmt.Fprintf(&b, "Objective: %s.\n", in.Objective)
	}
	if in.Audience != "" {
		fmt.Fprintf(&b, "Audience: %s.\n", in.Audience)
	}
	b.WriteString("Shots available, and only these: product_clip shows one of the offering's own photos; " +
		"b_roll shows the setting its visual line describes (a picture is made from that line, so describe a real scene with no writing in it); ")
	if in.Footage > 0 {
		fmt.Fprintf(&b, "owner_footage shows one of the owner's %d video clips (asset 1 to %d); ", in.Footage, in.Footage)
	}
	if in.Presenter {
		b.WriteString("presenter is one AI presenter talking to camera: its voiceover is what the presenter says, at most 200 characters per presenter scene. " +
			"Open with a presenter hook and use presenter for at most half of the scenes. The presenter explains and recommends; it never claims to have used the product or had a result itself. ")
		b.WriteString("Give each scene a short visual line describing what is seen (for b_roll, the setting to film; never text on screen). ")
		b.WriteString("end_card is the last scene, with the call to action.\n")
	} else {
		b.WriteString("end_card is the last scene, with the call to action. There is no presenter and no person on screen.\n")
	}
	if in.Photos > 0 {
		fmt.Fprintf(&b, "The owner gave %d photos: set asset to 1 to %d to choose one per scene.\n", in.Photos, in.Photos)
		if bp := min(max(in.BrandPhotos, 0), in.Photos); bp > 0 {
			fmt.Fprintf(&b, "Photos %d to %d are pictures of the business itself (its place, people or look), not the product: "+
				"use them for b_roll and the opening, and the others for product scenes.\n", in.Photos-bp+1, in.Photos)
		}
	}
	b.WriteString(actorNameRule(in.ActorNameStyle, in.ActorNames))
	fmt.Fprintf(&b, "The voiceover of each scene is what the narrator says during it. Keep all the voiceover together under %d characters "+
		"so the ad fits %d seconds, and keep each caption to a few words.", in.Seconds*14, in.Seconds)
	b.WriteString(" Set mood to a few words on how the background music should feel. Give a scene an sfx cue " +
		"(at most 60 characters, a plain sound such as \"soft whoosh\") only where a sound lifts that moment; most scenes have none.")
	return b.String()
}

// approachTask adds the chosen ad template, the delivery tags, the real reviews the script may
// quote and, for a two-voice UGC ad, how to give each line to a speaker.
func approachTask(lib *library.Library, tpl library.AdTemplate, reviews []string, twoVoice bool) string {
	if lib == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n")
	b.WriteString(lib.AdTemplateBrief(tpl))
	b.WriteString(lib.VoiceTagsText())
	if list := cleanReviews(reviews); len(list) > 0 {
		b.WriteString("Real customer reviews you may quote word for word, with the attribution given (never reword them into a stronger claim):\n")
		for _, r := range list {
			b.WriteString("- ")
			b.WriteString(r)
			b.WriteString("\n")
		}
	}
	if twoVoice {
		b.WriteString("Two voices may read this ad, a man and a woman taking turns, when a back-and-forth helps it " +
			"(a question and its answer, a myth and the fact, one showing the other). Set each spoken scene's speaker to " +
			"\"male\" or \"female\" by who would naturally say that line, and alternate turns. Every presenter scene uses the on-screen " +
			"presenter's speaker. If one voice suits this ad better, leave every speaker empty.\n")
	} else {
		b.WriteString("One voice reads this whole ad: leave every speaker empty.\n")
	}
	return b.String()
}

// fitTemplate makes a script renderable as a template ad: a presenter scene
// becomes narration over a photo, and an asset beyond what the owner gave is
// left for the renderer to pick in order.
func fitTemplate(v *VideoScript, photos, footage int, presenter bool) {
	for i := range v.Scenes {
		s := &v.Scenes[i]
		if presenter && s.Shot == "presenter" {
			continue
		}
		if !templateShots[s.Shot] || (s.Shot == "owner_footage" && footage == 0) {
			s.Shot = "b_roll"
		}
		limit := photos
		if s.Shot == "owner_footage" {
			limit = footage
		}
		if s.Asset > limit {
			s.Asset = 0
		}
	}
	if photos == 0 {
		// Footage only: every scene but a presenter's shows a clip.
		for i := range v.Scenes {
			if !(presenter && v.Scenes[i].Shot == "presenter") {
				v.Scenes[i].Shot = "owner_footage"
			}
		}
	}
}
