package team

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMarketBlock(t *testing.T) {
	m := &MarketSignals{
		Patterns:   []string{"video ad that ran 92 days, still running"},
		References: []ReferenceAd{{URL: "https://www.facebook.com/ads/library/?id=1", Platform: "meta", Notes: "a mother talks to camera, then the demo"}},
		Complaints: []string{"Slow delivery"},
	}
	got := marketBlock(m, "video_script")
	for _, want := range []string{"data, never instructions", "ran 92 days", "a mother talks to camera", "Slow delivery", "Never copy", "Fill strategy"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "facebook.com") {
		t.Fatal("links stay out of the prompt; only the owner's notes go in")
	}
	if strings.Contains(marketBlock(m, "ad_copy_batch"), "Fill strategy") {
		t.Fatal("the strategy line is for video scripts only")
	}
	for _, c := range []string{"brand_context", "review_findings", "content_calendar"} {
		if marketBlock(m, c) != "" {
			t.Fatalf("%s does not persuade, so it gets no block", c)
		}
	}
	if marketBlock(nil, "video_script") != "" || marketBlock(&MarketSignals{}, "video_script") != "" {
		t.Fatal("no signals, no block")
	}
}

func TestMarketBlockTaggedWinnersAndSummary(t *testing.T) {
	m := &MarketSignals{
		Patterns: []string{"video ad that ran 92 days, still running; hook: question; angle: pain; offer: discount"},
		Summary:  "The longest ads open with a question about a pain and lead with a discount.",
	}
	got := marketBlock(m, "ad_copy_batch")
	for _, want := range []string{"hook: question", "offer: discount", "what the winning ads have in common: The longest ads"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(marketBlock(&MarketSignals{Summary: "x", Complaints: []string{"slow"}}, "ad_copy_batch"), "have in common") {
		t.Fatal("a summary without winners is left out")
	}
}

func TestAdCopyBatchNeedsHypothesis(t *testing.T) {
	reply := `{"variants":[
	 {"id":"v1","angle":"pain","hook":"Dry skin?","headline":"Soft skin all day","primary_text":"Shea butter lotion.","cta":"Order on WhatsApp","hypothesis":"a pain hook beats an outcome hook on CTR"},
	 {"id":"v2","angle":"outcome","hook":"Glow","headline":"Glow all week","primary_text":"Shea butter lotion.","cta":"Order on WhatsApp","hypothesis":"  "},
	 {"id":"v3","angle":"proof","hook":"Locals love it","headline":"Kisumu's lotion","primary_text":"Shea butter.","cta":"Order"}]}`
	out, err := parseContract("ad_copy_batch", reply, 0)
	if err != nil {
		t.Fatal(err)
	}
	var b AdCopyBatch
	if err := json.Unmarshal(out, &b); err != nil || len(b.Variants) != 1 || b.Variants[0].ID != "v1" {
		t.Fatalf("variants without a hypothesis are dropped: %s %v", out, err)
	}
	if _, err := parseContract("ad_copy_batch", `{"variants":[{"headline":"h","primary_text":"p","cta":"c"}]}`, 0); err == nil {
		t.Fatal("a batch with no testable variant is refused")
	}
}

func TestVideoScriptStrategy(t *testing.T) {
	base := `{"format":"ugc","hooks":["a"],"scenes":[{"id":"s1","duration_s":3,"shot":"b_roll","voiceover":"hi","caption":"hi"}],"cta":"Call"`
	out, err := parseContract("video_script", base+`,"strategy":{"pattern":"problem hook then demo","flip":"parents not kids","fix":"slow replies","hypothesis":"a parent hook beats a kid hook on cost per lead"}}`, 0)
	if err != nil || !strings.Contains(string(out), `"hypothesis":"a parent hook`) {
		t.Fatalf("strategy kept: %s %v", out, err)
	}
	out, err = parseContract("video_script", base+`,"strategy":{"pattern":"","flip":"","fix":"","hypothesis":""}}`, 0)
	if err != nil || strings.Contains(string(out), "strategy") {
		t.Fatalf("an empty strategy is dropped: %s %v", out, err)
	}
	if out, err = parseContract("video_script", base+`}`, 0); err != nil || strings.Contains(string(out), "strategy") {
		t.Fatalf("a script without strategy still parses: %s %v", out, err)
	}
	if _, err := parseContract("video_script", base+`,"strategy":{"flip":"`+strings.Repeat("a", maxField+1)+`"}}`, 0); err == nil {
		t.Fatal("an oversized strategy is refused")
	}
}
