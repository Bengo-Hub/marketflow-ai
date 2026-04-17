package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ImageSearch uses Google Custom Search to find relevant images. The agent
// calls this when composing responses about specific products so the chat
// UI can render images alongside text. Free tier: 100 queries/day shared
// with web_search (same API key + CSE). Falls back to FLP CDN image URLs
// when the CSE key isn't configured.
type ImageSearch struct {
	apiKey string
	cseID  string
	http   *http.Client
	logger *zap.Logger
}

func NewImageSearch(apiKey, cseID string, logger *zap.Logger) *ImageSearch {
	return &ImageSearch{
		apiKey: apiKey,
		cseID:  cseID,
		http:   &http.Client{Timeout: 10 * time.Second},
		logger: logger.Named("tool.image_search"),
	}
}

func (*ImageSearch) Name() string { return "search_images" }
func (*ImageSearch) Description() string {
	return "Search for product or topic images via Google. Returns up to 3 image URLs. Use when composing detailed responses about products to make the chat visually appealing."
}
func (*ImageSearch) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":       map[string]any{"type": "string", "description": "Image search query (e.g. 'Forever Aloe Vera Gel product')"},
			"max_results": map[string]any{"type": "integer", "description": "1-3", "default": 3},
		},
		"required": []string{"query"},
	}
}

func (t *ImageSearch) Invoke(ctx context.Context, _ uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.MaxResults <= 0 || args.MaxResults > 3 {
		args.MaxResults = 3
	}

	// If Google CSE is configured, use it for high-quality results.
	if t.apiKey != "" && t.cseID != "" {
		u := fmt.Sprintf(
			"https://www.googleapis.com/customsearch/v1?key=%s&cx=%s&q=%s&searchType=image&num=%d&safe=active",
			url.QueryEscape(t.apiKey), url.QueryEscape(t.cseID),
			url.QueryEscape(args.Query), args.MaxResults,
		)
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		resp, err := t.http.Do(req)
		if err == nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode == 200 {
				var parsed struct {
					Items []struct {
						Title string `json:"title"`
						Link  string `json:"link"`
						Image struct {
							Width  int `json:"width"`
							Height int `json:"height"`
						} `json:"image"`
					} `json:"items"`
				}
				if json.Unmarshal(body, &parsed) == nil && len(parsed.Items) > 0 {
					images := make([]map[string]any, 0, len(parsed.Items))
					for _, item := range parsed.Items {
						images = append(images, map[string]any{
							"title":  item.Title,
							"url":    item.Link,
							"width":  item.Image.Width,
							"height": item.Image.Height,
						})
					}
					out, _ := json.Marshal(map[string]any{"images": images})
					t.logger.Debug("image_search", zap.String("q", args.Query), zap.Int("n", len(images)))
					return string(out), nil
				}
			}
		}
	}

	// Fallback: try FLP CDN for common product images.
	fallback := []map[string]any{
		{"title": args.Query, "url": "https://cdn.foreverliving.com/content/products/images/" + url.PathEscape(args.Query) + ".jpg", "source": "flp_cdn_fallback"},
	}
	out, _ := json.Marshal(map[string]any{"images": fallback, "note": "Image search not configured — using FLP CDN fallback"})
	return string(out), nil
}
