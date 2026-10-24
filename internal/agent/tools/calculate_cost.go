package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/registry"
)

// CalculateCost quotes an ad campaign the way MarketFlow bills it (decision of
// 2026-10-22, .claude/plans/marketflow-ads-autopilot-2026-10-22.md):
//
//   - the ad budget is paid to the ad platform at its own price, from the
//     client's own ad account; Codevertex adds no fee and holds no wallet;
//   - Codevertex charges only AI credits for making the ads (posters, copy),
//     priced by the output registry, from the plan allowance or a bundle.
type CalculateCost struct {
	registry func(context.Context) registry.Registry
	logger   *zap.Logger
}

// NewCalculateCost builds the tool; reg serves the live output registry.
func NewCalculateCost(reg func(context.Context) registry.Registry, logger *zap.Logger) *CalculateCost {
	return &CalculateCost{registry: reg, logger: logger.Named("tool.calculate_ad_cost")}
}

func (*CalculateCost) Name() string { return "calculate_ad_cost" }
func (*CalculateCost) Description() string {
	return "Quotes an ad campaign before the user commits. Returns two separate parts: the ad budget (daily budget x days), which the user pays to the ad platform from their own ad account at the platform's price with no MarketFlow fee, and the AI credits for making the ads (posters and ad copy). Never describe a MarketFlow fee or wallet: there is none."
}
func (*CalculateCost) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"daily_budget": map[string]any{"type": "number", "description": "Daily ad spend in the given currency"},
			"days":         map[string]any{"type": "integer", "description": "Number of days to run the campaign"},
			"currency":     map[string]any{"type": "string", "description": "KES or USD", "default": "KES"},
			"platform":     map[string]any{"type": "string", "description": "meta | google | tiktok | linkedin (optional, for the minimum daily spend check)"},
			"posters":      map[string]any{"type": "integer", "description": "How many poster ads to make (optional)"},
			"poster_tier":  map[string]any{"type": "string", "description": "standard | premium", "default": "standard"},
			"copy_batch":   map[string]any{"type": "boolean", "description": "Whether the team writes a batch of ad copy variants"},
		},
		"required": []string{"daily_budget", "days"},
	}
}

func (t *CalculateCost) Invoke(ctx context.Context, _ uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		DailyBudget float64 `json:"daily_budget"`
		Days        int     `json:"days"`
		Currency    string  `json:"currency"`
		Platform    string  `json:"platform"`
		Posters     int     `json:"posters"`
		PosterTier  string  `json:"poster_tier"`
		CopyBatch   bool    `json:"copy_batch"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.DailyBudget < 0 || args.Posters < 0 || args.Posters > 20 {
		return "", fmt.Errorf("daily_budget must be positive and posters 0 to 20")
	}
	if args.Days <= 0 {
		args.Days = 1
	}
	args.Currency = strings.ToUpper(strings.TrimSpace(args.Currency))
	if args.Currency != "USD" {
		args.Currency = "KES"
	}
	reg := registry.Defaults()
	if t.registry != nil {
		reg = t.registry(ctx)
	}
	fx := reg.Settings.FXRate
	if fx <= 0 {
		fx = registry.Defaults().Settings.FXRate
	}

	total := args.DailyBudget * float64(args.Days)
	dailyKES, totalKES := args.DailyBudget, total
	if args.Currency == "USD" {
		dailyKES, totalKES = args.DailyBudget*fx, total*fx
	}
	budget := map[string]any{
		"daily":     round2(args.DailyBudget),
		"days":      args.Days,
		"total":     round2(total),
		"currency":  args.Currency,
		"total_kes": math.Round(totalKES),
		"paid_to":   "the ad platform, from your own ad account, at the platform's price",
		"fee":       0,
	}
	var warnings []string
	if p := strings.ToLower(strings.TrimSpace(args.Platform)); p != "" {
		if min := reg.Settings.MinDailyTestKES[p]; min > 0 && dailyKES < min {
			warnings = append(warnings, fmt.Sprintf("%s needs about KES %.0f a day per live ad to learn; this budget is below that, so results will be slow and noisy.", p, min))
		}
	}

	credits := 0
	lines := []map[string]any{}
	if args.Posters > 0 {
		key := registry.Poster
		if args.PosterTier == "premium" {
			key = registry.PosterPremium
		}
		each, err := reg.Credits(key)
		if err != nil {
			return "", err
		}
		lines = append(lines, map[string]any{"item": reg.Outputs[key].Name, "qty": args.Posters, "credits": each * args.Posters})
		credits += each * args.Posters
	}
	if args.CopyBatch {
		each, err := reg.Credits(registry.CopyBatch)
		if err != nil {
			return "", err
		}
		lines = append(lines, map[string]any{"item": reg.Outputs[registry.CopyBatch].Name, "qty": 1, "credits": each})
		credits += each
	}

	summary := fmt.Sprintf("Ad budget: %s %.0f over %d days, paid to the platform from your own ad account (no MarketFlow fee).", args.Currency, total, args.Days)
	if credits > 0 {
		summary += fmt.Sprintf(" Making the ads: %d AI credits from your plan or a credit bundle.", credits)
	}
	out, _ := json.Marshal(map[string]any{
		"ad_budget":   budget,
		"ai_credits":  map[string]any{"lines": lines, "total": credits},
		"warnings":    warnings,
		"summary":     summary,
		"how_you_pay": "Ads are billed by the platform to the payment method on your own ad account. AI credits come from your MarketFlow plan or a credit bundle.",
	})
	t.logger.Debug("calculate_ad_cost", zap.Float64("budget_kes", totalKES), zap.Int("credits", credits))
	return string(out), nil
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
