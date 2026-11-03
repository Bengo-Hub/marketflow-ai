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
	// Only the calling tenant's own profile. There is no fallback to another tenant's
	// profile: a missing profile is reported as missing.
	var profile map[string]any
	url := fmt.Sprintf("%s/api/v1/public/profile/%s", t.apiBaseURL, tenantID.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := t.http.Do(req)
	if err == nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			_ = json.Unmarshal(body, &profile)
		}
	}
	if profile == nil {
		return fmt.Sprintf(`{"error":"profile not found for tenant %s"}`, tenantID), nil
	}

	text := func(k string) string { s, _ := profile[k].(string); return s }
	slug, name := text("slug"), text("name")
	result := map[string]any{"name": name, "slug": slug}
	// Forever links follow marketflow-api's flp.ForTenant rule (flp_links.go). Only FLP
	// tenants get a /p page; marketflow-api sends the distributor fields only for them.
	if b, ok := flpForTenant(slug, text("fbo_distrib_id"), text("flp_store_code"), text("flp_language"), text("country")); ok && text("fbo_distrib_id") != "" {
		result["distrib_id"] = b.DistribID
		result["store_code"] = b.StoreCode
		result["language"] = b.Language
		result["shop_link"] = b.ShopURL()
		result["recruit_link"] = b.RecruitURL()
		result["profile_url"] = b.ProfileURL()
	}
	if wa := text("whatsapp_number"); wa != "" {
		result["whatsapp_link"] = whatsAppURL(wa, "")
	}

	out, _ := json.Marshal(result)
	t.logger.Debug("get_fbo_profile", zap.String("name", name), zap.Bool("forever", result["distrib_id"] != nil))
	return string(out), nil
}
