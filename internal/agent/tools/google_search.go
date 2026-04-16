package tools

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
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// GoogleSearcher queries the Google Programmable Search Engine (Custom Search
// JSON API). Preferred over Tavily when a Google Cloud key is configured
// because the free tier (100 queries/day, shared) is generally sufficient
// for MarketFlow's agentic RAG workload and the result quality is stronger
// for product/FBO-adjacent queries.
type GoogleSearcher struct {
	apiKey    string
	cseID     string
	dailyCap  int
	redis     *redis.Client
	namespace string
	http      *http.Client
	logger    *zap.Logger
	fallback  Searcher
}

// Searcher is implemented by any web-search tool that can be used as a
// fallback when the primary provider hits a quota / 429.
type Searcher interface {
	Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error)
}

// GoogleSearchConfig configures GoogleSearcher.
type GoogleSearchConfig struct {
	APIKey    string
	CSEID     string
	DailyCap  int
	Redis     *redis.Client
	Namespace string
	Logger    *zap.Logger
	Fallback  Searcher
}

func NewGoogleSearcher(cfg GoogleSearchConfig) *GoogleSearcher {
	if cfg.DailyCap <= 0 {
		cfg.DailyCap = 80
	}
	return &GoogleSearcher{
		apiKey:    cfg.APIKey,
		cseID:     cfg.CSEID,
		dailyCap:  cfg.DailyCap,
		redis:     cfg.Redis,
		namespace: cfg.Namespace,
		http:      &http.Client{Timeout: 15 * time.Second},
		logger:    cfg.Logger.Named("tool.google_search"),
		fallback:  cfg.Fallback,
	}
}

// Name — registered as `web_search` so swapping providers is invisible to the LLM.
func (*GoogleSearcher) Name() string { return "web_search" }

func (*GoogleSearcher) Description() string {
	return "Performs a live web search via Google when the knowledge base lacks the answer OR when the user asks about recent/time-sensitive facts (news, pricing changes, events). Returns up to 5 snippets with URLs. Per-tenant daily quota applies."
}

func (*GoogleSearcher) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":       map[string]any{"type": "string", "description": "The web search query"},
			"max_results": map[string]any{"type": "integer", "description": "1-5", "default": 5},
		},
		"required": []string{"query"},
	}
}

func (g *GoogleSearcher) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	if g.apiKey == "" || g.cseID == "" {
		if g.fallback != nil {
			return g.fallback.Invoke(ctx, tenantID, argsJSON)
		}
		return "", fmt.Errorf("google custom search not configured")
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

	// Per-tenant daily cap. Failing-open on Redis errors is intentional —
	// we prefer a search call over blocking the agent on infra hiccups.
	if g.redis != nil {
		key := fmt.Sprintf("%s:gcse:quota:%s:%s", g.namespace, tenantID.String(), time.Now().UTC().Format("2006-01-02"))
		n, err := g.redis.Incr(ctx, key).Result()
		if err == nil {
			if n == 1 {
				g.redis.Expire(ctx, key, 36*time.Hour)
			}
			if int(n) > g.dailyCap {
				if g.fallback != nil {
					g.logger.Info("gcse cap reached, using fallback", zap.Int("cap", g.dailyCap))
					return g.fallback.Invoke(ctx, tenantID, argsJSON)
				}
				return "", fmt.Errorf("google search daily cap (%d) reached for tenant", g.dailyCap)
			}
		}
	}

	u := fmt.Sprintf(
		"https://www.googleapis.com/customsearch/v1?key=%s&cx=%s&q=%s&num=%d",
		url.QueryEscape(g.apiKey),
		url.QueryEscape(g.cseID),
		url.QueryEscape(args.Query),
		args.MaxResults,
	)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	resp, err := g.http.Do(req)
	if err != nil {
		if g.fallback != nil {
			return g.fallback.Invoke(ctx, tenantID, argsJSON)
		}
		return "", fmt.Errorf("gcse: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// 429 / 403 → fall back to secondary provider if available.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		g.logger.Warn("gcse quota/forbidden", zap.Int("status", resp.StatusCode))
		if g.fallback != nil {
			return g.fallback.Invoke(ctx, tenantID, argsJSON)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gcse %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var parsed struct {
		Items []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"items"`
		Queries struct {
			Request []struct {
				SearchTerms string `json:"searchTerms"`
			} `json:"request"`
		} `json:"queries"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("gcse decode: %w", err)
	}

	// Shape output to match what the LLM expects from web_search: {answer, results}.
	results := make([]map[string]any, 0, len(parsed.Items))
	var summary strings.Builder
	for i, it := range parsed.Items {
		results = append(results, map[string]any{
			"title":   it.Title,
			"url":     it.Link,
			"content": it.Snippet,
		})
		if i < 3 {
			summary.WriteString("• ")
			summary.WriteString(it.Title)
			summary.WriteString(" — ")
			summary.WriteString(it.Snippet)
			summary.WriteString("\n")
		}
	}
	out, _ := json.Marshal(map[string]any{
		"answer":  strings.TrimSpace(summary.String()),
		"results": results,
	})
	g.logger.Debug("google_search", zap.String("q", args.Query), zap.Int("n", len(results)))
	return string(out), nil
}
