package media

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// A storyboard is the cheap look at an ad before any video is paid for: one still per
// scene, about 1 credit in all (registry "storyboard"). The owner approves it, then the
// render uses exactly these frames:
//   - a presenter scene shows the ad's one actor still, which is also the reference the
//     presenter route lip-syncs, so every presenter scene has the same face;
//   - a product scene shows the offering's own photo (the product is never generated);
//   - any other scene shows a brand-library picture whose caption or tags match it, or a
//     still generated from its visual in the ad's shape, which the clip route then animates
//     (frames.go has the rule; a logo is never a frame);
//   - owner footage shows no still (the footage is the scene).

// CapStill is a storyboard frame or the actor still.
const CapStill Capability = "still"

// StillRoutes start on Higgsfield Soul v2 (USD 0.0031 an image, measured 2026-11-03),
// with fal Seedream v5 lite as the cross-provider fallback.
var StillRoutes = []Route{
	{Key: "higgsfield/soul-v2", Provider: "higgsfield", Capability: CapStill, PriceUSD: 0.0031, Quality: 3.5, PAYG: true, Timeout: 90 * time.Second},
	{Key: "fal/seedream-v5-lite", Provider: "fal", Capability: CapStill, PriceUSD: 0.035, Quality: 4, PAYG: true, Timeout: 60 * time.Second, Fallback: true},
}

// Storyboard still sources.
const (
	StillActor     = "actor"
	StillPhoto     = "photo"
	StillGenerated = "generated"
	StillFootage   = "footage"
)

// DefaultActor describes the presenter when the brief names none.
const DefaultActor = "a friendly Kenyan presenter in their thirties, smart casual clothes"

// StoryboardRequest asks for one still per scene and, for a presenter ad, the actor.
type StoryboardRequest struct {
	TenantID  uuid.UUID
	VersionID string
	Format    string
	Kind      string // AdTemplate, AdPresenter or AdPresenterPremium
	Scenes    []ScriptScene
	Photos    []string
	// PhotoInfo describes Photos by index: the offering's own photos, or brand-library pictures
	// with their caption, tags and logo flag (frames.go).
	PhotoInfo []PhotoInfo
	// Actor is the brief's existing actor: kept, so every version of the brief has the same
	// presenter, until the owner asks for a new one (marketflow-api then sends none). Empty makes
	// one from ActorPrompt.
	Actor       *Actor
	ActorPrompt string
	// Delivery is the ad's voice delivery; DeliveryUGC draws generated stills and the actor as
	// candid phone photos rather than advertising photos.
	Delivery string
	// Tier is the still routes' tier: premium for the platform owner's tenant (policy.go), else standard.
	Tier string
	// Brand and Place tell the self-review whose ad it is and where the business is (review.go).
	Brand string
	Place string
	// EndCard and LogoURL are the end card's words, contacts and logo: composed over the end card's
	// still as a preview, so the plan the owner approves shows the end card that renders.
	EndCard *Overlay
	LogoURL string
	// Previous are the stills of the storyboard this one replaces, sent only when the owner keeps
	// unchanged scenes (owner, 2026-11-24): a generated still whose scene and exact prompt are the
	// same is reused, so only the scenes that changed are drawn again.
	Previous []Still
	// Branding are the in-scene branding rules (branding.go); nil places no logo in the scenes.
	Branding *BrandingRules
	// Screens are the screenshot rules and Screenshots the tenant's own screens (screens.go).
	Screens     *ScreenRules
	Screenshots []Screenshot
	// Face is the consented person a new actor takes its look from (faces.go); nil for none.
	Face *Face
	// Cues are the local cue rules (cues.go); nil draws no cue.
	Cues *CueRules
	// brand is the prepared placements (PrepareBranding).
	brand *brandPlan
}

// Actor is an ad's one presenter: the still every presenter scene is lip-synced from.
// The seed is recorded with it; the presenter routes confirmed so far take the image as
// the identity reference and no seed.
type Actor struct {
	URL    string `json:"url"`
	Key    string `json:"key,omitempty"`
	Prompt string `json:"prompt"`
	Seed   int64  `json:"seed"`
	Route  string `json:"route,omitempty"`
	// Gender is the actor's look ("male", "female" or "" when the description does not say).
	// Later scripts give the presenter's lines to the matching voice (owner, 2026-11-14).
	Gender string `json:"gender,omitempty"`
	// FaceID is the consented person the actor was drawn from, or "" (faces.go).
	FaceID string `json:"face_id,omitempty"`
	// Logo, PlainURL and PlainKey: the presenter's still with the real logo on a banner behind
	// them, and the still without it (branding.go).
	Logo     *PlacedLogo `json:"logo,omitempty"`
	PlainURL string      `json:"plain_url,omitempty"`
	PlainKey string      `json:"plain_key,omitempty"`
}

// Still is one scene's storyboard frame.
type Still struct {
	SceneID string  `json:"scene_id"`
	URL     string  `json:"url,omitempty"`
	Key     string  `json:"key,omitempty"`
	Source  string  `json:"source"`
	Route   string  `json:"route,omitempty"`
	CostUSD float64 `json:"cost_usd,omitempty"`
	// Photo and Origin say which photo a photo frame is (1-based) and whether it is the
	// offering's own or a matched brand-library picture.
	Photo  int    `json:"photo,omitempty"`
	Origin string `json:"origin,omitempty"`
	// Prompt is the exact prompt a generated still was drawn from.
	Prompt string `json:"prompt,omitempty"`
	// Reused marks a generated still kept from the previous storyboard because its prompt did not
	// change (StoryboardRequest.Previous): it costs nothing and is not reviewed again.
	Reused bool `json:"reused,omitempty"`
	// Logo is the real logo placed on a surface in the still (branding.go); PlainURL and PlainKey
	// are the still without it, the fallback when a clip would smear the logo.
	Logo     *PlacedLogo `json:"logo,omitempty"`
	PlainURL string      `json:"plain_url,omitempty"`
	PlainKey string      `json:"plain_key,omitempty"`
	// Screen is the tenant's own screenshot placed on a device in the still (screens.go). The scene
	// is rendered as a still, never animated by a clip route.
	Screen *PlacedScreen `json:"screen,omitempty"`
	// Cue is the short writing the still carries (cues.go); the scene is rendered as a still too,
	// because a clip route would smear it. plainPrompt is the prompt without it.
	Cue         string `json:"cue,omitempty"`
	plainPrompt string
}

// StoryboardResult is the approved-to-be storyboard.
type StoryboardResult struct {
	Actor   *Actor  `json:"actor,omitempty"`
	Stills  []Still `json:"stills"`
	CostUSD float64 `json:"cost_usd"`
	// FaceDrawn: a new actor was drawn from the request's face.
	FaceDrawn bool `json:"-"`
	// Review is what the self-review found and redrew (review.go).
	Review *ReviewReport `json:"review,omitempty"`
	// EndCardPreview is the end card as it will render: its still with our overlay drawn on it.
	EndCardPreview *EndCardPreview `json:"end_card_preview,omitempty"`
}

// Storyboards makes storyboards.
type Storyboards struct {
	router *Router
	keys   KeySource
	store  Store
	posts  *Posters
	queue  *queueClient
	logger *zap.Logger
	// endpoints are the provider endpoint ids (image_endpoints.go).
	endpoints ImageEndpoints
}

// NewStoryboards builds the storyboard pipeline; stills are stored like posters.
func NewStoryboards(keys KeySource, store Store, posters *Posters, logger *zap.Logger) *Storyboards {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Storyboards{router: NewRouter(slices.Concat(StillRoutes, IdentityRoutes, TextStillRoutes)), keys: keys, store: store, posts: posters, queue: newQueueClient(),
		logger: logger.Named("media.storyboards"), endpoints: DefaultImageEndpoints()}
}

// Ready reports whether storyboards can be made here.
func (s *Storyboards) Ready() bool { return s != nil && s.store != nil && s.posts != nil }

// Plan lists what each scene's still will be, without generating anything. Generated
// counts the stills that cost money, for the quote.
func (s *Storyboards) Plan(req StoryboardRequest) (stills []Still, generated int) {
	frames := newFrameChooser(req.Photos, nil, req.PhotoInfo)
	for _, sc := range req.Scenes {
		st := Still{SceneID: sc.ID}
		f := frames.choose(sc)
		st.Source = f.Source
		switch f.Source {
		case FramePhoto:
			st.URL, st.Photo, st.Origin = f.URL, f.Photo, f.Origin
		case FrameGenerated:
			visual := sceneVisual(sc)
			placed := false
			if p, ok := req.brand.placement(sc.ID); ok {
				placed = true
				// Added after the picture-only filter, which would drop a banner (branding.go).
				visual += ". In the scene: " + p.Surface
			}
			st.Prompt = StillPrompt(req.Format, req.Delivery, visual)
			if c, ok := req.brand.cue(sc.ID); ok {
				st.plainPrompt, st.Prompt, st.Cue = st.Prompt, withCue(st.Prompt, c), strings.TrimSpace(c.Text)
			}
			// A vouched still (no prompt) predates the planned banner, so it is drawn again.
			// So does one whose screenshot the business has since removed or replaced.
			planned, _ := req.brand.placement(sc.ID)
			if p, ok := previousStill(req.Previous, sc.ID, st.Prompt); ok && !(placed && p.Prompt == "") &&
				(p.Prompt != "" || p.Cue == st.Cue) && (p.Screen == nil) == (planned.ScreenID == "") && (p.Screen == nil || p.Screen.ID == planned.ScreenID) {
				st.URL, st.Key, st.Route, st.Reused = p.URL, p.Key, p.Route, true
				st.Logo, st.PlainURL, st.PlainKey, st.Screen = p.Logo, p.PlainURL, p.PlainKey, p.Screen
				break
			}
			generated++
		}
		stills = append(stills, st)
	}
	if isPresenterKind(req.Kind) && (req.Actor == nil || req.Actor.URL == "") {
		generated++
	}
	return stills, generated
}

// Make draws the actor (when needed) and the generated stills, in parallel, and stores them.
func (s *Storyboards) Make(ctx context.Context, req StoryboardRequest) (*StoryboardResult, error) {
	if !s.Ready() {
		return nil, fmt.Errorf("media: storyboards need object storage")
	}
	if _, ok := Formats[req.Format]; !ok {
		return nil, &UserError{Msg: "format must be " + FormatsText}
	}
	if len(req.Scenes) == 0 || len(req.Scenes) > MaxAdScenes {
		return nil, &UserError{Msg: fmt.Sprintf("a storyboard has 1 to %d scenes", MaxAdScenes)}
	}
	s.PrepareBranding(ctx, &req)
	stills, _ := s.Plan(req)
	res := &StoryboardResult{Stills: stills}

	if isPresenterKind(req.Kind) {
		if req.Actor != nil && req.Actor.URL != "" {
			res.Actor = req.Actor
		} else {
			desc := strings.TrimSpace(req.ActorPrompt)
			if desc == "" {
				desc = DefaultActor
			}
			// The actor's gender follows the presenter's speaker, so the lip-synced voice fits.
			desc = actorWithGender(desc, presenterSpeaker(req.Scenes))
			setting := presenterSetting(req.Scenes)
			if p, ok := req.brand.placement("actor"); ok {
				setting = strings.TrimSpace(setting + "; " + p.Surface)
			}
			face := ""
			if req.Face != nil {
				face = req.Face.ID
			}
			url, key, route, cost, faceID, err := s.actorStill(ctx, req, "actor", desc, setting, face)
			if err != nil {
				return nil, fmt.Errorf("media: actor still: %w", err)
			}
			res.Actor = &Actor{URL: url, Key: key, Prompt: desc, Seed: seedFor(req.VersionID, desc), Route: route,
				Gender: ActorGender(desc), FaceID: faceID}
			res.CostUSD += cost
			res.FaceDrawn = faceID != ""
		}
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		sem  = make(chan struct{}, 3)
	)
	for i := range res.Stills {
		st := &res.Stills[i]
		switch st.Source {
		case StillActor:
			if res.Actor != nil {
				st.URL = res.Actor.URL
			}
			continue
		case StillGenerated:
			if st.Reused {
				continue
			}
		default:
			continue
		}
		wg.Add(1)
		go func(st *Still) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			url, key, route, cost, err := s.sceneStill(ctx, req, "sb-"+st.SceneID, st)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("scene %s: %w", st.SceneID, err))
				return
			}
			st.URL, st.Key, st.Route, st.CostUSD = url, key, route, cost
			res.CostUSD += cost
		}(st)
	}
	wg.Wait()
	if len(errs) > 0 {
		return nil, errs[0]
	}
	// The self-review looks at the generated stills and the actor before the owner does.
	if err := s.settleCues(ctx, req, res, s.reviewStills(ctx, req, res)); err != nil {
		return nil, err
	}
	// The real logo and screenshots go on the planned surfaces of the reviewed stills (branding.go).
	if err := s.placeArt(ctx, req, res); err != nil {
		return nil, err
	}
	// The end card is previewed after the review, so a redrawn end card still is the one shown.
	res.EndCardPreview = s.endCardPreview(ctx, req, res)
	return res, nil
}

// still makes one image on the best route with fallbacks and stores it as a JPEG.
func (s *Storyboards) still(ctx context.Context, req StoryboardRequest, name, prompt string) (url, key, route string, cost float64, err error) {
	ok := func(rt Route) bool { return usable(ctx, s.keys, rt, rt.Cost(0)) }
	tier := req.Tier
	if tier != TierPremium {
		tier = TierStandard
	}
	routes := s.router.OrderPreferred(CapStill, tier, 0, ok, prefer(ctx, ScenarioStill, tier))
	if len(routes) == 0 && tier == TierPremium {
		// No premium still route can take it: the standard ones do.
		routes = s.router.OrderPreferred(CapStill, TierStandard, 0, ok, prefer(ctx, ScenarioStill, TierStandard))
	}
	return s.draw(ctx, req, name, routes, func(ctx context.Context, rt Route) (string, error) {
		return s.generate(ctx, rt, req.Format, prompt)
	})
}

// draw tries routes in order, each making one image with gen, and stores the first that works.
func (s *Storyboards) draw(ctx context.Context, req StoryboardRequest, name string, routes []Route,
	gen func(ctx context.Context, rt Route) (string, error)) (url, key, route string, cost float64, err error) {
	var lastErr error
	for _, rt := range routes {
		if ctx.Err() != nil {
			break
		}
		// The platform's still timeout (routing still_timeout_seconds) covers provider queues; the
		// route's own timeout is the fallback.
		timeout := rt.Timeout
		if s := Policy(ctx).StillTimeoutSeconds; s > 0 {
			timeout = time.Duration(s) * time.Second
		}
		actx, cancel := context.WithTimeout(ctx, timeout)
		src, rerr := gen(actx, rt)
		var data []byte
		if rerr == nil {
			img, derr := s.posts.download(actx, src)
			if derr == nil {
				data, derr = EncodeJPEG(img)
			}
			rerr = derr
		}
		cancel()
		if rerr != nil {
			lastErr = rerr
			s.router.Failed(rt.Key, rerr, ctx.Err() != nil)
			s.logger.Warn("still route failed, trying the next", zap.String("route", rt.Key), zap.Error(rerr))
			continue
		}
		s.router.Succeeded(rt.Key)
		k, u, perr := s.store.PutPoster(ctx, req.TenantID, req.VersionID+"-"+name, data)
		if perr != nil {
			return "", "", "", 0, fmt.Errorf("media: store still: %w", perr)
		}
		return u, k, rt.Key, rt.PriceUSD, nil
	}
	if lastErr == nil {
		return "", "", "", 0, ErrNoRoute
	}
	return "", "", "", 0, fmt.Errorf("%w: %w", ErrNoRoute, lastErr)
}

// generate runs one still route. Soul v2 takes prompt and aspect_ratio (confirmed by its
// validation and the bake-off renders); Seedream as the poster pipeline calls it.
func (s *Storyboards) generate(ctx context.Context, rt Route, format, prompt string) (string, error) {
	key := s.keys(ctx, rt.Provider)
	var (
		job   queueJob
		input map[string]any
	)
	switch rt.Key {
	case "higgsfield/soul-v2":
		job, input = higgsfieldJob(key, s.endpoints.SoulStill), map[string]any{"prompt": prompt, "aspect_ratio": soulAspect(format)}
	case "fal/seedream-v5-lite":
		job = falJob(key, s.endpoints.SeedreamText)
		input = map[string]any{"prompt": prompt, "image_size": seedreamSize(format), "num_images": 1}
	case "fal/nano-banana-pro":
		job = falJob(key, s.endpoints.NanoBananaPro)
		input = map[string]any{"prompt": prompt, "aspect_ratio": aspectFor(format), "resolution": "2K", "output_format": "jpeg", "num_images": 1}
	default:
		return "", fmt.Errorf("media: no builder for route %s", rt.Key)
	}
	return s.runImage(ctx, rt, job, input)
}

// runImage runs an image job and returns its first picture's link.
func (s *Storyboards) runImage(ctx context.Context, rt Route, job queueJob, input map[string]any) (string, error) {
	var out imagesOut
	if err := s.queue.run(ctx, job, input, &out); err != nil {
		return "", err
	}
	if len(out.Images) == 0 || out.Images[0].URL == "" {
		return "", &errJobFailed{provider: rt.Provider, status: "completed", detail: "no image in the result"}
	}
	return out.Images[0].URL, nil
}

// presenterSpeaker is the speaker of the ad's presenter scenes in a two-voice script ("" when
// the script names none): speakerTurns gives every presenter scene the same one.
func presenterSpeaker(scenes []ScriptScene) string {
	turns := speakerTurns(scenes)
	for i, s := range scenes {
		if s.Shot == ShotPresenter && turns[i] != "" {
			return turns[i]
		}
	}
	return ""
}

// genderWords already say who the actor is; a description with one is left as written.
var genderWords = []string{"man", "woman", "male", "female", "lady", "guy", "boy", "girl", "mother", "father", "mum", "dad",
	"mwanamume", "mwanamke", "he", "she", "his", "her"}

// Words that say an actor is a man or a woman (English and Swahili).
var (
	maleWords   = []string{"man", "male", "guy", "boy", "father", "dad", "gentleman", "mwanamume", "kijana", "he", "his", "him"}
	femaleWords = []string{"woman", "female", "lady", "girl", "mother", "mum", "mom", "mwanamke", "she", "her"}
)

// ActorGender reads an actor description: SpeakerMale, SpeakerFemale, or "" when it says
// neither or both.
func ActorGender(desc string) string {
	words := " " + strings.Join(strings.FieldsFunc(strings.ToLower(desc), func(r rune) bool {
		return !(r >= 'a' && r <= 'z')
	}), " ") + " "
	has := func(list []string) bool {
		for _, w := range list {
			if strings.Contains(words, " "+w+" ") {
				return true
			}
		}
		return false
	}
	m, f := has(maleWords), has(femaleWords)
	switch {
	case m && !f:
		return SpeakerMale
	case f && !m:
		return SpeakerFemale
	}
	return ""
}

// actorWithGender adds the speaker's gender to an actor description that does not give one.
func actorWithGender(desc, speaker string) string {
	if speaker == "" {
		return desc
	}
	words := " " + strings.Join(strings.FieldsFunc(strings.ToLower(desc), func(r rune) bool {
		return !(r >= 'a' && r <= 'z')
	}), " ") + " "
	for _, w := range genderWords {
		if strings.Contains(words, " "+w+" ") {
			return desc
		}
	}
	if speaker == SpeakerMale {
		return desc + ", a man"
	}
	return desc + ", a woman"
}

// ugcStillPrompt is a generated storyboard still for a UGC ad: a candid phone photo of a real,
// everyday setting rather than a polished advertising shot, so the ad feels like creator content.
func ugcStillPrompt(format, visual string) string {
	return strings.TrimSpace(visual) + ". Candid " + orientation(format) + " phone-camera photo, natural window or daylight, a real everyday East African setting, " +
		"slightly imperfect framing, no studio lighting. No text, no letters, no words, no logos, no watermark, no screens showing writing. Keep the lower third calm for captions."
}

// sceneVisual is what a scene shows: its visual line, else its caption, in its setting.
func sceneVisual(sc ScriptScene) string {
	v := strings.TrimSpace(sc.Visual)
	if v == "" {
		v = strings.TrimSpace(sc.Caption)
	}
	return withSetting(v, sc.Setting)
}

// sceneText is the visual line a clip or presenter route is sent, in the scene's setting.
func sceneText(sc ScriptScene) string { return withSetting(strings.TrimSpace(sc.Visual), sc.Setting) }

// withSetting adds the setting brief's line to a visual, so every generated picture and clip
// happens in the business's real town and kind of place (owner, 2026-11-20).
func withSetting(visual, setting string) string {
	visual, setting = pictureOnly(visual), pictureOnly(setting)
	switch {
	case setting == "":
		return visual
	case visual == "":
		return "Setting: " + setting
	}
	return strings.TrimRight(visual, ". ") + ". Setting: " + setting
}

// presenterSetting is the setting of the ad's first presenter scene ("" when none has one).
func presenterSetting(scenes []ScriptScene) string {
	for _, s := range scenes {
		if s.Shot == ShotPresenter && strings.TrimSpace(s.Setting) != "" {
			return s.Setting
		}
	}
	return ""
}

// soulAspect is Soul v2's aspect_ratio for a format. The 4:5 feed has no Soul ratio; it gets the
// vertical frame, as before, and the start frame is fitted to 4:5 before animation.
func soulAspect(format string) string {
	switch format {
	case "1080x1080":
		return "1:1"
	case "1920x1080":
		return "16:9"
	}
	return "9:16"
}

// seedFor is a stable seed for an actor, recorded with the brief.
func seedFor(versionID, desc string) int64 {
	h := sha256.Sum256([]byte(versionID + "|" + desc))
	return int64(binary.BigEndian.Uint32(h[:4]))
}

// overlayWord marks what only our overlay draws: icons, logos, buttons, contact details, calls to
// action and any writing. An image model asked for them paints a fake WhatsApp icon or garbled
// text into the scene (owner, 2026-11-23), so they never reach a picture or clip prompt.
var overlayWord = regexp.MustCompile(`(?i)\b(icons?|logos?|buttons?|contacts?|contact details|whatsapp|qr|call[- ]to[- ]action|cta|previews?|invoices?|numbers?|placeholders?|contact info|phone numbers?|e-?mails?|websites?|web address|url|links?|signs?|signage|banners?|captions?|text|writing|lettering|headline|price tag|displaying|showing)\b`)

// clauseSplit cuts a prompt into its clauses, keeping the separators.
var clauseSplit = regexp.MustCompile(`[^,;.]+[,;.]?`)

// pictureOnly keeps the clauses of a visual or setting line that describe the picture and drops
// those asking for what the overlay draws.
func pictureOnly(s string) string {
	s = strings.TrimSpace(s)
	if !overlayWord.MatchString(s) {
		return s
	}
	var kept []string
	for _, c := range clauseSplit.FindAllString(s, -1) {
		if t := strings.TrimSpace(c); t != "" && !overlayWord.MatchString(t) {
			kept = append(kept, t)
		}
	}
	out := strings.TrimSpace(strings.Join(kept, " "))
	return strings.TrimRight(strings.TrimLeft(out, ",; "), ",; ")
}

// EndCardPreview is the end card as it will render.
type EndCardPreview struct {
	SceneID string `json:"scene_id"`
	URL     string `json:"url"`
	Key     string `json:"key,omitempty"`
}

// endCardPreview composes the end card overlay (headline, the CTA as words, the contact strip
// and the logo) over the end card scene's still with the same layer the render uses
// (ComposeOverlay and drawOverlay), and stores it. Nothing is generated, so it costs nothing; a
// failure leaves the plan without a preview rather than failing the storyboard.
func (s *Storyboards) endCardPreview(ctx context.Context, req StoryboardRequest, res *StoryboardResult) *EndCardPreview {
	if req.EndCard == nil || res == nil {
		return nil
	}
	var sceneID string
	for _, sc := range req.Scenes {
		if sc.Shot == ShotEndCard {
			sceneID = sc.ID
		}
	}
	var stillURL string
	for _, st := range res.Stills {
		if st.SceneID == sceneID && st.URL != "" {
			stillURL = st.URL
		}
	}
	if sceneID == "" || stillURL == "" {
		return nil
	}
	p, err := s.ComposeEndCardPreview(ctx, EndCardPreviewRequest{TenantID: req.TenantID, VersionID: req.VersionID, Format: req.Format,
		SceneID: sceneID, StillURL: stillURL, EndCard: *req.EndCard, LogoURL: req.LogoURL})
	if err != nil {
		s.logger.Info("end card preview skipped", zap.Error(err))
		return nil
	}
	return p
}

// EndCardPreviewRequest recomposes an end card preview over a still already drawn.
type EndCardPreviewRequest struct {
	TenantID  uuid.UUID
	VersionID string
	Format    string
	SceneID   string
	StillURL  string
	EndCard   Overlay
	LogoURL   string
	// Name is the stored file's suffix; "" is the storyboard's own preview name.
	Name string
}

// ComposeEndCardPreview lays the end card overlay over its approved still and stores the result.
// It is the recompose-only path too (owner, 2026-11-24): new words, contacts or a new logo plate
// reach the preview without redrawing any still, so it never costs a credit. A missing or broken
// logo is left out quietly.
func (s *Storyboards) ComposeEndCardPreview(ctx context.Context, req EndCardPreviewRequest) (*EndCardPreview, error) {
	if s == nil || s.posts == nil || s.store == nil {
		return nil, fmt.Errorf("media: end card previews are not available")
	}
	scene, err := s.posts.download(ctx, req.StillURL)
	if err != nil {
		return nil, fmt.Errorf("media: the end card still could not be read: %w", err)
	}
	o := req.EndCard
	if req.LogoURL != "" && o.Logo == nil {
		if logo, err := s.posts.download(ctx, req.LogoURL); err == nil {
			o.Logo = logo
		}
	}
	// The same layer the render lays over the end card (ComposeOverlay), over the still fitted
	// to the format as the assembly fits it, so the preview matches the rendered end card.
	layer, err := ComposeOverlay(req.Format, o)
	if err != nil {
		return nil, err
	}
	img := FitFrame(scene, layer.Bounds().Size())
	draw.Draw(img, img.Bounds(), layer, image.Point{}, draw.Over)
	data, err := EncodeJPEG(img)
	if err != nil {
		return nil, err
	}
	name := req.VersionID + "-endcard-preview"
	if req.Name != "" {
		name += "-" + req.Name
	}
	key, url, err := s.store.PutPoster(ctx, req.TenantID, name, data)
	if err != nil {
		return nil, fmt.Errorf("media: end card preview not stored: %w", err)
	}
	return &EndCardPreview{SceneID: req.SceneID, URL: url, Key: key}, nil
}

// previousStill finds the previous storyboard's generated still for a scene drawn from exactly
// this prompt. A previous still sent without its prompt is one marketflow-api compared itself
// (the scene's shot, visual and setting are unchanged, as after a settings re-set) and is kept.
func previousStill(prev []Still, sceneID, prompt string) (Still, bool) {
	for _, p := range prev {
		if p.SceneID == sceneID && p.Source == StillGenerated && p.URL != "" && (p.Prompt == "" || p.Prompt == prompt) {
			return p, true
		}
	}
	return Still{}, false
}
