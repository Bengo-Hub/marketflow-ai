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

// GetFBOProfile fetches the tenant's FBO profile from marketflow-api and
// returns distribID, shop link, recruit link, WhatsApp link, profile URL.
// Enables the agent to include personalised links in every answer.
type GetFBOProfile struct {
	apiBaseURL string
	apiKey     string
	http       *http.Client
	logger     *zap.Logger
}

type FBOProfileConfig struct {
	APIBaseURL string
	APIKey     string
	Logger     *zap.Logger
}

func NewGetFBOProfile(cfg FBOProfileConfig) *GetFBOProfile {
	return &GetFBOProfile{
		apiBaseURL: cfg.APIBaseURL,
		apiKey:     cfg.APIKey,
		http:       &http.Client{Timeout: 10 * time.Second},
		logger:     cfg.Logger.Named("tool.get_fbo_profile"),
	}
}

func (*GetFBOProfile) Name() string { return "get_fbo_profile" }
func (*GetFBOProfile) Description() string {
	return "Fetches the FBO's profile (name, distribID, shop link, recruit link, WhatsApp link, profile URL). Use this to personalise recommendations with the FBO's own links instead of generic ones."
}
func (*GetFBOProfile) Schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *GetFBOProfile) Invoke(ctx context.Context, tenantID uuid.UUID, _ string) (string, error) {
	url := fmt.Sprintf("%s/api/v1/admin/providers/resolve?type=tenant_profile&name=fbo", t.apiBaseURL)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("X-API-Key", t.apiKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	// Fallback: use the public profile endpoint directly since it doesn't
	// require specific provider rows — every tenant has one via Tenant entity.
	fallbackURL := fmt.Sprintf("%s/api/v1/public/profile/%s", t.apiBaseURL, tenantID.String())
	fallbackReq, _ := http.NewRequestWithContext(ctx, "GET", fallbackURL, nil)

	resp, err := t.http.Do(fallbackReq)
	if err != nil {
		return "", fmt.Errorf("get_fbo_profile: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Sprintf(`{"error":"profile not found for tenant %s"}`, tenantID), nil
	}

	var profile struct {
		Name           string `json:"name"`
		Slug           string `json:"slug"`
		FboDistribID   string `json:"fbo_distrib_id"`
		WhatsappNumber string `json:"whatsapp_number"`
		FlpStoreCode   string `json:"flp_store_code"`
		FlpLanguage    string `json:"flp_language"`
		Country        string `json:"country"`
	}
	if json.Unmarshal(body, &profile) != nil {
		return string(body), nil
	}

	store := profile.FlpStoreCode
	if store == "" {
		store = "KEN"
	}
	lang := profile.FlpLanguage
	if lang == "" {
		lang = "en"
	}
	did := profile.FboDistribID

	result := map[string]any{
		"name":           profile.Name,
		"slug":           profile.Slug,
		"distrib_id":     did,
		"country":        profile.Country,
		"whatsapp_number": profile.WhatsappNumber,
		"profile_url":    fmt.Sprintf("https://marketflow.codevertexitsolutions.com/p/%s", profile.Slug),
	}
	if did != "" {
		result["shop_link"] = fmt.Sprintf("https://shop.foreverliving.com/retail/entry/Shop.do?store=%s&language=%s&distribID=%s", store, lang, did)
		result["recruit_link"] = fmt.Sprintf("https://join.foreverliving.com/marketing/joinnow/applicationForm.do?action=display&store=%s&distribID=%s&menuoff=n&langID=%s", store, did, lang)
	}
	if profile.WhatsappNumber != "" {
		digits := ""
		for _, r := range profile.WhatsappNumber {
			if r >= '0' && r <= '9' {
				digits += string(r)
			}
		}
		result["whatsapp_link"] = fmt.Sprintf("https://wa.me/%s?text=Hi%%20I'm%%20interested%%20in%%20your%%20products", digits)
	}

	out, _ := json.Marshal(result)
	t.logger.Debug("get_fbo_profile", zap.String("tenant", tenantID.String()), zap.String("name", profile.Name))
	return string(out), nil
}
