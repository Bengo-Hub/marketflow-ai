package media

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func cueRules() *CueRules {
	return &CueRules{Enabled: true, MaxScenes: 1, Cues: []LocalCue{{Match: []string{"counter"}, Object: "a small plain card on the counter", Text: "LIPA HAPA"}},
		Marks: map[string]string{"m-pesa": "mobile money", "lipa na m-pesa": "mobile money"}}
}

var counterScene = []ScriptScene{{ID: "s1", Shot: ShotBRoll, Visual: "a customer pays with a phone at the counter"}}

// Only short words without digits, on a matching scene, once.
func TestPlanCues(t *testing.T) {
	free := func(string) bool { return true }
	scenes := append(counterScene[:1:1], ScriptScene{ID: "s2", Shot: ShotBRoll, Visual: "another counter"})
	if got := planCues(scenes, *cueRules(), free); len(got) != 1 || got["s1"].Text != "LIPA HAPA" {
		t.Fatalf("cues %v", got)
	}
	for _, bad := range []string{"TILL 123456", "", "A SIGN WITH FAR TOO MANY WORDS", "KES 500"} {
		r := *cueRules()
		r.Cues = []LocalCue{{Match: []string{"counter"}, Object: "a card", Text: bad}}
		if got := planCues(scenes, r, free); len(got) != 0 {
			t.Fatalf("%q was drawn", bad)
		}
	}
	if got := planCues(scenes, *cueRules(), func(string) bool { return false }); len(got) != 0 {
		t.Fatal("a scene carrying something else got a cue")
	}
}

// A cue is drawn only on a listed text route and kept only when the review reads it; every other
// still keeps the ban on writing.
func TestStoryboardCue(t *testing.T) {
	for _, c := range []struct {
		name, tier, routes string
		pass, answers      bool
		kept               bool
	}{
		{"kept", TierPremium, "", true, true, true},
		{"misspelt", TierPremium, "", false, true, false},
		{"reviewer silent", TierPremium, "", true, false, false},
		{"standard tier has no text route", TierStandard, "", true, true, false},
		{"switched off", TierPremium, "[]", true, true, false},
	} {
		SetReviewer(NewReviewer(func(context.Context, string, string, []string) (string, string, float64, error) {
			switch {
			case !c.answers:
				return "", "", 0, errors.New("429")
			case !c.pass:
				return `{"pass":false,"issues":[{"frame":"s1","kind":"text_artefact","severity":"high","note":"LIPA HPAA"}]}`, "fake", 0, nil
			}
			return `{"pass":true,"issues":[]}`, "fake", 0, nil
		}, nil))
		SetPolicySource(func(context.Context) RoutePolicy { return PolicyFromSettings(map[string]string{"text_routes": c.routes}) })
		f := newFakeProviders(t)
		store := newServingStore(t)
		s := NewStoryboards(keysFor("fal", "higgsfield"), store, NewPosters(keysFor("fal", "higgsfield"), store, nil), nil)
		res, err := s.Make(context.Background(), StoryboardRequest{TenantID: [16]byte{1}, VersionID: "v1", Format: "1920x1080", Kind: AdTemplate, Tier: c.tier,
			Scenes: counterScene, Cues: cueRules()})
		SetReviewer(nil)
		SetPolicySource(nil)
		if err != nil {
			t.Fatal(c.name, err)
		}
		st := res.Stills[0]
		for path, in := range f.inputs {
			if path != "fal-ai/nano-banana-pro" && strings.Contains(fmt.Sprint(in["prompt"]), "LIPA HAPA") {
				t.Fatalf("%s: the cue went to %s", c.name, path)
			}
		}
		if (st.Cue == "LIPA HAPA") != c.kept || strings.Contains(st.Prompt, "LIPA HAPA") != c.kept || strings.Contains(st.Prompt, "no text, no letters") == c.kept {
			t.Fatalf("%s: still %+v", c.name, st)
		}
		if c.kept && st.Route != "fal/nano-banana-pro" {
			t.Fatalf("route %s", st.Route)
		}
	}
}

// Trade marks leave a drawn scene's prompt but not its words, and a scene using the tenant's own
// picture is left alone.
func TestNeutralScenes(t *testing.T) {
	scenes := []ScriptScene{
		{ID: "s1", Shot: ShotBRoll, Visual: "a Lipa na M-Pesa sign on the desk", Setting: "beside an M-PESA stand", Voiceover: "Pay by M-Pesa."},
		{ID: "s2", Shot: ShotBRoll, Visual: "our mpesa counter sign, M-Pesa"},
		{ID: "s3", Shot: ShotPresenter, Setting: "an M-Pesa shop"},
	}
	photos, info := []string{"https://x/sign.jpg"}, []PhotoInfo{{Library: true, Caption: "our mpesa counter sign"}}
	got := NeutralScenes(scenes, cueRules(), photos, nil, info)
	if got[0].Visual != "a mobile money sign on the desk" || got[0].Setting != "beside an mobile money stand" || got[0].Voiceover != "Pay by M-Pesa." {
		t.Fatalf("drawn scene %+v", got[0])
	}
	if got[1].Visual != scenes[1].Visual || got[2].Setting != "an mobile money shop" || scenes[0].Visual == got[0].Visual {
		t.Fatalf("scenes %+v", got)
	}
}

func TestCueSceneIsNotAnimated(t *testing.T) {
	plan, err := (&Ads{}).Plan(context.Background(), AdRequest{Kind: AdPresenter, Format: "1920x1080", Scenes: counterScene,
		Stills: map[string]string{"s1": "https://x/1.jpg"}, CueWords: map[string]string{"s1": "LIPA HAPA"}})
	if err != nil || plan.Scenes[0].Work != WorkStill {
		t.Fatalf("plan %+v err %v", plan, err)
	}
}
