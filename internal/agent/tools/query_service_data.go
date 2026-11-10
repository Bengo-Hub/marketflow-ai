package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// serviceRegistry maps service names to their internal k8s base URLs.
// URLs are derived from devops-k8s/apps/*/values.yaml (nameOverride + service.port).
// Namespaces match the Helm release namespace (app folder name pattern).
var serviceRegistry = map[string]string{
	"ordering":      "http://ordering-backend.ordering.svc.cluster.local:4000",
	"inventory":     "http://inventory-api.inventory.svc.cluster.local:4000",
	"treasury":      "http://treasury-api.treasury.svc.cluster.local:4000",
	"subscriptions": "http://subscription-api.subscriptions.svc.cluster.local:4000",
	"pos":           "http://pos-api.pos.svc.cluster.local:4000",
	"logistics":     "http://logistics-api.logistics.svc.cluster.local:4000",
	"notifications": "http://notifications-api.notifications.svc.cluster.local:4000",
	"marketflow":    "http://marketflow-api.marketflow.svc.cluster.local:4000",
	// codevertex-website ClusterIP port is 80 (targetPort 3000 is the container port).
	"codevertex": "http://codevertex-website.codevertex.svc.cluster.local:80",
}

// resourcePaths maps (service, resource) pairs to fixed internal read paths. Decision 61: no
// mutations, and no reads of money (invoices, transactions, balances, subscriptions billing), auth,
// users, payroll, HR, audit or logs. Further routes come from endpoint discovery once the platform
// owner approves them (see discovered).
var resourcePaths = map[string]map[string]string{
	"ordering": {
		"orders":   "/api/v1/internal/orders",
		"products": "/api/v1/internal/products",
	},
	"inventory": {
		"products": "/api/v1/internal/products",
		"stock":    "/api/v1/internal/stock",
	},
	"subscriptions": {
		"plans": "/api/v1/internal/plans",
	},
	"pos": {
		"sessions": "/api/v1/internal/sessions",
		"sales":    "/api/v1/internal/sales",
	},
	"logistics": {
		"shipments": "/api/v1/internal/shipments",
		"routes":    "/api/v1/internal/routes",
	},
	"marketflow": {
		"leads":     "/api/v1/internal/leads",
		"campaigns": "/api/v1/internal/campaigns",
	},
	// codevertex-website: exposes read-safe admin and public endpoints.
	// enrollments returns paginated enrollment records for a tenant/course.
	// courses returns the public course catalog.
	"codevertex": {
		"enrollments": "/api/admin/enrollments",
		"courses":     "/api/courses",
	},
}

// QueryServiceData is the agent tool that queries live data from any registered
// Codevertex microservice using the shared INTERNAL_SERVICE_KEY.
// Results are returned as JSON for the LLM to summarize into natural language.
//
// Tenant scoping is enforced: tenantID is always injected as a query parameter
// so cross-tenant data leakage is impossible even if the agent tries.
type QueryServiceData struct {
	serviceKey string
	apiURL     string // marketflow-api, for the approved discovered routes
	apiKey     string
	http       *http.Client
	logger     *zap.Logger
	// allowedBases are the only hosts a discovered URL may point at (the service registry).
	allowedBases []string

	mu    sync.Mutex
	cache map[uuid.UUID]discoveredSet
}

// QueryServiceDataConfig configures the tool.
type QueryServiceDataConfig struct {
	ServiceKey string // INTERNAL_SERVICE_KEY, sent to the services
	Logger     *zap.Logger
	// APIBaseURL and APIKey reach marketflow-api's /internal/endpoints/tools; empty turns
	// discovered routes off.
	APIBaseURL string
	APIKey     string
}

func NewQueryServiceData(cfg QueryServiceDataConfig) *QueryServiceData {
	bases := make([]string, 0, len(serviceRegistry))
	for _, b := range serviceRegistry {
		bases = append(bases, b)
	}
	return &QueryServiceData{
		serviceKey:   cfg.ServiceKey,
		apiURL:       strings.TrimSuffix(cfg.APIBaseURL, "/"),
		apiKey:       cfg.APIKey,
		http:         &http.Client{Timeout: 20 * time.Second},
		logger:       cfg.Logger.Named("tool.query_service_data"),
		allowedBases: bases,
		cache:        map[uuid.UUID]discoveredSet{},
	}
}

// discoveredTool is one approved route from endpoint discovery, tenant already in its URL.
type discoveredTool struct {
	Service  string   `json:"service"`
	Resource string   `json:"resource"`
	URL      string   `json:"url"`
	Params   []string `json:"params"`
}

type discoveredSet struct {
	tools []discoveredTool
	at    time.Time
}

const discoveredTTL = 10 * time.Minute

// maxResponseBytes bounds one service response read; the LLM sees at most 8 KB of it anyway.
const maxResponseBytes = 1 << 20

// discovered returns the tenant's approved routes, cached for a few minutes. A failed fetch
// returns none, so the fixed resources keep working.
func (t *QueryServiceData) discovered(ctx context.Context, tenantID uuid.UUID) []discoveredTool {
	if t.apiURL == "" || t.apiKey == "" {
		return nil
	}
	t.mu.Lock()
	if s, ok := t.cache[tenantID]; ok && time.Since(s.at) < discoveredTTL {
		t.mu.Unlock()
		return s.tools
	}
	t.mu.Unlock()
	u := t.apiURL + "/api/v1/internal/endpoints/tools?" + url.Values{"tenant_id": {tenantID.String()}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-API-Key", t.apiKey)
	resp, err := t.http.Do(req)
	if err != nil {
		t.logger.Warn("discovered tools fetch failed", zap.Error(err))
		return nil
	}
	defer resp.Body.Close()
	var body struct {
		Tools []discoveredTool `json:"tools"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&body) != nil {
		t.logger.Warn("discovered tools fetch failed", zap.Int("status", resp.StatusCode))
		return nil
	}
	t.mu.Lock()
	t.cache[tenantID] = discoveredSet{tools: body.Tools, at: time.Now()}
	t.mu.Unlock()
	return body.Tools
}

// allowedURL reports whether a discovered URL points at one of the known services.
func (t *QueryServiceData) allowedURL(u string) bool {
	for _, b := range t.allowedBases {
		if strings.HasPrefix(u, strings.TrimSuffix(b, "/")+"/") {
			return true
		}
	}
	return false
}

// fillParams puts the agent's values into a discovered URL's {params}. A value may not carry a
// slash or "..", so it can never reach another route.
func fillParams(raw string, names []string, values map[string]string) (string, error) {
	for _, n := range names {
		v := strings.TrimSpace(values[n])
		if v == "" {
			return "", fmt.Errorf("param %q is required", n)
		}
		if strings.ContainsAny(v, `/\`) || strings.Contains(v, "..") {
			return "", fmt.Errorf("param %q has an invalid value", n)
		}
		raw = strings.ReplaceAll(raw, "{"+n+"}", url.PathEscape(v))
	}
	if strings.Contains(raw, "{") {
		return "", fmt.Errorf("route needs params it was not given")
	}
	return raw, nil
}

func (*QueryServiceData) Name() string { return "query_service_data" }

func (*QueryServiceData) Description() string {
	return "Reads live marketing data from a Codevertex service (ordering, inventory, pos, logistics, subscriptions plans, marketflow, codevertex courses, plus any routes the platform approved). Use for orders, products, stock, promotions, deals, delivery zones, courses and similar. Always give 'service' and 'resource'; an unknown resource returns the valid list, and a resource that needs path values says which to pass in 'params'. Read only, scoped to this business. Money, payments, users, staff and logs are never available."
}

func (*QueryServiceData) Schema() map[string]any {
	services := make([]string, 0, len(serviceRegistry))
	for k := range serviceRegistry {
		services = append(services, k)
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"service": map[string]any{
				"type":        "string",
				"enum":        services,
				"description": "Which Codevertex microservice to query",
			},
			"resource": map[string]any{
				"type":        "string",
				"description": "Resource type to fetch (e.g. 'orders', 'invoices', 'products', 'subscriptions')",
			},
			"filters": map[string]any{
				"type":                 "object",
				"description":          "Optional key-value filters (e.g. {\"status\": \"pending\", \"limit\": 10})",
				"additionalProperties": map[string]any{"type": "string"},
			},
			"params": map[string]any{
				"type":                 "object",
				"description":          "Path values a resource needs, named in its error message (e.g. {\"itemId\": \"42\"})",
				"additionalProperties": map[string]any{"type": "string"},
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum number of results to return (default 10, max 50)",
				"default":     10,
			},
		},
		"required": []string{"service", "resource"},
	}
}

func (t *QueryServiceData) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	if t.serviceKey == "" {
		return "", fmt.Errorf("query_service_data: internal service key not configured")
	}

	var args struct {
		Service  string            `json:"service"`
		Resource string            `json:"resource"`
		Filters  map[string]string `json:"filters"`
		Params   map[string]string `json:"params"`
		Limit    int               `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.Service == "" || args.Resource == "" {
		return "", fmt.Errorf("service and resource are required")
	}
	if args.Limit <= 0 || args.Limit > 50 {
		args.Limit = 10
	}

	baseURL, ok := serviceRegistry[args.Service]
	if !ok {
		return "", fmt.Errorf("unknown service %q; valid: %s", args.Service, strings.Join(keys(serviceRegistry), ", "))
	}

	// A fixed resource first, then the routes the platform owner approved through discovery.
	target := ""
	if path, ok := resourcePaths[args.Service][args.Resource]; ok {
		target = baseURL + path
	} else {
		valid := keys(resourcePaths[args.Service])
		for _, d := range t.discovered(ctx, tenantID) {
			if d.Service != args.Service {
				continue
			}
			valid = append(valid, d.Resource)
			if d.Resource != args.Resource {
				continue
			}
			if !t.allowedURL(d.URL) {
				return "", fmt.Errorf("resource %q points outside the known services", args.Resource)
			}
			filled, err := fillParams(d.URL, d.Params, args.Params)
			if err != nil {
				return "", fmt.Errorf("%s/%s: %w (needs params: %s)", args.Service, args.Resource, err, strings.Join(d.Params, ", "))
			}
			target = filled
			break
		}
		if target == "" {
			return "", fmt.Errorf("unknown resource %q for service %q; valid: %s", args.Resource, args.Service, strings.Join(valid, ", "))
		}
	}

	// Build query params — tenant scoping is always enforced
	params := url.Values{}
	params.Set("tenant_id", tenantID.String())
	params.Set("limit", fmt.Sprintf("%d", args.Limit))
	for k, v := range args.Filters {
		// Skip any attempt to override tenant_id from filters
		if strings.EqualFold(k, "tenant_id") {
			continue
		}
		params.Set(k, v)
	}

	endpoint := target + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())
	req.Header.Set("Accept", "application/json")

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("query_service_data http %s/%s: %w", args.Service, args.Resource, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 300 {
		t.logger.Warn("query_service_data api error",
			zap.String("service", args.Service),
			zap.String("resource", args.Resource),
			zap.Int("status", resp.StatusCode),
		)
		return "", fmt.Errorf("%s/%s returned %d: %s", args.Service, args.Resource, resp.StatusCode, truncate(string(body), 200))
	}

	// Truncate large payloads to avoid burning the LLM context budget.
	// The LLM only needs a summary, not raw dumps of thousands of records.
	var result any
	if err := json.Unmarshal(body, &result); err != nil {
		return string(body[:min(len(body), 2000)]), nil
	}

	// If result is an array, cap at limit
	if arr, ok := result.([]any); ok && len(arr) > args.Limit {
		result = arr[:args.Limit]
	}

	out, _ := json.Marshal(map[string]any{
		"service":  args.Service,
		"resource": args.Resource,
		"count":    countOf(result),
		"data":     result,
	})

	if len(out) > 8000 {
		out = out[:8000]
		out = append(out, []byte("...}")...)
	}

	t.logger.Info("service data queried via agent tool",
		zap.String("tenant", tenantID.String()),
		zap.String("service", args.Service),
		zap.String("resource", args.Resource),
	)
	return string(out), nil
}

func keys[K comparable, V any](m map[K]V) []K {
	ks := make([]K, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func countOf(v any) int {
	if arr, ok := v.([]any); ok {
		return len(arr)
	}
	return 1
}
