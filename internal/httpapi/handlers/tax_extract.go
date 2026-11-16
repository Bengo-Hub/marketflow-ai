package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/guardrail"
	"github.com/bengobox/marketflow-ai/internal/llm"
)

// Tax rate extraction for treasury's source watch (owner request 2026-11-17, plan
// treasury-tax-advisor-live-rates). treasury-api sends a page it fetched from an allowlisted
// publisher (KRA, Kenya Law, the National Treasury, the Big Four tax alerts) and the rate codes it
// knows; a free model lists rate statements in the page. The model only proposes: every candidate
// must quote the page word for word and the quote must hold the number, here and again in treasury,
// and nothing changes until the platform owner approves it.

// TaxExtractor completes one model call; *llm.Router satisfies it.
type TaxExtractor interface {
	Complete(ctx context.Context, class llm.Class, req ai.ChatRequest) (*ai.ChatResponse, llm.Call, error)
}

// TaxExtractHandler serves POST /api/v1/internal/tax/extract (S2S).
type TaxExtractHandler struct {
	models TaxExtractor
	logger *zap.Logger
}

func NewTaxExtractHandler(models TaxExtractor, logger *zap.Logger) *TaxExtractHandler {
	return &TaxExtractHandler{models: models, logger: logger.Named("tax.extract")}
}

// KnownTaxCode is one statutory rate treasury already holds.
type KnownTaxCode struct {
	Code        string  `json:"code"`
	Name        string  `json:"name"`
	Category    string  `json:"category"`
	CurrentRate float64 `json:"current_rate"`
}

type taxExtractRequest struct {
	URL        string         `json:"url"`
	Title      string         `json:"title"`
	Content    string         `json:"content"`
	KnownCodes []KnownTaxCode `json:"known_codes"`
}

// TaxCandidate is one rate statement found in the page.
type TaxCandidate struct {
	Code          string  `json:"code"`
	Rate          float64 `json:"rate"`
	EffectiveFrom string  `json:"effective_from"`
	Excerpt       string  `json:"excerpt"`
}

const (
	maxTaxPage      = 40000
	maxTaxCodes     = 200
	maxTaxExcerpt   = 400
	maxTaxCandidate = 20
)

const taxExtractSystem = `You read a published Kenyan tax document and list statements of tax RATES it makes for the listed rate codes.
Reply with ONE JSON object: {"candidates":[{"code":"<a listed code>","rate":<percent number>,"effective_from":"YYYY-MM-DD or empty","excerpt":"<the sentence or table row copied WORD FOR WORD from the page that states this rate>"}]}
Rules: use only listed codes; the excerpt must be copied exactly from the page and must contain the number; leave out anything the page does not state plainly; never guess an effective date; at most 20 candidates; an empty list is a good answer.`

// Extract handles POST /api/v1/internal/tax/extract.
func (h *TaxExtractHandler) Extract(w http.ResponseWriter, r *http.Request) {
	var req taxExtractRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" || len(req.KnownCodes) == 0 {
		writeError(w, http.StatusBadRequest, "content and known_codes are required")
		return
	}
	if h.models == nil {
		writeError(w, http.StatusServiceUnavailable, "no model configured")
		return
	}
	if len([]rune(req.Content)) > maxTaxPage {
		req.Content = string([]rune(req.Content)[:maxTaxPage])
	}
	if len(req.KnownCodes) > maxTaxCodes {
		req.KnownCodes = req.KnownCodes[:maxTaxCodes]
	}
	codes, _ := json.Marshal(req.KnownCodes)
	user := "Listed rate codes:\n" + string(codes) + "\n\nPage " + req.URL + " (" + req.Title + "):\n" + guardrail.Quote("tax_page", req.Content)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	resp, call, err := h.models.Complete(ctx, llm.Nano, ai.ChatRequest{
		Messages: []ai.Message{
			{Role: "system", Content: taxExtractSystem + "\n" + guardrail.UntrustedRule},
			{Role: "user", Content: user},
		},
		Temperature:    0,
		MaxTokens:      1500,
		ResponseFormat: ai.JSONObject,
	})
	if err != nil || resp == nil || len(resp.Choices) == 0 {
		h.logger.Warn("tax extract: no model answer", zap.String("url", req.URL), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "no model available")
		return
	}
	var out struct {
		Candidates []TaxCandidate `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &out); err != nil {
		h.logger.Warn("tax extract: unreadable model answer", zap.String("url", req.URL))
		writeJSON(w, http.StatusOK, map[string]any{"candidates": []TaxCandidate{}, "model": call.Model})
		return
	}
	kept := FilterTaxCandidates(out.Candidates, req.Content, req.KnownCodes)
	writeJSON(w, http.StatusOK, map[string]any{"candidates": kept, "model": call.Model})
}

var (
	taxSpace  = regexp.MustCompile(`\s+`)
	taxNumber = regexp.MustCompile(`\d+(?:\.\d+)?`)
	taxDate   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

func normTaxText(s string) string {
	return strings.ToLower(taxSpace.ReplaceAllString(strings.TrimSpace(s), " "))
}

// FilterTaxCandidates keeps only candidates the page itself supports: a listed code, a rate from
// 0 to 100, an excerpt found word for word in the page that contains the rate's number, and an
// effective date that is empty or a real date. Duplicates (same code and rate) are dropped.
func FilterTaxCandidates(cands []TaxCandidate, page string, known []KnownTaxCode) []TaxCandidate {
	codes := map[string]bool{}
	for _, k := range known {
		codes[strings.ToUpper(strings.TrimSpace(k.Code))] = true
	}
	normPage := normTaxText(page)
	seen := map[string]bool{}
	out := []TaxCandidate{}
	for _, c := range cands {
		if len(out) >= maxTaxCandidate {
			break
		}
		c.Code = strings.ToUpper(strings.TrimSpace(c.Code))
		c.Excerpt = strings.TrimSpace(c.Excerpt)
		if !codes[c.Code] || c.Rate < 0 || c.Rate > 100 || c.Excerpt == "" || len([]rune(c.Excerpt)) > maxTaxExcerpt {
			continue
		}
		if !strings.Contains(normPage, normTaxText(c.Excerpt)) || !excerptHasRate(c.Excerpt, c.Rate) {
			continue
		}
		if c.EffectiveFrom != "" {
			if !taxDate.MatchString(c.EffectiveFrom) {
				continue
			}
			if _, err := time.Parse("2006-01-02", c.EffectiveFrom); err != nil {
				continue
			}
		}
		key := c.Code + "|" + strconv.FormatFloat(c.Rate, 'f', -1, 64)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

// excerptHasRate reports whether the excerpt states the rate as a number (16, 16.0, 0.5, 1.5).
func excerptHasRate(excerpt string, rate float64) bool {
	for _, m := range taxNumber.FindAllString(excerpt, -1) {
		if v, err := strconv.ParseFloat(m, 64); err == nil && v == rate {
			return true
		}
	}
	return false
}
