package media

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Brand faces (owner approved 2026-11-24, plan item 66 f): the presenter's actor may take its look
// from a person the tenant recorded a consent for. marketflow-api holds the consents and sends a
// face only while one is live, for that tenant only; this package draws the actor from it on the
// routes the platform lists (RoutePolicy.IdentityRoutes). When no such route is on, or none
// answers, the actor is drawn as a presenter not modelled on anyone and carries no face id. The
// person's name is never sent to a model.

// CapIdentity is an actor still drawn from a reference picture of a person.
const CapIdentity Capability = "identity"

// IdentityRoutes are the reference-image routes; the edit endpoint the poster pipeline already uses.
var IdentityRoutes = []Route{
	{Key: "fal/nano-banana-pro", Provider: "fal", Capability: CapIdentity, PriceUSD: 0.15, Quality: 5, PAYG: true, Timeout: 90 * time.Second},
}

// Face is a consented person's picture.
type Face struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// facePrompt asks for the same person in the presenter's framing, as they really look.
func facePrompt(format, setting string) string {
	return ActorPromptIn(format, "the same adult as in the reference photo, in smart casual clothes", setting) +
		" Keep this person's own face, skin tone, age and hair exactly as in the reference photo, not beautified. Only this one person."
}

// actorStill draws the presenter's actor: from the consented face when there is one and a route for
// it, else from the description. faceID says which it was.
func (s *Storyboards) actorStill(ctx context.Context, req StoryboardRequest, name, desc, setting, face string) (url, key, route string, cost float64, faceID string, err error) {
	if req.Face != nil && face == req.Face.ID && face != "" && httpLink(req.Face.URL) {
		allowed := Policy(ctx).IdentityRoutes
		routes := s.router.OrderPreferred(CapIdentity, TierStandard, 0, func(rt Route) bool {
			return slices.Contains(allowed, rt.Key) && usable(ctx, s.keys, rt, rt.Cost(0))
		}, allowed)
		url, key, route, cost, err = s.draw(ctx, req, name, routes, func(ctx context.Context, rt Route) (string, error) {
			return s.fromFace(ctx, rt, req.Format, facePrompt(req.Format, setting), req.Face.URL)
		})
		if err == nil {
			return url, key, route, cost, face, nil
		}
		s.logger.Info("brand face not drawn; the presenter is not modelled on anyone", zap.Error(err))
	}
	url, key, route, cost, err = s.still(ctx, req, name, ActorPromptIn(req.Format, desc, setting))
	return url, key, route, cost, "", err
}

// actorExpect is what the reviewer compares the actor with. The reviewer is not told who a
// consented person is.
func actorExpect(a *Actor) string {
	if a.FaceID != "" {
		return "one adult presenter facing the camera, natural and real-looking"
	}
	return a.Prompt
}

// fromFace runs one identity route (the request shape the poster pipeline sends it).
func (s *Storyboards) fromFace(ctx context.Context, rt Route, format, prompt, picture string) (string, error) {
	if rt.Key != "fal/nano-banana-pro" {
		return "", fmt.Errorf("media: no identity builder for route %s", rt.Key)
	}
	return s.runImage(ctx, rt, falJob(s.keys(ctx, rt.Provider), s.endpoints.NanoBananaProEdit), map[string]any{"prompt": strings.TrimSpace(prompt),
		"aspect_ratio": aspectFor(format), "resolution": "2K", "output_format": "jpeg", "num_images": 1, "image_urls": []string{picture}})
}
