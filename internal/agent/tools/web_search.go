package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// TavilySearcher queries the Tavily API for live web results.
// Enforces a per-tenant daily cap (Redis counter) to protect the free-tier quota.
type TavilySearcher struct {
	apiKey    string
	baseURL   string
	dailyCap  int
	redis     *redis.Client
	namespace string
	http      *http.Client
	logger    *zap.Logger
}

// TavilyConfig configures TavilySearcher.
type TavilyConfig struct {
	APIKey    string
	BaseURL   string
	DailyCap  int
	Redis     *redis.Client
	Namespace string
	Logger    *zap.Logger
}

func NewTavilySearcher(cfg TavilyConfig) *TavilySearcher {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.tavily.com"
	}
	if cfg.DailyCap <= 0 {
		cfg.DailyCap = 20
	}
	return &TavilySearcher{
		apiKey:    cfg.APIKey,
		baseURL:   strings.TrimSuffix(cfg.BaseURL, "/"),
		dailyCap:  cfg.DailyCap,
		redis:     cfg.Redis,
		namespace: cfg.Namespace,
		http:      &http.Client{Timeout: 20 * time.Second},
		logger:    cfg.Logger.Named("tool.web_search"),
	}
}

func (*TavilySearcher) Name() string { return "web_search" }

func (*TavilySearcher) Description() string {
	return "Performs a live web search via Tavily when the knowledge base lacks the answer OR when the user asks about recent/time-sensitive facts (news, pricing changes, events). Returns a short LLM-generated summary plus up to 5 snippets with URLs. Per-tenant daily quota applies."
}

func (*TavilySearcher) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":       map[string]any{"type": "string", "description": "The web search query"},
			"max_results": map[string]any{"type": "integer", "description": "1-5", "default": 5},
		},
		"required": []string{"query"},
	}
}

func (t *TavilySearcher) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	if t.apiKey == "" {
		return "", fmt.Errorf("tavily not configured")
	}
	var args struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.MaxResults <= 0 || args.MaxResults > 5 {
		args.MaxResults = 5
	}

	// Enforce per-tenant daily cap.
	if t.redis != nil {
		key := fmt.Sprintf("%s:tavily:quota:%s:%s", t.namespace, tenantID.String(), time.Now().UTC().Format("2006-01-02"))
		n, err := t.redis.Incr(ctx, key).Result()
		if err == nil {
			if n == 1 {
				t.redis.Expire(ctx, key, 36*time.Hour)
			}
			if int(n) > t.dailyCap {
				return "", fmt.Errorf("tavily daily cap (%d) reached for tenant", t.dailyCap)
			}
		}
	}

	reqBody, _ := json.Marshal(map[string]any{
		"api_key":       t.apiKey,
		"query":         args.Query,
		"search_depth":  "basic",
		"max_results":   args.MaxResults,
		"include_answer": true,
	})
	httpReq, err := http.NewRequestWithContext(ctx, "POST", t.baseURL+"/search", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("tavily: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tavily %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	// Tavily returns a rich object; pass only the useful fields through to the LLM.
	var tav struct {
		Answer  string `json:"answer"`
		Results []struct {
			Title   string  `json:"title"`
			URL     string  `json:"url"`
			Content string  `json:"content"`
			Score   float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &tav); err != nil {
		return "", fmt.Errorf("tavily decode: %w", err)
	}
	out, _ := json.Marshal(map[string]any{
		"answer":  tav.Answer,
		"results": tav.Results,
	})
	t.logger.Debug("web_search", zap.String("q", args.Query), zap.Int("n", len(tav.Results)))
	return string(out), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
