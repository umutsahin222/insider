// Command worker delivers queued notifications: one worker pool per channel plus a lease reaper.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/umutsahin222/insider/internal/config"
	"github.com/umutsahin222/insider/internal/delivery"
	"github.com/umutsahin222/insider/internal/observability"
	"github.com/umutsahin222/insider/internal/provider"
	"github.com/umutsahin222/insider/internal/ratelimit"
	"github.com/umutsahin222/insider/internal/store/postgres"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	log := observability.NewLogger(os.Stdout, cfg.LogLevel).With("service", "worker")
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := postgres.New(pool)

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parsing REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(redisOpts)
	defer func() { _ = rdb.Close() }()

	limiter := ratelimit.New(rdb, cfg.RateLimitPerSecond, cfg.RateLimitBurst)
	sender := provider.New(cfg.ProviderURL, cfg.ProviderTimeout)

	var wg sync.WaitGroup
	for _, ch := range cfg.Channels {
		w := delivery.NewWorker(delivery.Config{
			Channel:            ch,
			Concurrency:        cfg.Concurrency,
			BatchSize:          cfg.ClaimBatchSize,
			Lease:              cfg.Lease,
			PollInterval:       cfg.PollInterval,
			SendTimeout:        cfg.ProviderTimeout,
			Retry:              delivery.RetryPolicy{Base: cfg.RetryBase, Max: cfg.RetryMax},
			BreakerFailures:    uint32(cfg.BreakerFailures),
			BreakerOpenTimeout: cfg.BreakerOpenTimeout,
		}, store, sender, limiter, log)
		wg.Go(func() { w.Run(ctx) })
	}
	wg.Go(func() { delivery.RunReaper(ctx, store, cfg.ReaperInterval, log.With("component", "reaper")) })

	log.Info("worker running", "channels", cfg.Channels, "provider_url", cfg.ProviderURL,
		"rate_limit_per_second", cfg.RateLimitPerSecond)
	<-ctx.Done()
	log.Info("shutting down; finishing in-flight deliveries", "timeout", cfg.ShutdownTimeout.String())

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(cfg.ShutdownTimeout):
		// Anything still claimed is recovered by the reaper once its lease expires.
		return fmt.Errorf("shutdown timed out after %s", cfg.ShutdownTimeout)
	}
}
