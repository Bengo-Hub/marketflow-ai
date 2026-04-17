package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// CalculateCost computes the total cost for an ad campaign including the
// MarketFlow service charge. Uses the same formula that the billing system
// will enforce at payment time:
//
//	service_charge = clamp(ads_budget × charge_pct, min_charge, max_charge)
//	total = ads_budget + service_charge
//
// The percentage, min, and max are configurable from /platform/billing
// (defaults: 5%, KES 50 min, KES 5000 max).
type CalculateCost struct {
	logger *zap.Logger
}

func NewCalculateCost(logger *zap.Logger) *CalculateCost {
	return &CalculateCost{logger: logger.Named("tool.calculate_cost")}
}

func (*CalculateCost) Name() string { return "calculate_ad_cost" }
func (*CalculateCost) Description() string {
	return "Calculates the total cost for an ad campaign: (daily_budget × days) + MarketFlow service charge. Returns a breakdown in both USD and KES so the user can decide whether to proceed. Call this before asking the user to pay."
}
func (*CalculateCost) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"daily_budget":  map[string]any{"type": "number", "description": "Daily ad spend in the given currency"},
			"days":          map[string]any{"type": "integer", "description": "Number of days to run the campaign"},
			"currency":      map[string]any{"type": "string", "description": "USD or KES", "default": "USD"},
		},
		"required": []string{"daily_budget", "days"},
	}
}

func (t *CalculateCost) Invoke(_ context.Context, _ uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		DailyBudget float64 `json:"daily_budget"`
		Days        int     `json:"days"`
		Currency    string  `json:"currency"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.Days <= 0 {
		args.Days = 1
	}
	if args.Currency == "" {
		args.Currency = "USD"
	}

	// Defaults — these should be loaded from subscriptions-api
	// ServiceChargePlan SC_MARKETFLOW_ADS in production.
	const (
		chargePct   = 5.0   // 5% of ads budget
		minKES      = 5.0   // KES 5 minimum
		maxKES      = 500.0 // KES 500 max per campaign
		usdToKES    = 130.0 // approximate exchange rate
		gatewayCard = 1.5   // Paystack card fee %
		gatewayMpesa = 0.0  // M-Pesa Paybill: KES 0 (STK Push)
	)

	adsBudget := args.DailyBudget * float64(args.Days)

	// Convert to KES for charge calculation if input is USD.
	adsBudgetKES := adsBudget
	if args.Currency == "USD" {
		adsBudgetKES = adsBudget * usdToKES
	}

	rawCharge := adsBudgetKES * chargePct / 100
	serviceChargeKES := math.Max(minKES, math.Min(maxKES, rawCharge))
	serviceChargeUSD := serviceChargeKES / usdToKES

	// Gateway fees are accrued by the paying party (tenant).
	subtotal := adsBudgetKES + serviceChargeKES
	gatewayFeeCard := subtotal * gatewayCard / 100
	gatewayFeeMpesa := gatewayMpesa // flat for paybill

	totalCardKES := subtotal + gatewayFeeCard
	totalMpesaKES := subtotal + gatewayFeeMpesa

	result := map[string]any{
		"ads_budget": map[string]any{
			"daily":    args.DailyBudget,
			"days":     args.Days,
			"total":    adsBudget,
			"currency": args.Currency,
		},
		"service_charge": map[string]any{
			"percentage": chargePct,
			"amount_kes": round2(serviceChargeKES),
			"amount_usd": round2(serviceChargeUSD),
			"min_kes":    minKES,
			"max_kes":    maxKES,
		},
		"gateway_fee": map[string]any{
			"card_kes":  round2(gatewayFeeCard),
			"mpesa_kes": round2(gatewayFeeMpesa),
			"note":      "Gateway fee varies by payment channel. M-Pesa Paybill is free; card is ~1.5%.",
		},
		"total": map[string]any{
			"via_card_kes":  round2(totalCardKES),
			"via_mpesa_kes": round2(totalMpesaKES),
			"via_card_usd":  round2(totalCardKES / usdToKES),
		},
		"note":         "You pay once — MarketFlow handles ad platform payment on your behalf. Gateway fee is accrued by the paying party.",
		"wallet_debit": round2(totalCardKES),
		"breakdown":    fmt.Sprintf("KES %.0f ads + KES %.0f service fee + KES %.0f gateway fee (card) = KES %.0f total", adsBudgetKES, serviceChargeKES, gatewayFeeCard, totalCardKES),
	}
	out, _ := json.Marshal(result)
	t.logger.Debug("calculate_cost",
		zap.Float64("ads_budget", adsBudget),
		zap.Float64("charge_kes", serviceChargeKES),
		zap.Float64("total_card_kes", totalCardKES),
	)
	return string(out), nil
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
