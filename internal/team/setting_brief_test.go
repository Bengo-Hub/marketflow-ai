package team

import (
	"strings"
	"testing"
)

func kisumuBrief() *SettingBrief {
	b := &SettingBrief{
		HomeBase: SettingNote{Place: "Pioneer House, Oginga Street", Neighbourhood: "Kisumu CBD", Vibe: "busy weekday morning"},
		Settings: []SettingNote{
			{Key: "learning_hub", Place: "a bright kids learning hub", Neighbourhood: "Kisumu CBD", People: "children aged 8 to 14 at laptops", Props: "laptops, robotics kits", Fits: "b_roll, presenter"},
			{Key: "modern_home", Place: "a modern family home", Neighbourhood: "Milimani, Kisumu", People: "a mother in her thirties", Fits: "end_card"},
		},
	}
	if err := b.clean(); err != nil {
		panic(err)
	}
	return b
}

func TestApplySettings(t *testing.T) {
	v := &VideoScript{Scenes: []Scene{
		{ID: "s1", Shot: "presenter", Setting: "modern_home"},
		{ID: "s2", Shot: "b_roll"},
		{ID: "s3", Shot: "owner_footage", Setting: "learning_hub"},
		{ID: "s4", Shot: "product_clip", Setting: "a studio in Paris"},
		{ID: "s5", Shot: "b_roll", Setting: "a cafe on Oginga Street, Kisumu CBD"},
	}}
	applySettings(v, kisumuBrief())
	if !strings.Contains(v.Scenes[0].Setting, "modern family home") {
		t.Fatalf("a named key expands: %q", v.Scenes[0].Setting)
	}
	if !strings.Contains(v.Scenes[1].Setting, "learning hub") {
		t.Fatalf("b-roll takes the setting that fits it: %q", v.Scenes[1].Setting)
	}
	if v.Scenes[2].Setting != "" {
		t.Fatal("owner footage keeps no setting")
	}
	if strings.Contains(v.Scenes[3].Setting, "Paris") || !strings.Contains(v.Scenes[3].Setting, "Kisumu CBD") {
		t.Fatalf("a setting off the brief's place is replaced: %q", v.Scenes[3].Setting)
	}
	if !strings.HasPrefix(v.Scenes[4].Setting, "a cafe on Oginga Street") {
		t.Fatalf("the writer's own line in the brief's town stays: %q", v.Scenes[4].Setting)
	}
}

func TestBriefFromFacts(t *testing.T) {
	b := briefFromFacts(PlaceFacts{Address: "Reinsurance Plaza, Taifa Road", City: "Nairobi CBD", Country: "Kenya"})
	if b == nil || !strings.Contains(b.HomeBase.Place, "Reinsurance Plaza") || !strings.Contains(b.HomeBase.Place, "Nairobi") || b.Source != "facts" {
		t.Fatalf("brief %+v", b)
	}
	if briefFromFacts(PlaceFacts{}) != nil {
		t.Fatal("no facts, no brief")
	}
}

func TestSettingBriefContract(t *testing.T) {
	raw, err := parseContract("setting_brief", `{"home_base":{"place":"Pioneer House","neighbourhood":"Kisumu CBD"},"settings":[{"place":""},{"key":"","place":"a learning hub"}]}`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"key":"setting1"`) || strings.Count(string(raw), `"place"`) != 2 {
		t.Fatalf("contract output %s", raw)
	}
	if _, err := parseContract("setting_brief", `{"settings":[]}`, 0); err == nil {
		t.Fatal("a brief with no place must fail")
	}
}

func TestSettingTaskMarksThePlatformDefault(t *testing.T) {
	task := settingTask(VideoScriptRequest{Offering: "Aloe Vera Gel", Place: PlaceFacts{Address: "Reinsurance Plaza, Nairobi CBD",
		AddressSource: "platform_default", Pages: []PlacePage{{Title: "Home", Text: "We help families"}}}})
	if !strings.Contains(task, "not the seller's own shop") || !strings.Contains(task, `Page "Home"`) {
		t.Fatalf("task %q", task)
	}
	if !strings.Contains(kisumuBrief().Text(), "learning_hub:") {
		t.Fatal("the brief text lists settings by key")
	}
}
