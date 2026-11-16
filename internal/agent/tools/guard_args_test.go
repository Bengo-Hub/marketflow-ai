package tools

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/guardrail"
)

// The model's arguments never decide fields it should not: ad platform ids, metrics, status.
func TestCampaignBodyIsTypedAndDraft(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	body, err := campaignBody(campaignArgs{Name: "Gel launch", Platform: "Meta", BudgetDaily: 500, Currency: "kes", DurationDays: 7}, now)
	if err != nil {
		t.Fatal(err)
	}
	if body["budget_total"] != 3500.0 || body["currency"] != "KES" || body["platform"] != "meta" {
		t.Fatalf("body = %v", body)
	}
	meta := body["metadata"].(map[string]any)
	if meta["needs_review"] != true || meta["created_by"] != "ai_assistant" {
		t.Fatalf("draft marks missing: %v", meta)
	}
	for _, k := range []string{"status", "meta_campaign_id", "metrics", "funnel_id"} {
		if _, ok := body[k]; ok {
			t.Fatalf("%s must never come from the model", k)
		}
	}
	for _, bad := range []campaignArgs{
		{Name: "", Platform: "meta"},
		{Name: "x", Platform: "myspace"},
		{Name: "x", Platform: "meta", BudgetDaily: -1},
		{Name: "x", Platform: "meta", BudgetDaily: 1e12},
		{Name: "x", Platform: "meta", DurationDays: 400},
		{Name: "x", Platform: "meta", CampaignType: "pyramid"},
	} {
		if _, err := campaignBody(bad, now); err == nil {
			t.Errorf("campaignBody(%+v) should fail", bad)
		}
	}
}

func TestFunnelPayloadIsDraftWithTypedSteps(t *testing.T) {
	p, err := funnelPayload(funnelArgs{Name: "C9", Slug: "c9-challenge", Headline: "Start today", BrandColor: "#16a34a",
		Steps: []map[string]any{{"block_type": "form"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p["status"]; ok {
		t.Fatal("status must never be set by the model")
	}
	if p["steps"] == nil || p["funnel_type"] != "lead_capture" {
		t.Fatalf("payload = %v", p)
	}
	meta := p["metadata"].(map[string]any)
	if meta["headline"] != "Start today" || meta["needs_review"] != true || meta["theme"] == nil {
		t.Fatalf("metadata = %v", meta)
	}
	for _, bad := range []funnelArgs{
		{Name: "x", Slug: "../admin"},
		{Name: "x", Slug: "ok-slug", FunnelType: "phishing"},
		{Name: "x", Slug: "ok-slug", Steps: []map[string]any{{"block_type": "script"}}},
	} {
		if _, err := funnelPayload(bad); err == nil {
			t.Errorf("funnelPayload(%+v) should fail", bad)
		}
	}
	if p, _ := funnelPayload(funnelArgs{Name: "x", Slug: "ok-slug", BrandColor: "red;background:url(x)"}); p["metadata"].(map[string]any)["theme"] != nil {
		t.Fatal("a non-hex colour must be dropped")
	}
}

// A public booking goes only to an email the visitor typed; times and lengths are bounded.
func TestCheckBooking(t *testing.T) {
	now := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	start := now.Add(48 * time.Hour).Format(time.RFC3339)
	pub := guardrail.WithActor(context.Background(), guardrail.Actor{Origin: guardrail.OriginPublic, Said: "my email is jane@example.com"})
	owner := guardrail.WithActor(context.Background(), guardrail.Actor{Origin: guardrail.OriginChat, UserID: uuid.New()})

	d, mt := 0, ""
	if msg := checkBooking(pub, "jane@example.com", start, &d, &mt, now); msg != "" || d != 30 || mt != "product_demo" {
		t.Fatalf("visitor's own email refused: %q (%d, %s)", msg, d, mt)
	}
	d, mt = 0, ""
	if msg := checkBooking(pub, "victim@other.com", start, &d, &mt, now); msg == "" {
		t.Fatal("an email the visitor never typed must be refused")
	}
	d, mt = 0, ""
	if msg := checkBooking(owner, "lead@client.co.ke", start, &d, &mt, now); msg != "" {
		t.Fatalf("a signed-in owner may book any lead: %s", msg)
	}
	for _, c := range []struct {
		start string
		dur   int
		typ   string
	}{
		{now.Add(-48 * time.Hour).Format(time.RFC3339), 30, ""},
		{now.AddDate(1, 0, 0).Format(time.RFC3339), 30, ""},
		{start, 600, ""},
		{start, 30, "party"},
		{"tomorrow", 30, ""},
	} {
		d, mt := c.dur, c.typ
		if msg := checkBooking(owner, "lead@client.co.ke", c.start, &d, &mt, now); msg == "" {
			t.Errorf("booking %+v should be refused", c)
		}
	}
	d, mt = 0, ""
	if msg := checkBooking(owner, "not-an-email", start, &d, &mt, now); msg == "" {
		t.Fatal("an invalid email must be refused")
	}
}
