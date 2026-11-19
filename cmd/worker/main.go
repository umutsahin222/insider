// Command worker delivers queued notifications: one worker pool per channel plus a lease reaper.
// It also serves /metrics, /healthz and /readyz on HTTP_ADDR for scraping and orchestration.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/umutsahin222/insider/internal/config"
	"github.com/umutsahin222/insider/internal/delivery"
	"github.com/umutsahin222/insider/internal/metrics"
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

	reg := metrics.NewRegistry()
	deliveryMetrics := metrics.NewDeliveryMetrics(reg)
	limiter := ratelimit.New(rdb, cfg.RateLimitPerSecond, cfg.RateLimitBurst)
	sender := provider.New(cfg.ProviderURL, cfg.ProviderTimeout)

	ops := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: opsHandler(metrics.MetricsHandler(reg), map[string]func(context.Context) error{
			"postgres": store.Ping,
			"redis":    func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	opsErr := make(chan error, 1)
	go func() {
		if err := ops.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			opsErr <- err
		}
		close(opsErr)
	}()

	var wg sync.WaitGroup
	for _, ch := range cfg.Channels {
		w := delivery.NewWorker(delivery.Config{
			Channel:      ch,
			Concurrency:  cfg.Concurrency,
			BatchSize:    cfg.ClaimBatchSize,
			Lease:        cfg.Lease,
			PollInterval: cfg.PollInterval,
			SendTimeout:  cfg.ProviderTimeout,
			Retry:        delivery.RetryPolicy{Base: cfg.RetryBase, Max: cfg.RetryMax},
			Breaker: delivery.BreakerPolicy{
				ConsecutiveFailures: uint32(cfg.BreakerFailures),
				FailureRatio:        cfg.BreakerFailureRatio,
				MinRequests:         uint32(cfg.BreakerMinRequests),
				Window:              cfg.BreakerWindow,
				OpenTimeout:         cfg.BreakerOpenTimeout,
			},
		}, store, sender, limiter, deliveryMetrics, log)
		wg.Go(func() { w.Run(ctx) })
	}
	wg.Go(func() {
		delivery.RunReaper(ctx, store, cfg.ReaperInterval, deliveryMetrics, log.With("component", "reaper"))
	})

	log.Info("worker running", "channels", cfg.Channels, "provider_url", cfg.ProviderURL,
		"rate_limit_per_second", cfg.RateLimitPerSecond, "ops_addr", cfg.HTTPAddr)
	select {
	case <-ctx.Done():
	case err := <-opsErr:
		stop() // the workers must not run unobservable
		wg.Wait()
		return fmt.Errorf("serving ops endpoints: %w", err)
	}
	log.Info("shutting down; finishing in-flight deliveries", "timeout", cfg.ShutdownTimeout.String())

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	var result error
	select {
	case <-done:
	case <-time.After(cfg.ShutdownTimeout):
		// Anything still claimed is recovered by the reaper once its lease expires.
		result = fmt.Errorf("shutdown timed out after %s", cfg.ShutdownTimeout)
	}
	// Keep /metrics up until the workers are done so the final counts can be scraped.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ops.Shutdown(shutdownCtx)
	return result
}

// opsHandler serves the worker's operational endpoints.
func opsHandler(metricsHandler http.Handler, checks map[string]func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metricsHandler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		status, code := "ok", http.StatusOK
		results := make(map[string]string, len(checks))
		for name, check := range checks {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			err := check(ctx)
			cancel()
			if err != nil {
				results[name], status, code = "unavailable", "unavailable", http.StatusServiceUnavailable
				continue
			}
			results[name] = "ok"
		}
		writeJSON(w, code, map[string]any{"status": status, "checks": results})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
