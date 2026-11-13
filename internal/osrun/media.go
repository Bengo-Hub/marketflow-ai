package osrun

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/bengobox/marketflow-ai/internal/registry"
	"github.com/bengobox/marketflow-ai/internal/team"
)

// Studio video kinds (marketflow-api creative.VideoTemplate and friends).
const (
	KindTemplate         = "template"
	KindPresenter        = "presenter"
	KindPresenterPremium = "presenter_premium"
)

// Studio video length limits (marketflow-api minAdSeconds and maxAdSeconds) and the default.
const (
	minSeconds     = 15
	maxSeconds     = 60
	defaultSeconds = 30
)

// ErrNoScript is a run with no usable video script to make into a video.
var ErrNoScript = errors.New("osrun: this plan has no video script to make")

// HasVideoScript reports whether an approved run has a script Studio can make into a video.
func (r *Run) HasVideoScript() bool {
	_, _, err := r.PickScript(-1)
	return err == nil
}

// PickScript returns the video script to make and its step index. step >= 0 is the owner's
// pick and must be a usable video_script step; otherwise the first usable one is taken (the
// review keeps a blocked script's step but leaves it out of the shortlist, so a script that
// was shortlisted comes first).
func (r *Run) PickScript(step int) (*team.VideoScript, int, error) {
	if r == nil || !r.Approved {
		return nil, -1, ErrNoScript
	}
	read := func(i int) *team.VideoScript {
		if i < 0 || i >= len(r.Steps) {
			return nil
		}
		s := r.Steps[i]
		if s.Contract != "video_script" || s.Error != "" || len(s.Output) == 0 {
			return nil
		}
		var v team.VideoScript
		if json.Unmarshal(s.Output, &v) != nil || len(v.Scenes) < 2 {
			return nil
		}
		return &v
	}
	if step >= 0 {
		if v := read(step); v != nil {
			return v, step, nil
		}
		return nil, -1, ErrNoScript
	}
	for i := range r.Steps {
		if v := read(i); v != nil {
			return v, i, nil
		}
	}
	return nil, -1, ErrNoScript
}

// secondsInText finds a length in the request: "30 second", "30s", "30-sec", "sekunde 30".
var secondsInText = regexp.MustCompile(`(?i)\b(\d{1,3})\s*-?\s*(?:s|secs?|seconds?|sekunde)\b|\bsekunde\s*(\d{1,3})\b`)

// secondsInTier reads a registry tier's length, such as template_video_30s.
var secondsInTier = regexp.MustCompile(`_(\d{1,3})s$`)

// VideoSpec is the length and kind of the video a run asks for: the request's own words
// first ("a 30 second presenter video"), then the plan's tier, then 30 seconds and template.
// The length is kept within Studio's 15 to 60 seconds.
func (r *Run) VideoSpec() (seconds int, kind string) {
	tier := r.Tier
	if r.Plan != nil && r.Plan.Recommended.Tier != "" {
		tier = r.Plan.Recommended.Tier
	}
	seconds = defaultSeconds
	if m := secondsInTier.FindStringSubmatch(tier); m != nil {
		seconds, _ = strconv.Atoi(m[1])
	}
	if m := secondsInText.FindStringSubmatch(r.Text); m != nil {
		n := m[1]
		if n == "" {
			n = m[2]
		}
		if v, err := strconv.Atoi(n); err == nil {
			seconds = v
		}
	}
	seconds = min(max(seconds, minSeconds), maxSeconds)

	switch {
	case tier == registry.PresenterPremium:
		kind = KindPresenterPremium
	case tier == registry.Presenter || r.Playbook == "ugc-video":
		kind = KindPresenter
	default:
		kind = KindTemplate
	}
	text := strings.ToLower(r.Text)
	switch {
	case strings.Contains(text, "template"):
		kind = KindTemplate
	case kind == KindTemplate && strings.Contains(text, "presenter"):
		kind = KindPresenter
	}
	return seconds, kind
}

// FitScript makes a turn's script fit Studio. The turn wrote it before knowing the owner's
// photos, so its photo numbers are dropped and Studio's renderer takes the photos in order;
// then team.FitTemplate turns shots the kind cannot render into b_roll (a presenter kind
// keeps its presenter scenes). There is no owner footage on this path.
func FitScript(v *team.VideoScript, kind string) {
	for i := range v.Scenes {
		v.Scenes[i].Asset = 0
	}
	presenter := kind == KindPresenter || kind == KindPresenterPremium
	// One photo stands for "some photos": with every asset at 0 the count only decides that
	// scenes show photos rather than footage.
	team.FitTemplate(v, 1, 0, presenter)
}
