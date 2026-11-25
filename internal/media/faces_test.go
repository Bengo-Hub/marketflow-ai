package media

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A consented face is sent to the identity route only, without a name, and the actor carries its
// id. With the route off, or failing, the actor is synthetic and carries none.
func TestActorFromFace(t *testing.T) {
	scenes := []ScriptScene{{ID: "s1", Shot: ShotPresenter, Visual: "a presenter", Voiceover: "Karibu."}}
	for _, name := range []string{"on", "off", "failing"} {
		f := newFakeProviders(t)
		store := newServingStore(t)
		s := NewStoryboards(keysFor("fal", "higgsfield"), store, NewPosters(keysFor("fal", "higgsfield"), store, nil), nil)
		switch name {
		case "off":
			SetPolicySource(func(context.Context) RoutePolicy { return PolicyFromSettings(map[string]string{"identity_routes": "[]"}) })
		case "failing":
			f.failFalPath = "nano-banana-pro"
		}
		res, err := s.Make(context.Background(), StoryboardRequest{TenantID: [16]byte{1}, VersionID: "v1", Format: "1920x1080", Kind: AdPresenter,
			Scenes: scenes, ActorPrompt: "a shop owner", Face: &Face{ID: "face-1", URL: "https://x/owner.jpg?sig=1"}})
		draws := StoryboardRequest{Kind: AdPresenter, Face: &Face{ID: "face-1", URL: "https://x/a.jpg"}}.DrawsFace(context.Background())
		if draws == (name == "off") {
			t.Fatalf("%s: draws %v", name, draws)
		}
		SetPolicySource(nil)
		if err != nil {
			t.Fatal(name, err)
		}
		sent, used := f.inputs["fal-ai/nano-banana-pro/edit"]
		for path, in := range f.inputs {
			if path != "fal-ai/nano-banana-pro/edit" && strings.Contains(fmt.Sprint(in), "owner.jpg") {
				t.Fatalf("%s: the face went to %s", name, path)
			}
		}
		// Only a drawn face is charged.
		if res.FaceDrawn != (name == "on") {
			t.Fatalf("%s: face drawn %v", name, res.FaceDrawn)
		}
		switch name {
		case "on":
			if !used || fmt.Sprint(sent["image_urls"]) != "[https://x/owner.jpg?sig=1]" || !strings.Contains(fmt.Sprint(sent["prompt"]), "same adult") {
				t.Fatalf("identity input %v", sent)
			}
			if res.Actor.FaceID != "face-1" || res.Actor.Route != "fal/nano-banana-pro" || res.Actor.Prompt == "" {
				t.Fatalf("actor %+v", res.Actor)
			}
		default:
			if (name == "off" && used) || res.Actor.FaceID != "" || res.Actor.URL == "" || res.Actor.Route == "fal/nano-banana-pro" {
				t.Fatalf("%s: actor %+v", name, res.Actor)
			}
		}
	}
}

func TestIdentityRoutesSetting(t *testing.T) {
	if p := PolicyFromSettings(nil); len(p.IdentityRoutes) != 1 {
		t.Fatalf("default %v", p.IdentityRoutes)
	}
	if p := PolicyFromSettings(map[string]string{"identity_routes": "oops"}); len(p.IdentityRoutes) != 1 {
		t.Fatalf("unreadable %v", p.IdentityRoutes)
	}
}
