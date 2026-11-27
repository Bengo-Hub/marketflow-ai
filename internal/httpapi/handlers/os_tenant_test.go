package handlers

import (
	"context"
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
	if headerTenantAllowed(reqWith(nil, ""), other) {
		t.Fatal("requests without claims are refused (the public widget path skips this check)")
	}
}

// Security audit 2026-11-10: "superuser" is a role inside one tenant, not platform authority.
// Only auth-api's is_platform_owner claim (or a service key) may name another tenant.
func TestTenantSuperuserCannotActForAnotherTenant(t *testing.T) {
	own, other := uuid.New(), uuid.New()
	tenantSuper := &authclient.Claims{TenantID: own.String(), Roles: []string{"superuser"}}
	if _, status, _ := tenantFromAuth(reqWith(tenantSuper, other.String())); status != http.StatusForbidden {
		t.Fatalf("a tenant superuser naming another tenant must be 403, got %d", status)
	}
	if headerTenantAllowed(reqWith(tenantSuper, ""), other) {
		t.Fatal("a tenant superuser must not use another tenant's header")
	}
	owner := &authclient.Claims{TenantID: own.String(), IsPlatformOwner: true}
	if id, status, _ := tenantFromAuth(reqWith(owner, other.String())); status != 0 || id != other {
		t.Fatalf("platform owner naming a tenant: %v %d", id, status)
	}
}

// A platform owner on a tenant's pages sends only X-Tenant-Slug; the AI must act on that
// tenant, never the owner's own (the Suggest button once filled Codevertex into Hadia).
func TestTenantFromAuthSlug(t *testing.T) {
	own, hadia := uuid.New(), uuid.New()
	SetTenantSlugResolver(func(_ context.Context, slug string) (uuid.UUID, bool) {
		return hadia, slug == "hadia"
	})
	defer SetTenantSlugResolver(nil)
	withSlug := func(c *authclient.Claims, slug string) *http.Request {
		r := reqWith(c, "")
		r.Header.Set("X-Tenant-Slug", slug)
		return r
	}
	owner := &authclient.Claims{TenantID: own.String(), IsPlatformOwner: true}
	if id, status, _ := tenantFromAuth(withSlug(owner, "hadia")); status != 0 || id != hadia {
		t.Fatalf("owner on hadia: %v %d", id, status)
	}
	if _, status, _ := tenantFromAuth(withSlug(owner, "nope")); status != http.StatusNotFound {
		t.Fatalf("unknown slug: %d", status)
	}
	// A tenant user's slug header never moves them to another tenant.
	user := &authclient.Claims{TenantID: own.String()}
	if id, status, _ := tenantFromAuth(withSlug(user, "hadia")); status != 0 || id != own {
		t.Fatalf("user slug: %v %d", id, status)
	}
}
