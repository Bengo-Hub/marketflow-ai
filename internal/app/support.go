package app

import (
	"context"
	"fmt"
	"net/http"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/config"
	"github.com/bengobox/marketflow-ai/internal/providers"
	"github.com/bengobox/marketflow-ai/internal/support"
)

// newSupportRouter wires the widget's support routing (owner, 2026-11-28): the classifier on the
// shared decide layer, settings from the vera_support ProviderSetting rows, the plan gate on
// auth-client's EntitlementClient, tickets through ticketing-api's internal create, the staff
// notice through notifications-api, and admin phones and tenant details from auth-api.
func newSupportRouter(cfg *config.Config, resolver *providers.Resolver, rdb *redis.Client, decider support.Decider, logger *zap.Logger) (*support.Router, *support.OTPValidator) {
	key := cfg.App.InternalServiceKey
	httpClient := &http.Client{Timeout: 10 * time.Second}
	subsKey := cfg.Security.SubscriptionsKey
	if subsKey == "" {
		subsKey = key
	}
	var settings support.SettingsSource
	if resolver != nil {
		settings = resolver.ResolveRaw
	}
	router := support.NewRouter(support.Config{
		Classifier:   support.NewClassifier(decider),
		Settings:     settings,
		Tenants:      supportTenantLookup(sharedcache.New(rdb, logger), cfg.AI.AuthAPIURL),
		PlatformSlug: cfg.App.PlatformTenantSlug,
		Plans:        authclient.NewEntitlementClient(cfg.Security.SubscriptionsURL, subsKey),
		Tickets:      &support.Ticketing{BaseURL: cfg.AI.TicketingURL, ServiceKey: key, HTTP: httpClient},
		Messages:     &support.Notifier{BaseURL: cfg.AI.NotificationsURL, ServiceKey: key, HTTP: httpClient},
		Admins:       &support.AdminReach{BaseURL: cfg.AI.AuthAPIURL, ServiceKey: key, HTTP: httpClient},
		Logger:       logger,
	})
	return router, &support.OTPValidator{BaseURL: cfg.AI.AuthAPIURL, ServiceKey: key, HTTP: httpClient}
}

// supportTenantLookup reads a tenant from auth-api's public tenant (cached in Redis by the shared
// cache module) and applies the shared support contact rule (cache.ResolveSupportContact): the
// support email and WhatsApp number from metadata "support", else the contact email and phone.
func supportTenantLookup(aside *sharedcache.Aside, authURL string) support.TenantLookup {
	return func(ctx context.Context, slug string) (support.Tenant, error) {
		d, err := sharedcache.GetTenantDetails(ctx, aside, authURL, slug, 0)
		if err != nil {
			return support.Tenant{}, err
		}
		id, err := uuid.Parse(d.ID)
		if err != nil {
			return support.Tenant{}, fmt.Errorf("tenant %q has no usable id", slug)
		}
		contact := sharedcache.ResolveSupportContact(d)
		return support.Tenant{ID: id, Slug: d.Slug, Name: d.Name, SupportEmail: contact.Email, SupportWhatsApp: contact.WhatsApp}, nil
	}
}
