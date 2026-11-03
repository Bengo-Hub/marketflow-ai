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
//   - a product scene shows the owner's own photo (the product is never generated);
//   - a b-roll scene gets a generated still, which the clip route then animates;
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
	// Actor is the brief's existing actor: kept, never re-drawn, so every version of the
	// brief has the same presenter. Empty makes one from ActorPrompt.
	Actor       *Actor
	ActorPrompt string
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
}

// Still is one scene's storyboard frame.
type Still struct {
	SceneID string  `json:"scene_id"`
	URL     string  `json:"url,omitempty"`
	Key     string  `json:"key,omitempty"`
	Source  string  `json:"source"`
	Route   string  `json:"route,omitempty"`
	CostUSD float64 `json:"cost_usd,omitempty"`
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
}

// NewStoryboards builds the storyboard pipeline; stills are stored like posters.
func NewStoryboards(keys KeySource, store Store, posters *Posters, logger *zap.Logger) *Storyboards {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Storyboards{router: NewRouter(StillRoutes), keys: keys, store: store, posts: posters, queue: newQueueClient(),
		logger: logger.Named("media.storyboards")}
}

// Ready reports whether storyboards can be made here.
func (s *Storyboards) Ready() bool { return s != nil && s.store != nil && s.posts != nil }

// Plan lists what each scene's still will be, without generating anything. Generated
// counts the stills that cost money, for the quote.
func (s *Storyboards) Plan(req StoryboardRequest) (stills []Still, generated int) {
	photo := 0
	for _, sc := range req.Scenes {
		st := Still{SceneID: sc.ID}
		switch {
		case sc.Shot == ShotPresenter:
			st.Source = StillActor
		case sc.Shot == ShotFootage:
			st.Source = StillFootage
		case sc.Shot == ShotBRoll && isPresenterKind(req.Kind):
			st.Source = StillGenerated
			generated++
		case len(req.Photos) > 0:
			st.Source, st.URL = StillPhoto, pick(req.Photos, sc.Asset, &photo)
		default:
			st.Source = StillGenerated
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
		return nil, &UserError{Msg: "format must be 1080x1080, 1080x1350 or 1080x1920"}
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
			prompt := "Vertical selfie-style photo of " + desc + ", upper body, looking straight at the camera with a relaxed smile, mouth closed, soft natural daylight, plain uncluttered background. Real photo, no text, no letters, no logos."
			url, key, route, cost, err := s.still(ctx, req, "actor", prompt)
			if err != nil {
				return nil, fmt.Errorf("media: actor still: %w", err)
			}
			res.Actor = &Actor{URL: url, Key: key, Prompt: desc, Seed: seedFor(req.VersionID, desc), Route: route}
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
		visual := strings.TrimSpace(req.Scenes[i].Visual)
		if visual == "" {
			visual = strings.TrimSpace(req.Scenes[i].Caption)
		}
		prompt := scenePrompt(visual)
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
	routes := s.router.Order(CapStill, TierStandard, func(rt Route) bool { return usable(ctx, s.keys, rt, rt.Cost(0)) })
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
		ar := "9:16"
		if format == "1080x1080" {
			ar = "1:1"
		}
		job, input = higgsfieldJob(key, "higgsfield-ai/soul/v2/standard"), map[string]any{"prompt": prompt, "aspect_ratio": ar}
	case "fal/seedream-v5-lite":
		job = falJob(key, "fal-ai/bytedance/seedream/v5/lite/text-to-image")
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

// seedFor is a stable seed for an actor, recorded with the brief.
func seedFor(versionID, desc string) int64 {
	h := sha256.Sum256([]byte(versionID + "|" + desc))
	return int64(binary.BigEndian.Uint32(h[:4]))
}
