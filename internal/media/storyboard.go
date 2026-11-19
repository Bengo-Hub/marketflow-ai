package media

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
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
}

// StoryboardResult is the approved-to-be storyboard.
type StoryboardResult struct {
	Actor   *Actor  `json:"actor,omitempty"`
	Stills  []Still `json:"stills"`
	CostUSD float64 `json:"cost_usd"`
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
	return &Storyboards{router: NewRouter(StillRoutes), keys: keys, store: store, posts: posters, queue: newQueueClient(),
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
			st.Prompt = StillPrompt(req.Format, req.Delivery, sceneVisual(sc))
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
			url, key, route, cost, err := s.still(ctx, req, "actor", ActorPrompt(req.Format, desc))
			if err != nil {
				return nil, fmt.Errorf("media: actor still: %w", err)
			}
			res.Actor = &Actor{URL: url, Key: key, Prompt: desc, Seed: seedFor(req.VersionID, desc), Route: route,
				Gender: ActorGender(desc)}
			res.CostUSD += cost
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
		default:
			continue
		}
		prompt := st.Prompt
		wg.Add(1)
		go func(st *Still, prompt string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			url, key, route, cost, err := s.still(ctx, req, "sb-"+st.SceneID, prompt)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("scene %s: %w", st.SceneID, err))
				return
			}
			st.URL, st.Key, st.Route, st.CostUSD = url, key, route, cost
			res.CostUSD += cost
		}(st, prompt)
	}
	wg.Wait()
	if len(errs) > 0 {
		return nil, errs[0]
	}
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
	var lastErr error
	for _, rt := range routes {
		if ctx.Err() != nil {
			break
		}
		actx, cancel := context.WithTimeout(ctx, rt.Timeout)
		src, rerr := s.generate(actx, rt, req.Format, prompt)
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
	default:
		return "", fmt.Errorf("media: no builder for route %s", rt.Key)
	}
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

// sceneVisual is what a scene shows: its visual line, else its caption.
func sceneVisual(sc ScriptScene) string {
	if v := strings.TrimSpace(sc.Visual); v != "" {
		return v
	}
	return strings.TrimSpace(sc.Caption)
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
