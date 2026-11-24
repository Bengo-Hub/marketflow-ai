package media

import (
	"strings"
	"testing"
)

// Only presenter scenes are lip-synced, so every other picture and clip keeps faces away from the
// camera (owner, 2026-11-24: the presenter smiled while the voice-over played).
func TestOnlyPresenterScenesTalkToTheCamera(t *testing.T) {
	for name, p := range map[string]string{
		"clip":        clipPrompt("a cashier serves a customer"),
		"empty clip":  clipPrompt(""),
		"ugc still":   StillPrompt("1080x1920", DeliveryUGC, "a cashier serves a customer"),
		"photo still": StillPrompt("1920x1080", "", "a cashier serves a customer"),
	} {
		if !strings.Contains(p, silentPeople) {
			t.Errorf("%s lets people talk to the camera: %q", name, p)
		}
	}
	for _, route := range []string{"fal/creatify-boreal", "higgsfield/seedance-2.5-480p"} {
		if p, _ := presenterPromptFor(route, "at the counter"); strings.Contains(p, silentPeople) {
			t.Errorf("%s: the presenter must talk", route)
		}
	}
	if got := reviewExpect(ScriptScene{Shot: ShotBRoll, Visual: "a busy cafe"}); got != "voice-over scene: a busy cafe" {
		t.Errorf("b-roll expectation %q", got)
	}
	if got := reviewExpect(ScriptScene{Shot: ShotProduct, Visual: "the till"}); got != "the till" {
		t.Errorf("owner photo expectation %q", got)
	}
}
