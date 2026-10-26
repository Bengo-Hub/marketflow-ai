package team

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/promptopt"
	"github.com/google/uuid"
)

// The studio's script step is one Content Creator step on the video-script
// playbook: no optimiser call, the template's shots and the owner's photo count
// in the task, and a presenter scene turned into narration over a photo.
func TestWriteVideoScriptForTheTemplate(t *testing.T) {
	var user string
	script := `{"format":"product in motion","hooks":["Dry skin?","",""],"scenes":[
	 {"id":"s1","duration_s":4,"shot":"presenter","asset":1,"visual":"","voiceover":"Dry skin in the morning? Look at this.","caption":"Dry skin?"},
	 {"id":"s2","duration_s":5,"shot":"product_clip","asset":7,"visual":"jar","voiceover":"Shea butter from Kisumu.","caption":"KES 850"},
	 {"id":"s3","duration_s":4,"shot":"end_card","asset":2,"visual":"","voiceover":"Order on WhatsApp.","caption":"Order now"}],"cta":"WhatsApp"}`
	rt, f := newRuntime(promptopt.TaskSpec{}, func(sys, u string) string { user = u; return script }, fitDecider{})
	rt.opt = nil // the studio path must not need the optimiser
	res, err := rt.WriteVideoScript(context.Background(), VideoScriptRequest{TenantID: uuid.New(), Offering: "Shea Butter Body Lotion",
		Facts: "Price KES 850", Seconds: 25, Photos: 2})
	if err != nil {
		t.Fatal(err)
	}
	s := res.Script.Scenes
	if s[0].Shot != "b_roll" || s[1].Asset != 0 || s[2].Asset != 2 || s[2].Shot != "end_card" {
		t.Fatalf("scenes = %+v", s)
	}
	for _, want := range []string{"25-second", "Shea Butter Body Lotion", "Price KES 850", "no presenter", "2 product photos", "under 350 characters"} {
		if !strings.Contains(user, want) {
			t.Fatalf("task lacks %q:\n%s", want, user)
		}
	}
	if len(f.systems) != 1 || !strings.Contains(f.systems[0], "PLAYBOOK: Video ad script") {
		t.Fatalf("one call on the video-script playbook expected, got %d", len(f.systems))
	}
}

// A script the compliance filter blocks is not returned; the findings say why.
func TestWriteVideoScriptBlocked(t *testing.T) {
	script := `{"format":"","hooks":["","",""],"scenes":[
	 {"id":"s1","duration_s":4,"shot":"product_clip","visual":"","voiceover":"This tea cures headaches.","caption":""},
	 {"id":"s2","duration_s":4,"shot":"end_card","visual":"","voiceover":"Order today.","caption":""}],"cta":""}`
	rt, _ := newRuntime(promptopt.TaskSpec{}, func(string, string) string { return script }, fitDecider{})
	res, err := rt.WriteVideoScript(context.Background(), VideoScriptRequest{TenantID: uuid.New(), Offering: "Herbal tea", Photos: 1})
	if !errors.Is(err, ErrScriptBlocked) || res == nil || len(res.Findings) == 0 || res.Script != nil {
		t.Fatalf("err %v, res %+v", err, res)
	}
	if _, err := rt.WriteVideoScript(context.Background(), VideoScriptRequest{TenantID: uuid.New(), Offering: "Tea"}); err == nil {
		t.Fatal("no photo and no clip must be refused before any model call")
	}
}
