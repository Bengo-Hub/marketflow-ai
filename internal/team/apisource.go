package team

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// APISource loads the tenant context from marketflow-api, which owns it
// (marketing context in Tenant.metadata, team member rows, KB offerings), over
// GET /api/v1/internal/os/context with the internal service key. Results are
// cached in Redis for five minutes; a save in the portal is seen within that
// window.
type APISource struct {
	BaseURL   string
	Key       string
	Redis     *redis.Client
	Namespace string
	HTTP      *http.Client
}

const contextTTL = 5 * time.Minute

// Load implements ContextSource.
func (s *APISource) Load(ctx context.Context, tenantID uuid.UUID) (TenantContext, error) {
	var tc TenantContext
	if s == nil || s.BaseURL == "" || s.Key == "" {
		return tc, fmt.Errorf("os context: marketflow-api not configured")
	}
	cacheKey := s.Namespace + ":os:ctx:" + tenantID.String()
	if s.Redis != nil {
		if raw, err := s.Redis.Get(ctx, cacheKey).Bytes(); err == nil && json.Unmarshal(raw, &tc) == nil {
			return tc, nil
		}
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 4 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(s.BaseURL, "/")+"/api/v1/internal/os/context?tenant_id="+tenantID.String(), nil)
	if err != nil {
		return tc, err
	}
	req.Header.Set("X-API-Key", s.Key)
	resp, err := client.Do(req)
	if err != nil {
		return tc, fmt.Errorf("os context: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tc, fmt.Errorf("os context: status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return tc, err
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return tc, fmt.Errorf("os context: %w", err)
	}
	if s.Redis != nil {
		_ = s.Redis.Set(ctx, cacheKey, raw, contextTTL).Err()
	}
	return tc, nil
}
