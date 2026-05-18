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
	"go.uber.org/zap"
)

// serviceRegistry maps service names to their internal k8s base URLs.
// URLs are derived from devops-k8s/apps/*/values.yaml (nameOverride + service.port).
// Namespaces match the Helm release namespace (app folder name pattern).
var serviceRegistry = map[string]string{
	"ordering":      "http://ordering-backend.ordering.svc.cluster.local:4000",
	"inventory":     "http://inventory-api.inventory.svc.cluster.local:4000",
	"finance":       "http://treasury-api.treasury.svc.cluster.local:4000",
	"treasury":      "http://treasury-api.treasury.svc.cluster.local:4000",
	"subscriptions": "http://subscription-api.subscriptions.svc.cluster.local:4000",
	"pos":           "http://pos-api.pos.svc.cluster.local:4000",
	"logistics":     "http://logistics-api.logistics.svc.cluster.local:4000",
	"notifications": "http://notifications-api.notifications.svc.cluster.local:4000",
	"marketflow":    "http://marketflow-api.marketflow.svc.cluster.local:4000",
	// codevertex-website ClusterIP port is 80 (targetPort 3000 is the container port).
	"codevertex":    "http://codevertex-website.codevertex.svc.cluster.local:80",
}

// resourcePaths maps (service, resource) pairs to the specific internal API paths.
// Only exposes read-safe endpoints — no mutation paths.
var resourcePaths = map[string]map[string]string{
	"ordering": {
		"orders":   "/api/v1/internal/orders",
		"products": "/api/v1/internal/products",
	},
	"inventory": {
		"products": "/api/v1/internal/products",
		"stock":    "/api/v1/internal/stock",
	},
	"finance": {
		"invoices":     "/api/v1/internal/invoices",
		"transactions": "/api/v1/internal/transactions",
		"balance":      "/api/v1/internal/balance",
	},
	"treasury": {
		"invoices":     "/api/v1/internal/invoices",
		"transactions": "/api/v1/internal/transactions",
		"balance":      "/api/v1/internal/balance",
	},
	"subscriptions": {
		"plans":         "/api/v1/internal/plans",
		"subscriptions": "/api/v1/internal/subscriptions",
	},
	"pos": {
		"sessions": "/api/v1/internal/sessions",
		"sales":    "/api/v1/internal/sales",
	},
	"logistics": {
		"shipments": "/api/v1/internal/shipments",
		"routes":    "/api/v1/internal/routes",
	},
	"notifications": {
		"logs": "/api/v1/internal/notification-logs",
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
	http       *http.Client
	logger     *zap.Logger
}

// QueryServiceDataConfig configures the tool.
type QueryServiceDataConfig struct {
	ServiceKey string // INTERNAL_SERVICE_KEY
	Logger     *zap.Logger
}

func NewQueryServiceData(cfg QueryServiceDataConfig) *QueryServiceData {
	return &QueryServiceData{
		serviceKey: cfg.ServiceKey,
		http:       &http.Client{Timeout: 20 * time.Second},
		logger:     cfg.Logger.Named("tool.query_service_data"),
	}
}

func (*QueryServiceData) Name() string { return "query_service_data" }

func (*QueryServiceData) Description() string {
	return "Queries real-time data from a Codevertex microservice (ordering, inventory, finance, treasury, subscriptions, pos, logistics, notifications, marketflow, codevertex). Use when the user asks about their orders, invoices, enrollments, courses, stock levels, subscriptions, or any live data. Always specify 'service' and 'resource'. Apply filters to narrow results. Tenant data is automatically scoped — no cross-tenant access."
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
				"type":        "object",
				"description": "Optional key-value filters (e.g. {\"status\": \"pending\", \"limit\": 10})",
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

	paths, ok := resourcePaths[args.Service]
	if !ok {
		return "", fmt.Errorf("no resources configured for service %q", args.Service)
	}
	path, ok := paths[args.Resource]
	if !ok {
		return "", fmt.Errorf("unknown resource %q for service %q; valid: %s",
			args.Resource, args.Service, strings.Join(keys(paths), ", "))
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

	endpoint := baseURL + path + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("Accept", "application/json")

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("query_service_data http %s/%s: %w", args.Service, args.Resource, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
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
