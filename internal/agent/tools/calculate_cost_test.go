package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/registry"
)

// The quote has no fee and no wallet: the budget is paid on the client's own ad
// account at cost, and only the creatives cost AI credits (registry prices).
func TestCalculateAdCostNoFee(t *testing.T) {
	tool := NewCalculateCost(func(context.Context) registry.Registry { return registry.Defaults() }, zap.NewNop())
	out, err := tool.Invoke(context.Background(), uuid.New(),
		`{"daily_budget":500,"days":10,"currency":"KES","platform":"tiktok","posters":3,"copy_batch":true}`)
	if err != nil {
		t.Fatal(err)
	}
	var q struct {
		AdBudget struct {
			Total float64 `json:"total"`
			Fee   float64 `json:"fee"`
		} `json:"ad_budget"`
		AICredits struct {
			Total int `json:"total"`
		} `json:"ai_credits"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &q); err != nil {
		t.Fatal(err)
	}
	reg := registry.Defaults()
	poster, _ := reg.Credits(registry.Poster)
	batch, _ := reg.Credits(registry.CopyBatch)
	if q.AdBudget.Total != 5000 || q.AdBudget.Fee != 0 || q.AICredits.Total != 3*poster+batch {
		t.Fatalf("quote: %s", out)
	}
	// KES 500 a day is below TikTok's learning minimum, so the user is warned.
	if len(q.Warnings) != 1 {
		t.Fatalf("warnings: %v", q.Warnings)
	}
	if strings.Contains(strings.ToLower(out), "wallet") || strings.Contains(strings.ToLower(out), "service fee") {
		t.Fatalf("legacy wording in the quote: %s", out)
	}
}
