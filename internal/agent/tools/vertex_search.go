package tools

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Searcher is a web-search tool another one can hand a query to when it cannot answer.
type Searcher interface {
	Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error)
}

// VertexSearcher answers web_search through Google Vertex AI Search (Discovery Engine), which
// replaces the Custom Search JSON API (closed to new customers, shut down on 2027-01-01).
// With a service account it calls the search method over OAuth; with only an API key it calls
// searchLite, which Google allows for public website data stores. A failure, an unconfigured
// engine or the daily cap hands the query to the fallback (Tavily).
type VertexSearcher struct {
	cfg       VertexConfig
	redis     *redis.Client
	namespace string
	http      *http.Client
	logger    *zap.Logger
	fallback  Searcher

	mu    sync.Mutex
	token string
	until time.Time
}

// VertexConfig configures VertexSearcher.
type VertexConfig struct {
	ProjectID          string
	Location           string // global (default), us or eu
	EngineID           string
	ServingConfig      string // default_search when empty
	ServiceAccountJSON string
	APIKey             string
	DailyCap           int
	Redis              *redis.Client
	Namespace          string
	Logger             *zap.Logger
	Fallback           Searcher
	// Endpoint and TokenURL replace Google's hosts in tests.
	Endpoint string
	TokenURL string
}

func NewVertexSearcher(cfg VertexConfig) *VertexSearcher {
	if cfg.Location == "" {
		cfg.Location = "global"
	}
	if cfg.ServingConfig == "" {
		cfg.ServingConfig = "default_search"
	}
	if cfg.DailyCap <= 0 {
		cfg.DailyCap = 100
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	return &VertexSearcher{cfg: cfg, redis: cfg.Redis, namespace: cfg.Namespace, http: &http.Client{Timeout: 15 * time.Second},
		logger: cfg.Logger.Named("tool.vertex_search"), fallback: cfg.Fallback}
}

func (*VertexSearcher) Name() string { return "web_search" }

func (*VertexSearcher) Description() string {
	return "Performs a live web search when the knowledge base lacks the answer OR when the user asks about recent or time-sensitive facts (news, pricing changes, events). Returns up to 5 snippets with URLs. Per-tenant daily quota applies."
}

func (*VertexSearcher) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":       map[string]any{"type": "string", "description": "The web search query"},
			"max_results": map[string]any{"type": "integer", "description": "1-5", "default": 5},
		},
		"required": []string{"query"},
	}
}

func (v *VertexSearcher) configured() bool {
	return v.cfg.ProjectID != "" && v.cfg.EngineID != "" && (v.cfg.ServiceAccountJSON != "" || v.cfg.APIKey != "")
}

func (v *VertexSearcher) fallBack(ctx context.Context, tenantID uuid.UUID, argsJSON string, why error) (string, error) {
	if v.fallback != nil {
		v.logger.Info("vertex search unavailable, using fallback", zap.Error(why))
		return v.fallback.Invoke(ctx, tenantID, argsJSON)
	}
	return "", why
}

func (v *VertexSearcher) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	if !v.configured() {
		return v.fallBack(ctx, tenantID, argsJSON, errors.New("vertex ai search not configured"))
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
	if v.redis != nil {
		key := fmt.Sprintf("%s:vertex:quota:%s:%s", v.namespace, tenantID.String(), time.Now().UTC().Format("2006-01-02"))
		if n, err := v.redis.Incr(ctx, key).Result(); err == nil {
			if n == 1 {
				v.redis.Expire(ctx, key, 36*time.Hour)
			}
			if int(n) > v.cfg.DailyCap {
				return v.fallBack(ctx, tenantID, argsJSON, fmt.Errorf("vertex search daily cap (%d) reached for tenant", v.cfg.DailyCap))
			}
		}
	}
	out, err := v.search(ctx, args.Query, args.MaxResults)
	if err != nil {
		return v.fallBack(ctx, tenantID, argsJSON, err)
	}
	return out, nil
}

// endpoint is the regional Discovery Engine host: global, or us- and eu- prefixed.
func (v *VertexSearcher) endpoint() string {
	if v.cfg.Endpoint != "" {
		return strings.TrimSuffix(v.cfg.Endpoint, "/")
	}
	if v.cfg.Location == "global" {
		return "https://discoveryengine.googleapis.com"
	}
	return "https://" + v.cfg.Location + "-discoveryengine.googleapis.com"
}

func (v *VertexSearcher) search(ctx context.Context, query string, n int) (string, error) {
	path := fmt.Sprintf("/v1/projects/%s/locations/%s/collections/default_collection/engines/%s/servingConfigs/%s",
		url.PathEscape(v.cfg.ProjectID), url.PathEscape(v.cfg.Location), url.PathEscape(v.cfg.EngineID), url.PathEscape(v.cfg.ServingConfig))
	body, _ := json.Marshal(map[string]any{
		"query":    query,
		"pageSize": n,
		"contentSearchSpec": map[string]any{
			"snippetSpec": map[string]any{"returnSnippet": true},
		},
	})
	var req *http.Request
	var err error
	if v.cfg.ServiceAccountJSON != "" {
		tok, terr := v.accessToken(ctx)
		if terr != nil {
			return "", terr
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint()+path+":search", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint()+path+":searchLite?key="+url.QueryEscape(v.cfg.APIKey), bytes.NewReader(body))
		if err != nil {
			return "", err
		}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("vertex search: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vertex search %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Results []struct {
			Document struct {
				DerivedStructData struct {
					Title    string `json:"title"`
					Link     string `json:"link"`
					Snippets []struct {
						Snippet string `json:"snippet"`
					} `json:"snippets"`
				} `json:"derivedStructData"`
			} `json:"document"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("vertex search decode: %w", err)
	}
	type result struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	}
	results := make([]result, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		d := r.Document.DerivedStructData
		var parts []string
		for _, s := range d.Snippets {
			if s.Snippet != "" {
				parts = append(parts, s.Snippet)
			}
		}
		results = append(results, result{Title: d.Title, URL: d.Link, Content: strings.Join(parts, " ")})
	}
	out, _ := json.Marshal(map[string]any{"results": results})
	return string(out), nil
}

// accessToken exchanges a signed service-account assertion for an OAuth token (Google's
// JWT bearer flow, scope cloud-platform) and keeps it until shortly before it expires.
func (v *VertexSearcher) accessToken(ctx context.Context) (string, error) {
	v.mu.Lock()
	if v.token != "" && time.Now().Before(v.until) {
		t := v.token
		v.mu.Unlock()
		return t, nil
	}
	v.mu.Unlock()

	var sa struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal([]byte(v.cfg.ServiceAccountJSON), &sa); err != nil || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return "", errors.New("vertex search: the service account JSON has no client_email or private_key")
	}
	tokenURL := firstNonEmptyStr(v.cfg.TokenURL, sa.TokenURI, "https://oauth2.googleapis.com/token")
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return "", errors.New("vertex search: the service account private_key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("vertex search: private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return "", errors.New("vertex search: the service account key is not RSA")
	}
	now := time.Now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iss": sa.ClientEmail, "scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud": tokenURL, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("vertex search: sign: %w", err)
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {signing + "." + enc.EncodeToString(sig)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := v.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("vertex search token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &tok) != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("vertex search token %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 3600
	}
	v.mu.Lock()
	v.token, v.until = tok.AccessToken, now.Add(time.Duration(tok.ExpiresIn-60)*time.Second)
	v.mu.Unlock()
	return tok.AccessToken, nil
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
