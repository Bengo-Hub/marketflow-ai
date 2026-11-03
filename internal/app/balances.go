package app

import (
	"context"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	events "github.com/Bengo-Hub/shared-events"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/config"
	"github.com/bengobox/marketflow-ai/internal/media"
	"github.com/bengobox/marketflow-ai/internal/providerbalance"
	"github.com/bengobox/marketflow-ai/internal/providers"
)

// startBalanceWatch runs the provider balance watcher in the background and returns its stop
// function. The alerts go out on NATS (billing.balance.low) when MF_EVENTS_NATS_URL is set;
// without it the router gate still works and alerts are only logged.
func startBalanceWatch(cfg *config.Config, resolver *providers.Resolver, rdb *redis.Client, logger *zap.Logger) func() {
	// ClaimOnce (the once-per-day alert claim) needs the shared lease client.
	sharedcache.SetLeaseClient(rdb)
	w := &providerbalance.Watcher{
		Readers:   providerbalance.Readers,
		Keys:      providerbalance.KeySource(mediaKeySource(resolver)),
		Settings:  providerSettings(resolver),
		Redis:     rdb,
		Namespace: cfg.Redis.Namespace,
		Claim:     sharedcache.ClaimOnce,
		Balances:  media.ProviderBalances(),
		Logger:    logger.Named("provider-balances"),
	}
	var nc *nats.Conn
	if url := cfg.Events.NATSURL; url != "" {
		conn, err := nats.Connect(url, nats.Name("marketflow-ai"), nats.Timeout(5*time.Second),
			nats.ReconnectWait(2*time.Second), nats.MaxReconnects(-1))
		if err != nil {
			logger.Warn("nats unavailable; provider balance alerts are logged only", zap.Error(err))
		} else if js, err := conn.JetStream(); err != nil {
			logger.Warn("jetstream unavailable; provider balance alerts are logged only", zap.Error(err))
			conn.Close()
		} else {
			nc = conn
			w.Publish = events.NewJetStreamAdapter(js, logger).Publish
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)
	return func() {
		cancel()
		if nc != nil {
			nc.Close()
		}
	}
}

// providerSettings reads media/<provider> from the platform ProviderSettings (the
// low-water marks and Higgsfield's funded amount and date).
func providerSettings(resolver *providers.Resolver) providerbalance.SettingsSource {
	return func(ctx context.Context, provider string) providerbalance.Settings {
		if resolver == nil {
			return nil
		}
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		s, err := resolver.ResolveRaw(rctx, "media", provider)
		if err != nil {
			return nil
		}
		return providerbalance.Settings(s)
	}
}
