package team

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// businessFacts mirrors marketflow-api marketingos.BusinessFacts.
type businessFacts struct {
	Name     string   `json:"name"`
	Tagline  string   `json:"tagline"`
	About    string   `json:"about"`
	Phone    string   `json:"phone"`
	WhatsApp string   `json:"whatsapp"`
	Email    string   `json:"email"`
	Website  string   `json:"website"`
	Address  string   `json:"address"`
	Hours    []string `json:"hours"`
	Summary  string   `json:"summary"`
}

const factsTTL = time.Hour

// BusinessFacts returns what the chat may state as true about the business, as prompt text,
// cached an hour. An empty string means nothing is known; the prompt rules still forbid guessing.
func (s *APISource) BusinessFacts(ctx context.Context, tenantID uuid.UUID) string {
	if s == nil || s.BaseURL == "" || s.Key == "" {
		return ""
	}
	key := s.Namespace + ":business-facts:" + tenantID.String()
	if s.Redis != nil {
		gctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		v, err := s.Redis.Get(gctx, key).Result()
		cancel()
		if err == nil {
			return v
		}
	}
	text, err := s.fetchFacts(ctx, tenantID)
	if err != nil {
		return ""
	}
	if s.Redis != nil {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 300*time.Millisecond)
		_ = s.Redis.Set(sctx, key, text, factsTTL).Err()
		cancel()
	}
	return text
}

func (s *APISource) fetchFacts(ctx context.Context, tenantID uuid.UUID) (string, error) {
	fctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	u := strings.TrimSuffix(s.BaseURL, "/") + "/api/v1/internal/os/business-facts?" + url.Values{"tenant_id": {tenantID.String()}}.Encode()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", s.Key)
	resp, err := s.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("business facts: status %d", resp.StatusCode)
	}
	var f businessFacts
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&f); err != nil {
		return "", err
	}
	return renderFacts(f), nil
}

// renderFacts lists only what is known, one fact a line.
func renderFacts(f businessFacts) string {
	var b strings.Builder
	line := func(label, v string) {
		if v = strings.TrimSpace(v); v != "" {
			fmt.Fprintf(&b, "- %s: %s\n", label, v)
		}
	}
	line("Business name", f.Name)
	line("Tagline", f.Tagline)
	line("About", f.About)
	line("Address", f.Address)
	if len(f.Hours) > 0 {
		line("Opening hours", strings.Join(f.Hours, "; "))
	}
	line("Phone", f.Phone)
	line("WhatsApp", f.WhatsApp)
	line("Email", f.Email)
	line("Website", f.Website)
	line("The website describes the business as", f.Summary)
	return b.String()
}
