package team

import (
	"context"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/library"
	"github.com/bengobox/marketflow-ai/internal/promptopt"
	"github.com/google/uuid"
)

func speakersOf(v VideoScript) []string {
	out := make([]string, len(v.Scenes))
	for i, s := range v.Scenes {
		out[i] = s.Speaker
	}
	return out
}

func TestAssignSpeakers(t *testing.T) {
	sc := func(shot, voice, speaker string) Scene {
		return Scene{Shot: shot, Voiceover: voice, Speaker: speaker}
	}
	cases := []struct {
		name     string
		scenes   []Scene
		twoVoice bool
		want     []string
	}{
		{"the writer's turns are kept", []Scene{
			sc("b_roll", "Is a POS too expensive for a duka?", "female"),
			sc("product_clip", "Not this one. KES 1,500 a month.", "male"),
			sc("end_card", "WhatsApp DEMO to try it.", "female")}, true,
			[]string{"female", "male", "female"}},
		{"blank lines take the next turn", []Scene{
			sc("b_roll", "Question first.", "male"),
			sc("product_clip", "The answer.", ""),
			sc("product_clip", "One more point.", ""),
			sc("end_card", "Order now on WhatsApp.", "")}, true,
			[]string{"male", "female", "male", "female"}},
		{"lines before the first named speaker lead into its turn", []Scene{
			sc("b_roll", "Opening line.", ""),
			sc("product_clip", "Second line.", "female")}, true,
			[]string{"male", "female"}},
		{"a silent scene has no speaker and does not take a turn", []Scene{
			sc("b_roll", "Hook line.", "female"),
			sc("product_clip", "", "male"),
			sc("product_clip", "[curious]", "female"),
			sc("end_card", "Call to action.", "")}, true,
			[]string{"female", "", "", "male"}},
		{"every presenter scene uses the presenter's speaker", []Scene{
			sc("presenter", "Hi, quick tip.", "female"),
			sc("b_roll", "Here's the thing most people miss.", "male"),
			sc("presenter", "So try it this way.", "male"),
			sc("end_card", "Book on WhatsApp.", "")}, true,
			[]string{"female", "male", "female", "male"}},
		{"the writer kept one voice", []Scene{
			sc("b_roll", "One.", ""), sc("end_card", "Two.", "")}, true,
			[]string{"", ""}},
		{"one speaker throughout is a one-voice ad", []Scene{
			sc("presenter", "Line one.", "female"), sc("presenter", "Line two.", "female")}, true,
			[]string{"", ""}},
		{"other video types keep one voice", []Scene{
			sc("b_roll", "A.", "female"), sc("end_card", "B.", "male")}, false,
			[]string{"", ""}},
		{"loose labels are read", []Scene{
			sc("b_roll", "A.", "Woman"), sc("end_card", "B.", "M")}, true,
			[]string{"female", "male"}},
	}
	for _, tc := range cases {
		v := VideoScript{Scenes: tc.scenes}
		AssignSpeakers(&v, tc.twoVoice, "")
		got := speakersOf(v)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: speakers = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A saved actor decides the presenter's voice (owner, 2026-11-14: the script follows the actor).
func TestAssignSpeakersFollowsTheActor(t *testing.T) {
	v := VideoScript{Scenes: []Scene{
		{Shot: "presenter", Voiceover: "Hi, quick tip.", Speaker: "female"},
		{Shot: "b_roll", Voiceover: "Here's the thing most people miss.", Speaker: "female"},
		{Shot: "presenter", Voiceover: "So try it this way.", Speaker: "female"},
		{Shot: "end_card", Voiceover: "Book on WhatsApp."},
	}}
	AssignSpeakers(&v, true, "male")
	if got := strings.Join(speakersOf(v), ","); got != "male,female,male,female" {
		t.Fatalf("speakers = %s", got)
	}
}

func TestCleanVoiceTags(t *testing.T) {
	lib := library.MustLoad()
	cases := map[string]string{
		"[excited] It is here.":            "[excited] It is here.",
		"[Curious] Have you tried this?":   "[curious] Have you tried this?",
		"[shouting] Buy it.":               "Buy it.",
		"Buy it [laughs softly] today.":    "Buy it today.",
		"[warm] Hello [excited] again.":    "[warm] Hello again.",
		"Plain line.":                      "Plain line.",
		"[warm]":                           "",
		"[unclosed tag and words":          "unclosed tag and words",
	}
	for in, want := range cases {
		if got := CleanVoiceTags(in, lib.AllowedVoiceTag); got != want {
			t.Errorf("CleanVoiceTags(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQualityFindings(t *testing.T) {
	q := library.MustLoad().Quality
	good := VideoScript{CTA: "WhatsApp 'LAMP' to 0712 000 000 for delivery today", Scenes: []Scene{
		{ID: "s1", DurationS: 2.5, Voiceover: "Lights out again?", Caption: "Blackout?"},
		{ID: "s2", DurationS: 4, Voiceover: "This lamp runs 12 hours on one charge.", Caption: "12 hours"},
		{ID: "s3", DurationS: 3, Shot: "end_card", Voiceover: "WhatsApp LAMP for delivery today.", Caption: "KES 1,200"}}}
	if f := QualityFindings(good, q); len(f) != 0 {
		t.Fatalf("a good script got findings: %+v", f)
	}
	bad := VideoScript{CTA: "Learn more", Scenes: []Scene{
		{ID: "s1", DurationS: 5, Voiceover: "In today's busy world many families across the country struggle with power cuts every week.", Caption: ""},
		{ID: "s2", DurationS: 9, Shot: "product_clip", Voiceover: "Our lamp.", Caption: "This caption is far too long to fit on two short lines"},
		{ID: "s3", DurationS: 3, Shot: "end_card", Voiceover: "Learn more."}}}
	f := QualityFindings(bad, q)
	var issues []string
	for _, x := range f {
		if x.Severity != "low" {
			t.Fatalf("quality findings never block: %+v", x)
		}
		issues = append(issues, x.Issue)
	}
	all := strings.Join(issues, " | ")
	for _, want := range []string{"hook takes longer", "first scene runs", "caption is longer", "too long", "generic"} {
		if !strings.Contains(all, want) {
			t.Errorf("findings lack %q: %s", want, all)
		}
	}
	silent := VideoScript{CTA: "Call 0712 000 000 to book a fitting", Scenes: []Scene{{ID: "s1", DurationS: 2}, {ID: "s2", DurationS: 3, Voiceover: "Book now."}}}
	if f := QualityFindings(silent, q); len(f) != 1 || !strings.Contains(f[0].Issue, "no words") {
		t.Fatalf("silent opening = %+v", f)
	}
}

// A UGC script is written on the ugc-video playbook with the chosen approach in the task, and
// its two voices survive; the same reply on a template ad loses its speakers.
func TestWriteVideoScriptUGCTwoVoices(t *testing.T) {
	var user string
	script := `{"format":"conversation","approach":"myth-fact","hooks":["","",""],"scenes":[
	 {"id":"s1","duration_s":2.5,"shot":"b_roll","voiceover":"[curious] Solar lamps die in an hour?","caption":"Myth?","speaker":"female"},
	 {"id":"s2","duration_s":4,"shot":"product_clip","voiceover":"[reassuring] This one runs 12 hours.","caption":"12 hours","speaker":"male"},
	 {"id":"s3","duration_s":3,"shot":"end_card","voiceover":"WhatsApp LAMP for delivery today.","caption":"KES 1,200"}],
	 "cta":"WhatsApp LAMP for delivery today"}`
	rt, f := newRuntime(promptopt.TaskSpec{}, func(sys, u string) string { user = u; return script }, fitDecider{})
	rt.opt = nil
	res, err := rt.WriteVideoScript(context.Background(), VideoScriptRequest{TenantID: uuid.New(), Offering: "Solar lamp",
		Facts: "Runs 12 hours on one charge. Price KES 1,200", Objective: "people think solar lamps are a myth", Seconds: 15, Photos: 1, Delivery: "ugc"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(speakersOf(*res.Script), ","); got != "female,male,female" {
		t.Fatalf("speakers = %s", got)
	}
	if res.Script.Approach != "myth-fact" || !strings.HasPrefix(res.Script.Scenes[0].Voiceover, "[curious] ") {
		t.Fatalf("script = %+v", res.Script)
	}
	if !strings.Contains(f.systems[0], "PLAYBOOK: UGC-style video ad") {
		t.Fatal("a UGC script must use the ugc-video playbook")
	}
	for _, want := range []string{"APPROACH: Myth versus fact", "Two voices may read this ad", "[reassuring]"} {
		if !strings.Contains(user, want) {
			t.Fatalf("task lacks %q:\n%s", want, user)
		}
	}

	res, err = rt.WriteVideoScript(context.Background(), VideoScriptRequest{TenantID: uuid.New(), Offering: "Solar lamp", Seconds: 15, Photos: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(speakersOf(*res.Script), ""); got != "" {
		t.Fatalf("a template ad kept speakers: %q", got)
	}
	if !strings.Contains(user, "One voice reads this whole ad") {
		t.Fatal("the template task must ask for one voice")
	}
}

// An asked-for testimonial without real reviews is not written; the owner is told why.
func TestWriteVideoScriptTestimonialNeedsReviews(t *testing.T) {
	var user string
	script := `{"format":"","hooks":["","",""],"scenes":[
	 {"id":"s1","duration_s":2.5,"shot":"b_roll","voiceover":"Tired of dim lamps?","caption":""},
	 {"id":"s2","duration_s":3,"shot":"end_card","voiceover":"WhatsApp LAMP today.","caption":"KES 1,200"}],"cta":"WhatsApp LAMP to 0712 000 000 today"}`
	rt, _ := newRuntime(promptopt.TaskSpec{}, func(sys, u string) string { user = u; return script }, fitDecider{})
	rt.opt = nil
	res, err := rt.WriteVideoScript(context.Background(), VideoScriptRequest{TenantID: uuid.New(), Offering: "Solar lamp",
		Seconds: 15, Photos: 1, Approach: "testimonial"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(user, "APPROACH: A real customer review") || res.Script.Approach == "testimonial" {
		t.Fatal("the testimonial approach was used without real reviews")
	}
	found := false
	for _, f := range res.Findings {
		found = found || strings.Contains(f.Issue, "needs real reviews")
	}
	if !found {
		t.Fatalf("no finding explains the swap: %+v", res.Findings)
	}
	_, err = rt.WriteVideoScript(context.Background(), VideoScriptRequest{TenantID: uuid.New(), Offering: "Solar lamp",
		Seconds: 15, Photos: 1, Approach: "testimonial", Reviews: []string{"Twelve hours every night, no fail. | Achieng, Kisumu"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(user, "APPROACH: A real customer review") || !strings.Contains(user, "Achieng, Kisumu") {
		t.Fatalf("the real review did not reach the writer:\n%s", user)
	}
}

func TestTwoVoicePlaybooksFromEnv(t *testing.T) {
	if TwoVoicePlaybooksFromEnv("") != nil {
		t.Fatal("empty must keep the default")
	}
	if l := TwoVoicePlaybooksFromEnv("off"); l == nil || len(l) != 0 {
		t.Fatalf("off = %v", l)
	}
	if l := TwoVoicePlaybooksFromEnv(" ugc-video, video-script "); strings.Join(l, ",") != "ugc-video,video-script" {
		t.Fatalf("list = %v", l)
	}
	rt := New(Config{Library: library.MustLoad(), TwoVoicePlaybooks: []string{}})
	if rt.twoVoice["ugc-video"] {
		t.Fatal("an empty list must turn two voices off")
	}
	if rt := New(Config{Library: library.MustLoad()}); !rt.twoVoice["ugc-video"] {
		t.Fatal("the default must allow two voices on ugc-video")
	}
}
