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
	"go.uber.org/zap"
)

// RequestIntegration is the agent tool that submits a platform-integration request (eTIMS/KRA
// tax fiscalization, TruLoad weighing hardware+software, camera/PLC/Arduino/general IoT
// automation) on the user's behalf, notifying the support team. It calls auth-api's S2S
// notify-only endpoint rather than the tenant-portal create endpoint, since this tool's caller
// identity is Vera itself (a service), not a browser session with an optional JWT -- the same
// path codevertex-website's own lead-capture form uses.
type RequestIntegration struct {
	authAPIURL string
	serviceKey string
	http       *http.Client
	logger     *zap.Logger
}

// RequestIntegrationConfig configures the tool.
type RequestIntegrationConfig struct {
	AuthAPIURL string // internal k8s URL for auth-api
	ServiceKey string // INTERNAL_SERVICE_KEY (X-API-Key header)
	Logger     *zap.Logger
}

func NewRequestIntegration(cfg RequestIntegrationConfig) *RequestIntegration {
	return &RequestIntegration{
		authAPIURL: strings.TrimSuffix(cfg.AuthAPIURL, "/"),
		serviceKey: cfg.ServiceKey,
		http:       &http.Client{Timeout: 15 * time.Second},
		logger:     cfg.Logger.Named("tool.request_integration"),
	}
}

func (*RequestIntegration) Name() string { return "request_integration" }

func (*RequestIntegration) Description() string {
	return "Submits a request to enable a Codevertex platform integration and notifies the support team. Use this when the user expresses interest in connecting/integrating with: eTIMS/KRA tax fiscalization, TruLoad weighing hardware or software, camera systems, PLCs, Arduino programming, or general IoT/automation hardware. Always collect the requester's name and email (and company name if they have one) before calling this tool, and confirm with the user what you're about to submit first -- only call this after they've agreed."
}

func (*RequestIntegration) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"request_type": map[string]any{
				"type":        "string",
				"enum":        []string{"etims_integration", "truload_weighing", "camera_systems", "plc_automation", "arduino_iot", "other_integration"},
				"description": "Which integration the user is interested in",
			},
			"integration_mode": map[string]any{
				"type":        "string",
				"enum":        []string{"self_serve", "assisted"},
				"description": "self_serve = the user's own developers will do the integration (no fee); assisted = Codevertex's team does the setup (a one-time fee may apply). Ask the user which they want; default to assisted if unsure.",
				"default":     "assisted",
			},
			"company_name":    map[string]any{"type": "string", "description": "The requester's company/business name, if any"},
			"requester_name":  map[string]any{"type": "string", "description": "Full name of the person requesting the integration"},
			"requester_email": map[string]any{"type": "string", "description": "Email address for follow-up"},
			"requester_phone": map[string]any{"type": "string", "description": "Phone number for follow-up (optional)"},
			"notes":           map[string]any{"type": "string", "description": "Any extra context from the conversation -- what they want to connect, volume/scale, timeline, etc."},
		},
		"required": []string{"request_type", "requester_name", "requester_email"},
	}
}

func (t *RequestIntegration) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	if t.authAPIURL == "" {
		return "", fmt.Errorf("request_integration: auth-api URL not configured")
	}
	if t.serviceKey == "" {
		return "", fmt.Errorf("request_integration: internal service key not configured")
	}

	var args struct {
		RequestType     string `json:"request_type"`
		IntegrationMode string `json:"integration_mode"`
		CompanyName     string `json:"company_name"`
		RequesterName   string `json:"requester_name"`
		RequesterEmail  string `json:"requester_email"`
		RequesterPhone  string `json:"requester_phone"`
		Notes           string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.RequestType == "" || args.RequesterName == "" || args.RequesterEmail == "" {
		return "", fmt.Errorf("request_type, requester_name and requester_email are required")
	}
	if args.IntegrationMode == "" {
		args.IntegrationMode = "assisted"
	}

	body := map[string]any{
		"request_type":     args.RequestType,
		"integration_mode": args.IntegrationMode,
		"company_name":     args.CompanyName,
		"requester_name":   args.RequesterName,
		"requester_email":  args.RequesterEmail,
		"requester_phone":  args.RequesterPhone,
		"notes":            args.Notes,
	}
	if tenantID != uuid.Nil {
		body["notes"] = strings.TrimSpace(args.Notes + fmt.Sprintf(" (tenant_id: %s)", tenantID))
	}

	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.authAPIURL+"/api/v1/s2s/notifications/etims-integration-request", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.serviceKey)

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request_integration http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		t.logger.Warn("request_integration api error",
			zap.Int("status", resp.StatusCode),
			zap.ByteString("body", respBody))
		return "", fmt.Errorf("auth-api %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	summary := map[string]any{
		"success":       true,
		"request_type":  args.RequestType,
		"message":       "Integration request submitted. Our team has been notified and will follow up by email shortly.",
		"confirmed_via": args.RequesterEmail,
	}
	out, _ := json.Marshal(summary)
	t.logger.Info("integration request submitted via agent tool",
		zap.String("tenant", tenantID.String()),
		zap.String("request_type", args.RequestType),
		zap.String("mode", args.IntegrationMode),
	)
	return string(out), nil
}
