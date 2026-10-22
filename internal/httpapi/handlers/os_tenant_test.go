package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
)

func reqWith(claims *authclient.Claims, header string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/os/ask", nil)
	if header != "" {
		r.Header.Set("X-Tenant-ID", header)
	}
	if claims != nil {
		r = r.WithContext(authclient.ContextWithClaims(r.Context(), claims))
	}
	return r
}

func TestTenantFromAuth(t *testing.T) {
	own, other := uuid.New(), uuid.New()
	user := &authclient.Claims{TenantID: own.String()}

	if id, status, _ := tenantFromAuth(reqWith(user, "")); status != 0 || id != own {
		t.Fatalf("token tenant: %v %d", id, status)
	}
	if id, status, _ := tenantFromAuth(reqWith(user, own.String())); status != 0 || id != own {
		t.Fatalf("matching header: %v %d", id, status)
	}
	if _, status, _ := tenantFromAuth(reqWith(user, other.String())); status != http.StatusForbidden {
		t.Fatalf("another tenant's header must be 403, got %d", status)
	}
	service := &authclient.Claims{TenantID: own.String(), IsService: true}
	if id, status, _ := tenantFromAuth(reqWith(service, other.String())); status != 0 || id != other {
		t.Fatalf("service caller naming a tenant: %v %d", id, status)
	}
	if _, status, _ := tenantFromAuth(reqWith(nil, other.String())); status != http.StatusUnauthorized {
		t.Fatalf("no claims must be 401, got %d", status)
	}
	if _, status, _ := tenantFromAuth(reqWith(&authclient.Claims{}, "")); status != http.StatusBadRequest {
		t.Fatalf("no tenant anywhere must be 400, got %d", status)
	}
}

func TestHeaderTenantAllowed(t *testing.T) {
	own, other := uuid.New(), uuid.New()
	user := &authclient.Claims{TenantID: own.String()}
	if !headerTenantAllowed(reqWith(user, ""), own) || headerTenantAllowed(reqWith(user, ""), other) {
		t.Fatal("user tenant check wrong")
	}
	if !headerTenantAllowed(reqWith(&authclient.Claims{IsService: true}, ""), other) {
		t.Fatal("service callers act for any tenant")
	}
	if !headerTenantAllowed(reqWith(nil, ""), other) {
		t.Fatal("unauthenticated (public widget) requests are not checked here")
	}
}
