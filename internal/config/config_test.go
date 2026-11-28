package config

import "testing"

// The credit client must never be off because only the shared key is set (28 November 2026: no
// chat, Ask or Studio turn was charged for any tenant while MF_SECURITY_SUBSCRIPTIONS_API_KEY was
// unset).
func TestSubscriptionsKeyFallsBackToInternalKey(t *testing.T) {
	t.Setenv("POSTGRES_URL", "postgres://x")
	t.Setenv("INTERNAL_SERVICE_KEY", "shared")
	t.Setenv("MF_SECURITY_SUBSCRIPTIONS_API_KEY", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Security.SubscriptionsKey != "shared" {
		t.Fatalf("subscriptions key %q, want the internal key", cfg.Security.SubscriptionsKey)
	}
	t.Setenv("MF_SECURITY_SUBSCRIPTIONS_API_KEY", "dedicated")
	if cfg, _ = Load(); cfg.Security.SubscriptionsKey != "dedicated" {
		t.Fatalf("dedicated key must win, got %q", cfg.Security.SubscriptionsKey)
	}
}
