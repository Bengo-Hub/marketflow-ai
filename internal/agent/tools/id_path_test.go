package tools

import "testing"

// Security audit 2026-11-10 (MF #8): model-supplied IDs went straight into marketflow-api URL
// paths called with the platform service key, so an ID like "../admin/providers/resolve?..."
// could reach an admin route. Only a UUID ever reaches the path.
func TestIDPath(t *testing.T) {
	ok := "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	if got := idPath(ok); got != ok {
		t.Fatalf("uuid kept: %q", got)
	}
	if got := idPath("  3F2504E0-4F89-11D3-9A0C-0305E82C3301 "); got != ok {
		t.Fatalf("canonical form: %q", got)
	}
	for _, bad := range []string{"", "../admin/providers/resolve?type=meta_ads", "abc/def", "x?y=1", "%2e%2e"} {
		if got := idPath(bad); got != "invalid-id" {
			t.Errorf("%q -> %q", bad, got)
		}
	}
}

// MF #8: filter keys could override tenant scoping or the row cap with names other than the
// exact "tenant_id" (tenant, tenantId, all_tenants, limit, ...).
func TestSafeFilterKey(t *testing.T) {
	for _, k := range []string{"status", "course_id", "courseId", "created_after", "q"} {
		if !safeFilterKey(k) {
			t.Errorf("%q should pass", k)
		}
	}
	for _, k := range []string{"tenant_id", "TENANT_ID", "tenant", "tenantId", "x_tenant_id", "all_tenants",
		"tenant_slug", "TenantSlug", "limit", "LIMIT", "all", "include_deleted", "status&tenant_id=x", "a b", "", "outlet-id"} {
		if safeFilterKey(k) {
			t.Errorf("%q should be dropped", k)
		}
	}
}
