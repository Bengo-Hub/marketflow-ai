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
	return "Fetches the FBO's profile (name, distribID, shop link, recruit link, WhatsApp link, profile URL). ALWAYS call this for recruitment, joining, or product link questions so you can include personalised URLs."
}
func (*GetFBOProfile) Schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *GetFBOProfile) Invoke(ctx context.Context, tenantID uuid.UUID, _ string) (string, error) {
	// Try by UUID first, then by UUID as slug (handles public chat where
	// tenantID is a SHA1-derived UUID from the slug string).
	var profile map[string]any
	for _, lookup := range []string{tenantID.String()} {
		url := fmt.Sprintf("%s/api/v1/public/profile/%s", t.apiBaseURL, lookup)
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := t.http.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			json.Unmarshal(body, &profile)
			break
		}
	}

	// If UUID lookup failed, try the admin tenant list to find any FBO tenant
	if profile == nil {
		url := fmt.Sprintf("%s/api/v1/admin/tenants", t.apiBaseURL)
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		req.Header.Set("X-API-Key", t.apiKey)
		resp, err := t.http.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				var result struct {
					Data []map[string]any `json:"data"`
				}
				if json.Unmarshal(body, &result) == nil {
					for _, tn := range result.Data {
						if did, _ := tn["fbo_distrib_id"].(string); did != "" {
							profile = tn
							break
						}
					}
				}
			}
		}
	}

	if profile == nil {
		return fmt.Sprintf(`{"error":"profile not found for tenant %s"}`, tenantID), nil
	}

	// Build links from profile data
	slug, _ := profile["slug"].(string)
	did, _ := profile["fbo_distrib_id"].(string)
	waNum, _ := profile["whatsapp_number"].(string)
	name, _ := profile["name"].(string)
	store, _ := profile["flp_store_code"].(string)
	lang, _ := profile["flp_language"].(string)
	if store == "" { store = "KEN" }
	if lang == "" { lang = "en" }

	result := map[string]any{
		"name":        name,
		"slug":        slug,
		"distrib_id":  did,
		"profile_url": fmt.Sprintf("https://marketflow.codevertexafrica.com/p/%s", slug),
	}
	if did != "" {
		result["shop_link"] = fmt.Sprintf("https://shop.foreverliving.com/retail/entry/Shop.do?store=%s&language=%s&distribID=%s", store, lang, did)
		result["recruit_link"] = fmt.Sprintf("https://join.foreverliving.com/marketing/joinnow/applicationForm.do?action=display&store=%s&distribID=%s&menuoff=n&langID=%s", store, did, lang)
	}
	if waNum != "" {
		digits := ""
		for _, r := range waNum {
			if r >= '0' && r <= '9' { digits += string(r) }
		}
		result["whatsapp_link"] = fmt.Sprintf("https://wa.me/%s", digits)
	}

	out, _ := json.Marshal(result)
	t.logger.Debug("get_fbo_profile", zap.String("name", name), zap.String("did", did))
	return string(out), nil
}
