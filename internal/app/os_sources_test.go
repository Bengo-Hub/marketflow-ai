package app

import (
	"testing"

	"github.com/bengobox/marketflow-ai/internal/adintel"
	"github.com/bengobox/marketflow-ai/internal/llm"
)

func TestParseAdIntelSettingsDetailsDefaults(t *testing.T) {
	got := parseAdIntelSettings(map[string]string{"api_key": "k", "tenant_share": "7"})
	if got.Details != adintel.DefaultDetailSettings() {
		t.Fatalf("unset details fields keep their defaults: %+v", got.Details)
	}
	if got.TenantShare != 7 || got.PaidCap != 0 {
		t.Fatalf("paid use stays off unless switched on: %+v", got)
	}
}

func TestParseAdIntelSettingsOverrides(t *testing.T) {
	got := parseAdIntelSettings(map[string]string{
		"api_key": "k", "paid_enabled": "true", "paid_monthly_cap": "400",
		"details_per_tenant": "0", "details_platform_monthly": "20", "details_k": "2",
		"summary_model_class": "standard",
	})
	if got.PaidCap != 400 {
		t.Fatalf("the paid cap counts when paid use is on: %d", got.PaidCap)
	}
	want := adintel.DetailSettings{PerTenant: 0, PlatformMonthly: 20, K: 2}
	if got.Details != want {
		t.Fatalf("set fields are used, 0 included: got %+v want %+v", got.Details, want)
	}
	if got.SummaryClass != llm.Standard {
		t.Fatalf("summary class from the row: %s", got.SummaryClass)
	}
}

func TestParseAdIntelSettingsIgnoresBadNumbers(t *testing.T) {
	got := parseAdIntelSettings(map[string]string{"details_k": "many", "details_per_tenant": "-4"})
	if got.Details != adintel.DefaultDetailSettings() {
		t.Fatalf("an unreadable or negative value keeps the default: %+v", got.Details)
	}
}
