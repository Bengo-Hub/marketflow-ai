package media

import (
	"context"
	"fmt"
	"image"
	"strings"
	"time"

	"go.uber.org/zap"
)

// The self-review in each pipeline (review.go): posters, storyboard stills and finished videos.

// minRetryTime is the time a redraw needs; with less left the review only reports.
const minRetryTime = 25 * time.Second

func timeFor(ctx context.Context, need time.Duration) bool {
	dl, ok := ctx.Deadline()
	return !ok || time.Until(dl) > need
}

// overlayWords are the exact words an overlay draws, for the reviewer.
func overlayWords(o Overlay) string {
	parts := []string{o.Headline, o.Subline, o.Price, o.CTA}
	for _, c := range CleanContacts(o.Contacts) {
		parts = append(parts, c.Value)
	}
	return strings.Join(nonEmpty(parts), " | ")
}

func nonEmpty(list []string) []string {
	var out []string
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// allHigh lists every high issue of a verdict.
func allHigh(v ReviewVerdict) []ReviewIssue {
	var out []ReviewIssue
	for _, list := range highIssues(v) {
		out = append(out, list...)
	}
	return out
}

// relayout changes an overlay for a layout retry: the logo to the other corner when the logo was
// the problem, a stronger band when the words were hard to read.
func relayout(o *Overlay, scene image.Image, format string, issues []ReviewIssue) {
	for _, is := range issues {
		switch is.Kind {
		case IssueLogo:
			if o.LogoCorner == "" {
				o.LogoCorner = AutoLogoCorner(scene, format, *o)
			}
			o.LogoCorner = otherCorner(o.LogoCorner)
		case IssueLegibility:
			o.Strong = true
		}
	}
}

// reviewPoster reviews a composed poster and redoes what fails: the layout for free, the scene on
// the same route while the retry budget allows. It returns the scene and request to finish with.
func (p *Posters) reviewPoster(ctx context.Context, rt Route, req PosterRequest, scene image.Image, sceneURL string) (image.Image, string, PosterRequest, *ReviewReport, float64) {
	r, s, on := reviewOn(ctx)
	if !on {
		return scene, sceneURL, req, nil, 0
	}
	rep := &ReviewReport{}
	extra := 0.0
	brief := ReviewBrief{Kind: "poster", Brand: req.Brand, Place: req.Place, Words: overlayWords(req.Overlay)}
	for round := 0; ; round++ {
		img, err := ComposePoster(scene, req.Format, req.Overlay)
		if err != nil {
			break
		}
		v, err := r.Review(ctx, brief, []ReviewFrame{{Label: "poster", Image: img, Expect: req.Prompt}})
		rep.Rounds++
		if err != nil {
			rep.Skipped = "the reviewer did not answer"
			p.logger.Info("poster review skipped", zap.Error(err))
			break
		}
		rep.Model, rep.CostUSD, rep.Issues, rep.Passed = v.Model, rep.CostUSD+v.CostUSD, v.Issues, v.Pass
		high := allHigh(v)
		if v.Pass || round >= s.MaxRetries || len(high) == 0 {
			break
		}
		if layoutOnly(high) {
			relayout(&req.Overlay, scene, req.Format, high)
			rep.Fixed = append(rep.Fixed, "poster: logo and words laid out again")
			continue
		}
		if extra+rt.PriceUSD > s.PosterRetryUSD+1e-9 || !timeFor(ctx, minRetryTime) {
			rep.Skipped = "the retry budget or time is used up"
			break
		}
		actx, cancel := context.WithTimeout(ctx, rt.Timeout)
		u, err := p.scene(actx, rt, req)
		var next image.Image
		if err == nil {
			next, err = p.download(actx, u)
		}
		cancel()
		if err != nil {
			rep.Skipped = "the scene could not be drawn again"
			break
		}
		extra += rt.PriceUSD
		scene, sceneURL = next, u
		relayout(&req.Overlay, scene, req.Format, high)
		rep.Fixed = append(rep.Fixed, "poster: scene drawn again")
	}
	rep.CostUSD += extra
	return scene, sceneURL, req, rep, extra
}

// reviewStills reviews the storyboard's generated stills and the actor, and redraws the failing
// ones while the retry budget allows. Photo frames are the business's own and are not redrawn.
func (s *Storyboards) reviewStills(ctx context.Context, req StoryboardRequest, res *StoryboardResult) {
	r, set, on := reviewOn(ctx)
	if !on {
		return
	}
	rep := &ReviewReport{}
	res.Review = rep
	spent := 0.0
	brief := ReviewBrief{Kind: "storyboard", Brand: req.Brand, Place: req.Place}
	for round := 0; ; round++ {
		var frames []ReviewFrame
		if res.Actor != nil && res.Actor.URL != "" && res.Actor.Route != "" {
			if img, err := s.posts.download(ctx, res.Actor.URL); err == nil {
				frames = append(frames, ReviewFrame{Label: "actor", Image: img, Expect: res.Actor.Prompt})
			}
		}
		for i, st := range res.Stills {
			if st.Source != StillGenerated || st.Reused || st.URL == "" || len(frames) >= set.VideoFrames {
				continue
			}
			if img, err := s.posts.download(ctx, st.URL); err == nil {
				frames = append(frames, ReviewFrame{Label: st.SceneID, Image: img, Expect: reviewExpect(req.Scenes[sceneIndex(req.Scenes, st.SceneID, i)])})
			}
		}
		if len(frames) == 0 {
			return
		}
		v, err := r.Review(ctx, brief, frames)
		rep.Rounds++
		if err != nil {
			rep.Skipped = "the reviewer did not answer"
			s.logger.Info("storyboard review skipped", zap.Error(err))
			break
		}
		rep.Model, rep.CostUSD, rep.Issues, rep.Passed = v.Model, rep.CostUSD+v.CostUSD, v.Issues, v.Pass
		failing := highIssues(v)
		if v.Pass || round >= set.MaxRetries || len(failing) == 0 || !timeFor(ctx, minRetryTime) {
			break
		}
		redrawn := 0
		for i := range res.Stills {
			st := &res.Stills[i]
			if st.Source != StillGenerated || st.Reused || len(failing[st.SceneID]) == 0 {
				continue
			}
			if spent+st.CostUSD > set.StillRetryUSD+1e-9 {
				rep.Skipped = "the retry budget is used up"
				continue
			}
			url, key, route, cost, err := s.still(ctx, req, fmt.Sprintf("sb-%s-r%d", st.SceneID, round+1), st.Prompt)
			if err != nil {
				continue
			}
			st.URL, st.Key, st.Route = url, key, route
			st.CostUSD += cost
			spent += cost
			res.CostUSD += cost
			redrawn++
			rep.Fixed = append(rep.Fixed, "still "+st.SceneID+" drawn again")
		}
		if res.Actor != nil && len(failing["actor"]) > 0 && res.Actor.Route != "" {
			prompt := ActorPromptIn(req.Format, res.Actor.Prompt, presenterSetting(req.Scenes))
			if url, key, route, cost, err := s.still(ctx, req, fmt.Sprintf("actor-r%d", round+1), prompt); err == nil && spent+cost <= set.StillRetryUSD+1e-9 {
				res.Actor.URL, res.Actor.Key, res.Actor.Route = url, key, route
				spent += cost
				res.CostUSD += cost
				redrawn++
				rep.Fixed = append(rep.Fixed, "actor drawn again")
				for i := range res.Stills {
					if res.Stills[i].Source == StillActor {
						res.Stills[i].URL = url
					}
				}
			}
		}
		if redrawn == 0 {
			break
		}
	}
	rep.CostUSD += spent
}

// sceneIndex finds a scene by id, else falls back to i.
func sceneIndex(scenes []ScriptScene, id string, i int) int {
	for k, sc := range scenes {
		if sc.ID == id {
			return k
		}
	}
	return min(i, len(scenes)-1)
}

// reviewVideo looks at one frame per scene and at the end card of a finished ad. It returns the
// high issues by frame label ("end_card" or a scene id) and the report; it never fails the ad.
func (a *Ads) reviewVideo(ctx context.Context, req AdRequest, res *MediaResult, outs []sceneOut) (map[string][]ReviewIssue, *ReviewReport, bool) {
	r, set, on := reviewOn(ctx)
	if !on || res == nil || res.URL == "" {
		return nil, nil, false
	}
	rep := &ReviewReport{}
	var frames []ReviewFrame
	t := 0.0
	for i, s := range req.Scenes {
		mid := t + outs[i].seconds/2
		t += outs[i].seconds
		if s.Shot == ShotFootage || len(frames) >= set.VideoFrames-1 {
			continue
		}
		if img, err := a.asm.frameAt(ctx, res.URL, mid); err == nil {
			frames = append(frames, ReviewFrame{Label: s.ID, Image: img, Expect: reviewExpect(s)})
		}
	}
	if img, err := a.asm.frameAt(ctx, res.URL, max(0, t-1)); err == nil {
		frames = append(frames, ReviewFrame{Label: "end_card", Image: img, Expect: "the end card: the logo, the closing words and the contact details"})
	}
	v, err := r.Review(ctx, ReviewBrief{Kind: "video", Brand: req.Brand, Place: req.Place, Words: overlayWords(req.EndCard)}, frames)
	rep.Rounds = 1
	if err != nil {
		rep.Skipped = "the reviewer did not answer"
		a.logger.Info("video review skipped", zap.Error(err))
		return nil, rep, false
	}
	rep.Model, rep.CostUSD, rep.Issues, rep.Passed = v.Model, v.CostUSD, v.Issues, v.Pass
	return highIssues(v), rep, set.MaxRetries > 0
}

// reviewAndRedo reviews a finished ad and redoes what fails, in place: the end card's layout, and
// clip scenes whose new clip keeps the ad under the per-video ceiling. scenes is updated with any
// new clip; cost gains what the new clips cost. It returns the report (nil when no review ran).
func (a *Ads) reviewAndRedo(ctx context.Context, req AdRequest, plan AdPlan, outs []sceneOut, scenes []AdScene,
	card *Overlay, cost *float64, extra float64, dir string, res *MediaResult) *ReviewReport {
	issues, rep, retry := a.reviewVideo(ctx, req, res, outs)
	if rep == nil || !retry || len(issues) == 0 {
		return rep
	}
	if end := issues["end_card"]; len(end) > 0 && layoutOnly(end) {
		relayout(card, nil, req.Format, end)
		rep.Fixed = append(rep.Fixed, "end card laid out again")
	}
	ceiling := req.MaxVideoUSD
	if ceiling <= 0 {
		ceiling = Policy(ctx).MaxVideoUSD
	}
	for i, s := range req.Scenes {
		list := issues[s.ID]
		if len(list) == 0 || layoutOnly(list) || plan.Scenes[i].Work != WorkClip || outs[i].cache.Reused || !timeFor(ctx, 3*time.Minute) {
			continue
		}
		price := plan.Scenes[i].CostUSD
		if ceiling > 0 && *cost+extra+price > ceiling+1e-9 {
			rep.Skipped = "a new clip would take the ad over its cost ceiling"
			continue
		}
		if plain := req.PlainStills[s.ID]; plain != "" && plain != plan.Scenes[i].source {
			// The placed logo did not survive the motion: the plain still makes the new clip.
			plan.Scenes[i].source = plain
		}
		before := outs[i].cache
		if err := a.visualScene(ctx, req, plan.Scenes[i], s, dir, &outs[i]); err != nil {
			outs[i].cache = before
			a.logger.Info("clip not made again after the review", zap.String("scene", s.ID), zap.Error(err))
			continue
		}
		*cost += outs[i].cache.CostUSD - before.CostUSD
		scenes[i].VideoURL = outs[i].cache.VideoURL
		rep.Fixed = append(rep.Fixed, "clip "+s.ID+" made again")
	}
	return rep
}

// reviewExpect is what the reviewer compares a scene with. A b-roll scene is marked as read by a
// voice-over, so a face talking to the camera in it is reported; a presenter scene and the owner's
// own photos are not.
func reviewExpect(s ScriptScene) string {
	switch s.Shot {
	case ShotBRoll:
		return "voice-over scene: " + sceneVisual(s)
	case ShotPresenter:
		return "presenter talking to the camera: " + sceneVisual(s)
	}
	return sceneVisual(s)
}
