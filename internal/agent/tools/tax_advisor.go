package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// TaxAdvisor is the Vera agent tool for Kenya tax/legal advisory. It reads the treasury
// Smart Tax & Compliance module's statutory-rate reference and derived compliance calendar
// (read-only S2S, internal-service-key auth, tenant-scoped by path).
type TaxAdvisor struct {
	serviceKey string
	http       *http.Client
	logger     *zap.Logger
}

// TaxAdvisorConfig configures the tool.
type TaxAdvisorConfig struct {
	ServiceKey string // INTERNAL_SERVICE_KEY (shared S2S key)
	Logger     *zap.Logger
}

func NewTaxAdvisor(cfg TaxAdvisorConfig) *TaxAdvisor {
	return &TaxAdvisor{
		serviceKey: cfg.ServiceKey,
		http:       &http.Client{Timeout: 20 * time.Second},
		logger:     cfg.Logger.Named("tool.tax_advisor"),
	}
}

func (*TaxAdvisor) Name() string { return "tax_advisor" }

func (*TaxAdvisor) Description() string {
	return "Looks up live Kenya tax/statutory rates, the upcoming compliance calendar (filing/remittance deadlines), and the business's tax-registration eligibility position. Use for questions about VAT, PAYE, withholding tax, turnover tax, NSSF/SHIF/housing levy, corporate tax, capital allowances, 'when is my next tax due', or 'do I need to register for VAT / am I compliant' (eligibility checks turnover vs the VAT KES 5M / TOT thresholds and whether eTIMS is active). Returns authoritative data from the treasury Tax & Compliance module — prefer this over general knowledge."
}

func (*TaxAdvisor) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query_type": map[string]any{
				"type":        "string",
				"enum":        []string{"rates", "calendar", "eligibility", "position", "deductions", "allowances", "structuring"},
				"description": "'rates' statutory/tax rates; 'calendar' upcoming filing deadlines; 'eligibility' turnover-vs-threshold registration position + warnings; 'position' current-period VAT/TOT liability + per-obligation due dates & penalty exposure; 'deductions' deductible-expense optimizer ('No eTIMS No Expense') + CIT projection; 'allowances' the capital-allowance (wear-and-tear) schedule per KRA asset class; 'structuring' capital-structuring advice (director-loan vs injection vs salary vs dividend) + 30%-EBITDA interest-cap simulation",
			},
			"category": map[string]any{
				"type":        "string",
				"enum":        []string{"vat", "paye", "withholding", "tot", "cit", "nssf", "shif", "housing_levy", "capital_allowance", "sep", "cgt", "excise"},
				"description": "Optional rate category filter (only used when query_type='rates')",
			},
		},
		"required": []string{"query_type"},
	}
}

func (t *TaxAdvisor) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	if t.serviceKey == "" {
		return "", fmt.Errorf("tax_advisor: internal service key not configured")
	}
	var args struct {
		QueryType string `json:"query_type"`
		Category  string `json:"category"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	baseURL := serviceRegistry["treasury"]
	var path string
	switch args.QueryType {
	case "calendar":
		path = fmt.Sprintf("/api/v1/s2s/%s/compliance-calendar", tenantID.String())
	case "eligibility":
		path = fmt.Sprintf("/api/v1/s2s/%s/eligibility-position", tenantID.String())
	case "position":
		path = fmt.Sprintf("/api/v1/s2s/%s/position-estimate", tenantID.String())
	case "deductions":
		path = fmt.Sprintf("/api/v1/s2s/%s/deductions", tenantID.String())
	case "allowances":
		path = fmt.Sprintf("/api/v1/s2s/%s/capital-allowances", tenantID.String())
	case "structuring":
		path = fmt.Sprintf("/api/v1/s2s/%s/structuring", tenantID.String())
	case "rates", "":
		path = fmt.Sprintf("/api/v1/s2s/%s/statutory-rates", tenantID.String())
		if args.Category != "" {
			path += "?category=" + args.Category
		}
	default:
		return "", fmt.Errorf("unknown query_type %q (use rates/calendar/eligibility/position/deductions/allowances)", args.QueryType)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("Accept", "application/json")

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("tax_advisor http: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.logger.Warn("tax_advisor api error", zap.Int("status", resp.StatusCode))
		return "", fmt.Errorf("treasury tax advisor returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return truncate(string(body), 6000), nil
}
