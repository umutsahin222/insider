// Command api serves the notification HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	apispec "github.com/umutsahin222/insider/api"
	"github.com/umutsahin222/insider/internal/config"
	"github.com/umutsahin222/insider/internal/events"
	"github.com/umutsahin222/insider/internal/httpapi"
	"github.com/umutsahin222/insider/internal/metrics"
	"github.com/umutsahin222/insider/internal/observability"
	"github.com/umutsahin222/insider/internal/store/postgres"
)

const (
	queueStatsTimeout = 2 * time.Second
	queueStatsCache   = time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("api exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadAPI()
	if err != nil {
		return err
	}
	log := observability.NewLogger(os.Stdout, cfg.LogLevel).With("service", "api")
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tracing, err := observability.NewTracing(ctx, observability.TracingConfig{
		ServiceName: cfg.Tracing.ServiceName, OTLPEndpoint: cfg.Tracing.OTLPEndpoint, SampleRatio: cfg.Tracing.SampleRatio,
	})
	if err != nil {
		return err
	}
	defer flushTraces(tracing, log)

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
	// Apply context deadlines to Redis I/O (go-redis ignores them by default and falls back to
	// its 5 s read timeout), so readiness checks, event publishes and rate-limit waits stop
	// when their contexts say so.
	redisOpts.ContextTimeoutEnabled = true
	rdb := redis.NewClient(redisOpts)
	defer func() { _ = rdb.Close() }()

	// Queue depth and the status event outbox backlog are exported by the API (not the
	// workers), read from the database at scrape time with a short cache; see ADR-007.
	reg := metrics.NewRegistry()
	reg.MustRegister(metrics.NewQueueCollector(store, queueStatsTimeout, queueStatsCache, log.With("component", "queue_metrics")))
	httpMetrics := metrics.NewHTTPMetrics(reg)

	// One reader of the event stream per process fans status events out to WebSocket clients
	// (ADR-012). It stops with ctx (on SIGTERM), which closes every stream with "try again
	// later". The API publishes no events: the database records them with every transition.
	hub := events.NewHub(redisOpts, log.With("component", "event_hub"), httpMetrics)
	hubDone := make(chan struct{})
	go func() { hub.Run(ctx); close(hubDone) }()
	defer func() { <-hubDone; _ = hub.Close() }()

	handler := httpapi.NewRouter(httpapi.Deps{
		Store:  store,
		Rules:  cfg.Rules,
		Logger: log,
		Checks: map[string]httpapi.Check{
			"postgres": store.Ping,
			"redis":    func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
		},
		Metrics:          httpMetrics,
		MetricsHandler:   metrics.MetricsHandler(reg),
		Stats:            store,
		Stream:           hub,
		WebSocketOrigins: cfg.WebSocketOrigins,
		Tracing:          tracing,
		OpenAPI:          apispec.OpenAPI,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr, "tracing", cfg.Tracing.OTLPEndpoint != "")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("serving http: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return nil
}

// flushTraces exports buffered spans before exit.
func flushTraces(t *observability.Tracing, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := t.Shutdown(ctx); err != nil {
		log.Warn("flushing traces failed", "error", err)
	}
}
