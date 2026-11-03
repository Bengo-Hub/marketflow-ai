package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The rule mirrors marketflow-api internal/flp ForTenant; these cases match its tests.
func TestFLPForTenantMirrorsMarketflowAPI(t *testing.T) {
	cases := []struct {
		slug, distrib, store, lang, country string
		ok                                  bool
		wantDistrib, wantStore, wantLang    string
	}{
		{"254200047984", "", "", "", "", true, "254200047984", "KEN", "en"},
		{"jane-shop", "", "", "", "UG", false, "", "", ""},
		{"jane-shop", "910000123", "", "", "UG", true, "910000123", "UGA", "en"},
		{"jane-shop", "910000123", "gbr", "FR", "UG", true, "910000123", "GBR", "fr"},
		{"1234567", "", "", "", "", false, "", "", ""},
		{"254200047984", "", "", "", "ZZ", true, "254200047984", "KEN", "en"},
	}
	for _, c := range cases {
		b, ok := flpForTenant(c.slug, c.distrib, c.store, c.lang, c.country)
		if ok != c.ok || b.DistribID != c.wantDistrib || b.StoreCode != c.wantStore || b.Language != c.wantLang {
			t.Errorf("flpForTenant(%q,%q,%q,%q,%q) = %+v,%v", c.slug, c.distrib, c.store, c.lang, c.country, b, ok)
		}
	}
	b, _ := flpForTenant("254200047984", "", "", "", "KE")
	if got := b.ShopURL(); got != "https://shop.foreverliving.com/retail/entry/Shop.do?distribID=254200047984&language=en&store=KEN" {
		t.Errorf("shop url %s", got)
	}
	if got := b.RecruitURL(); !strings.Contains(got, "distribID=254200047984") || !strings.Contains(got, "langID=en") || !strings.Contains(got, "store=KEN") {
		t.Errorf("recruit url %s", got)
	}
}

func TestGenerateLinksRefusesForeverLinkWithoutDistributor(t *testing.T) {
	g := NewGenerateLinks(zap.NewNop())
	out, err := g.Invoke(context.Background(), uuid.New(), `{"link_type":"shop","slug":"jane-shop"}`)
	if err != nil || !strings.Contains(out, "distrib_id is required") {
		t.Fatalf("got %s %v", out, err)
	}
	out, _ = g.Invoke(context.Background(), uuid.New(), `{"link_type":"product","item_code":"015","distrib_id":"910000123","country":"TZ"}`)
	if !strings.Contains(out, "store=TZA") || !strings.Contains(out, "itemCode=015") {
		t.Fatalf("product link %s", out)
	}
}

// get_fbo_profile reads only the caller's own profile and never falls back to another tenant.
func TestGetFBOProfileOwnTenantOnly(t *testing.T) {
	tenant := uuid.New()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/api/v1/public/profile/"+tenant.String() {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "Jane", "slug": "254200047984",
			"fbo_distrib_id": "254200047984", "flp_store_code": "", "flp_language": "", "country": "UG", "whatsapp_number": "+256 700 111222"})
	}))
	defer srv.Close()
	tool := NewGetFBOProfile(FBOProfileConfig{APIBaseURL: srv.URL, Logger: zap.NewNop()})
	out, err := tool.Invoke(context.Background(), tenant, "{}")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	_ = json.Unmarshal([]byte(out), &got)
	if got["store_code"] != "UGA" || !strings.Contains(got["shop_link"], "store=UGA") || got["whatsapp_link"] != "https://wa.me/256700111222" {
		t.Fatalf("profile %s", out)
	}
	out, _ = tool.Invoke(context.Background(), uuid.New(), "{}")
	if !strings.Contains(out, "profile not found") {
		t.Fatalf("missing profile should not fall back: %s", out)
	}
	for _, p := range paths {
		if strings.Contains(p, "admin") {
			t.Fatalf("called %s", p)
		}
	}
}
