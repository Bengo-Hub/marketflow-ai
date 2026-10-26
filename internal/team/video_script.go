package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/llm"
	"github.com/bengobox/marketflow-ai/internal/promptopt"
)

// The Creative Studio asks for a video ad script directly: the brief already
// names the offering and the owner already chose the video type, so there is
// no optimiser, plan or clarifying question. The step is the same specialist
// work as in Ask (the video-script playbook, contract validation with one
// repair, then the compliance review), so the rules are enforced once.

// VideoScriptRequest asks the Content Creator for one video ad script.
type VideoScriptRequest struct {
	TenantID  uuid.UUID
	Offering  string // the offering's name; the facts go in Facts
	Facts     string // price, description, promotion code and end, all true
	Objective string
	Audience  string
	Language  string // en (default) or sw
	Seconds   int    // the target length, 15 to 60
	Photos    int    // how many product photos the owner gave
	Footage   int    // how many video clips the owner gave
}

// VideoScriptResult is the reviewed script.
type VideoScriptResult struct {
	Script   *VideoScript `json:"script"`
	Findings []Finding    `json:"findings,omitempty"`
	CostUSD  float64      `json:"cost_usd"`
	Calls    []llm.Call   `json:"calls,omitempty"`
}

// ErrScriptBlocked is a script the review blocked; its findings say why.
var ErrScriptBlocked = errors.New("team: the script was blocked by the review")

// scriptBudget sits under marketflow-api's 100 s wait for the script.
var scriptBudget = 90 * time.Second

// templateShots are the shots a template video ad can render.
var templateShots = map[string]bool{"product_clip": true, "b_roll": true, "owner_footage": true, "end_card": true}

// WriteVideoScript writes and reviews one template video ad script.
func (r *Runtime) WriteVideoScript(ctx context.Context, in VideoScriptRequest) (*VideoScriptResult, error) {
	if strings.TrimSpace(in.Offering) == "" {
		return nil, fmt.Errorf("team: the offering is required")
	}
	if in.Seconds < 15 || in.Seconds > 60 {
		in.Seconds = 30
	}
	if in.Photos < 1 && in.Footage < 1 {
		return nil, fmt.Errorf("team: a video ad needs at least one product photo or clip")
	}
	ctx, cancel := context.WithTimeout(ctx, scriptBudget)
	defer cancel()
	meter := llm.MeterFrom(ctx)
	if meter == nil {
		meter = &llm.Meter{}
		ctx = llm.WithMeter(ctx, meter)
	}
	tc := TenantContext{}
	if r.source != nil {
		if loaded, err := r.source.Load(ctx, in.TenantID); err == nil {
			tc = loaded
		} else {
			r.logger.Warn("tenant context unavailable", zap.Error(err))
		}
	}
	pb, ok := r.lib.Playbook("video-script")
	if !ok {
		return nil, fmt.Errorf("team: the video-script playbook is missing")
	}
	m, ok := r.member("content_creator", tc)
	if !ok {
		m, _ = r.member("manager", tc)
	}
	task := scriptTask(in)
	spec := &promptopt.TaskSpec{Playbook: pb.Key, Specialist: m.Key, OptimisedBrief: task, Original: task,
		Entities: promptopt.Entities{Offering: in.Offering, Objective: in.Objective, Audience: in.Audience, Language: in.Language}}
	step := r.runStep(ctx, in.TenantID, plannedStep{member: m, playbook: pb, task: task}, spec, tc, "", 0)
	out := &VideoScriptResult{}
	defer func() {
		out.Calls = meter.Calls()
		_, out.CostUSD = meter.Totals()
	}()
	if step.Error != "" || step.Output == nil {
		return out, fmt.Errorf("team: no usable script: %s", step.Error)
	}
	_, out.Findings = r.review(ctx, &step, spec, tc, 0)
	if step.Error != "" || step.Output == nil {
		return out, ErrScriptBlocked
	}
	var v VideoScript
	if err := json.Unmarshal(step.Output, &v); err != nil {
		return out, fmt.Errorf("team: unreadable script: %w", err)
	}
	fitTemplate(&v, in.Photos, in.Footage)
	out.Script = &v
	return out, nil
}

// scriptTask tells the writer what this ad can show.
func scriptTask(in VideoScriptRequest) string {
	lang := "English"
	if in.Language == "sw" {
		lang = "Swahili"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Write a %d-second vertical video ad script in %s for: %s.\n", in.Seconds, lang, in.Offering)
	if f := strings.TrimSpace(in.Facts); f != "" {
		fmt.Fprintf(&b, "Facts you may use (use no other prices or claims):\n%s\n", trimTo(f, 1500))
	}
	if in.Objective != "" {
		fmt.Fprintf(&b, "Objective: %s.\n", in.Objective)
	}
	if in.Audience != "" {
		fmt.Fprintf(&b, "Audience: %s.\n", in.Audience)
	}
	b.WriteString("Shots available, and only these: product_clip and b_roll show one of the owner's product photos; ")
	if in.Footage > 0 {
		fmt.Fprintf(&b, "owner_footage shows one of the owner's %d video clips (asset 1 to %d); ", in.Footage, in.Footage)
	}
	b.WriteString("end_card is the last scene, with the call to action. There is no presenter and no person on screen.\n")
	if in.Photos > 0 {
		fmt.Fprintf(&b, "The owner gave %d product photos: set asset to 1 to %d to choose one per scene.\n", in.Photos, in.Photos)
	}
	fmt.Fprintf(&b, "The voiceover of each scene is what the narrator says during it. Keep all the voiceover together under %d characters "+
		"so the ad fits %d seconds, and keep each caption to a few words.", in.Seconds*14, in.Seconds)
	return b.String()
}

// fitTemplate makes a script renderable as a template ad: a presenter scene
// becomes narration over a photo, and an asset beyond what the owner gave is
// left for the renderer to pick in order.
func fitTemplate(v *VideoScript, photos, footage int) {
	for i := range v.Scenes {
		s := &v.Scenes[i]
		if !templateShots[s.Shot] || (s.Shot == "owner_footage" && footage == 0) {
			s.Shot = "b_roll"
		}
		limit := photos
		if s.Shot == "owner_footage" {
			limit = footage
		}
		if s.Asset > limit {
			s.Asset = 0
		}
	}
	if photos == 0 {
		// Footage only: every scene shows a clip.
		for i := range v.Scenes {
			v.Scenes[i].Shot = "owner_footage"
		}
	}
}
