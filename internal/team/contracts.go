package team

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Output contracts are the result shapes specialists return. Structured ones are
// JSON validated here; "answer" is plain markdown text.

// AdVariant is one ad copy variant.
type AdVariant struct {
	ID          string `json:"id"`
	Format      string `json:"format,omitempty"`
	Angle       string `json:"angle"`
	Hook        string `json:"hook"`
	Headline    string `json:"headline"`
	PrimaryText string `json:"primary_text"`
	CTA         string `json:"cta"`
	Hypothesis  string `json:"hypothesis"`
	Source      string `json:"source,omitempty"`
}

// AdCopyBatch is the ad_copy_batch contract.
type AdCopyBatch struct {
	Variants []AdVariant `json:"variants"`
}

// Scene is one video scene.
type Scene struct {
	ID        string  `json:"id"`
	DurationS float64 `json:"duration_s"`
	Shot      string  `json:"shot"` // presenter | product_clip | owner_footage | end_card | b_roll
	Visual    string  `json:"visual"`
	Voiceover string  `json:"voiceover"`
	Caption   string  `json:"caption"`
}

// VideoScript is the video_script contract.
type VideoScript struct {
	Format string   `json:"format"`
	Hooks  []string `json:"hooks"`
	Scenes []Scene  `json:"scenes"`
	CTA    string   `json:"cta"`
}

// PosterBrief is the poster_brief contract.
type PosterBrief struct {
	Headline    string   `json:"headline"`
	Subline     string   `json:"subline"`
	CTA         string   `json:"cta"`
	Layout      string   `json:"layout"`
	ImagePrompt string   `json:"image_prompt"`
	Sizes       []string `json:"sizes"`
}

// SocialPost is one post.
type SocialPost struct {
	Platform string `json:"platform"`
	Hook     string `json:"hook"`
	Text     string `json:"text"`
	Visual   string `json:"visual"`
}

// SocialPosts is the social_posts contract.
type SocialPosts struct {
	Posts []SocialPost `json:"posts"`
}

// CalendarItem is one content calendar slot.
type CalendarItem struct {
	Day         string `json:"day"`
	Platform    string `json:"platform"`
	Pillar      string `json:"pillar"`
	Format      string `json:"format"`
	Hook        string `json:"hook"`
	CTA         string `json:"cta"`
	AdCandidate bool   `json:"ad_candidate"`
}

// ContentCalendar is the content_calendar contract.
type ContentCalendar struct {
	Pillars []string       `json:"pillars"`
	Items   []CalendarItem `json:"items"`
}

// BrandContext is the brand_context contract (stored by marketflow-api in
// Tenant.metadata.marketing_context).
type BrandContext struct {
	Positioning   string   `json:"positioning"`
	Audience      []string `json:"audience"`
	Offers        []string `json:"offers"`
	Proof         []string `json:"proof"`
	Voice         string   `json:"voice"`
	VoiceExamples []string `json:"voice_examples"`
	BannedClaims  []string `json:"banned_claims"`
	Learnings     []string `json:"learnings"`
}

// Finding is one review finding.
type Finding struct {
	Quote    string `json:"quote"`
	Issue    string `json:"issue"`
	Severity string `json:"severity"` // high | review | low
	Fix      string `json:"fix"`
}

// ReviewFindings is the review_findings contract.
type ReviewFindings struct {
	Findings []Finding `json:"findings"`
	Verdict  string    `json:"verdict"`
}

// contractShapes are the JSON shapes shown to the model.
var contractShapes = map[string]string{
	"ad_copy_batch":    `{"variants":[{"id":"v1","format":"","angle":"","hook":"","headline":"","primary_text":"","cta":"","hypothesis":"","source":""}]}`,
	"video_script":     `{"format":"","hooks":["","",""],"scenes":[{"id":"s1","duration_s":3,"shot":"presenter|product_clip|owner_footage|end_card|b_roll","visual":"","voiceover":"","caption":""}],"cta":""}`,
	"poster_brief":     `{"headline":"","subline":"","cta":"","layout":"","image_prompt":"","sizes":["1080x1080","1080x1350","1080x1920"]}`,
	"social_posts":     `{"posts":[{"platform":"","hook":"","text":"","visual":""}]}`,
	"content_calendar": `{"pillars":[""],"items":[{"day":"","platform":"","pillar":"","format":"","hook":"","cta":"","ad_candidate":false}]}`,
	"brand_context":    `{"positioning":"","audience":[""],"offers":[""],"proof":[""],"voice":"","voice_examples":[""],"banned_claims":[""],"learnings":[]}`,
	"review_findings":  `{"findings":[{"quote":"","issue":"","severity":"high|review|low","fix":""}],"verdict":""}`,
}

// structured reports whether a contract is JSON.
func structured(contract string) bool {
	_, ok := contractShapes[contract]
	return ok
}

// maxField caps any single text field; longer values are model drift.
const maxField = 2200

// parseContract decodes and validates a specialist reply against its contract
// and returns the canonical JSON. n caps list sizes where the plan sets them.
func parseContract(contract, reply string, n int) (json.RawMessage, error) {
	body := extractJSON(reply)
	switch contract {
	case "ad_copy_batch":
		var v AdCopyBatch
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			return nil, fmt.Errorf("ad_copy_batch: %w", err)
		}
		out := v.Variants[:0]
		for i, a := range v.Variants {
			if strings.TrimSpace(a.Headline) == "" && strings.TrimSpace(a.PrimaryText) == "" {
				continue
			}
			if tooLong(a.Hook, a.Headline, a.PrimaryText, a.CTA, a.Hypothesis) {
				return nil, fmt.Errorf("ad_copy_batch: variant %d has an oversized field", i+1)
			}
			if a.ID == "" {
				a.ID = fmt.Sprintf("v%d", len(out)+1)
			}
			out = append(out, a)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("ad_copy_batch: no usable variants")
		}
		if n > 0 && len(out) > n {
			out = out[:n]
		}
		v.Variants = out
		return json.Marshal(v)
	case "video_script":
		var v VideoScript
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			return nil, fmt.Errorf("video_script: %w", err)
		}
		if len(v.Scenes) == 0 || len(v.Scenes) > 12 {
			return nil, fmt.Errorf("video_script: %d scenes", len(v.Scenes))
		}
		total := 0.0
		for i, s := range v.Scenes {
			if s.DurationS <= 0 || s.DurationS > 20 || tooLong(s.Visual, s.Voiceover, s.Caption) {
				return nil, fmt.Errorf("video_script: scene %d is invalid", i+1)
			}
			if s.ID == "" {
				v.Scenes[i].ID = fmt.Sprintf("s%d", i+1)
			}
			total += s.DurationS
		}
		if total > 90 {
			return nil, fmt.Errorf("video_script: %.0f s is too long", total)
		}
		return json.Marshal(v)
	case "poster_brief":
		var v PosterBrief
		if err := json.Unmarshal([]byte(body), &v); err != nil || v.Headline == "" {
			return nil, fmt.Errorf("poster_brief: invalid")
		}
		if tooLong(v.Headline, v.Subline, v.CTA, v.ImagePrompt) {
			return nil, fmt.Errorf("poster_brief: oversized field")
		}
		return json.Marshal(v)
	case "social_posts":
		var v SocialPosts
		if err := json.Unmarshal([]byte(body), &v); err != nil || len(v.Posts) == 0 {
			return nil, fmt.Errorf("social_posts: invalid")
		}
		return json.Marshal(v)
	case "content_calendar":
		var v ContentCalendar
		if err := json.Unmarshal([]byte(body), &v); err != nil || len(v.Items) == 0 || len(v.Items) > 62 {
			return nil, fmt.Errorf("content_calendar: invalid")
		}
		return json.Marshal(v)
	case "brand_context":
		var v BrandContext
		if err := json.Unmarshal([]byte(body), &v); err != nil || v.Positioning == "" {
			return nil, fmt.Errorf("brand_context: invalid")
		}
		return json.Marshal(v)
	case "review_findings":
		var v ReviewFindings
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			return nil, fmt.Errorf("review_findings: %w", err)
		}
		return json.Marshal(v)
	}
	return nil, fmt.Errorf("unknown contract %q", contract)
}

func tooLong(fields ...string) bool {
	for _, f := range fields {
		if len(f) > maxField {
			return true
		}
	}
	return false
}

func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return s
	}
	return s[start : end+1]
}
