// Package config loads runtime configuration from environment variables.
// Every value has a default suitable for local development.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/umutsahin222/insider/internal/domain"
)

// Tracing configures OpenTelemetry, using the standard OTEL_* variable names where they exist.
type Tracing struct {
	// OTLPEndpoint is the OTLP/HTTP collector base URL (e.g. http://jaeger:4318); empty
	// disables exporting, while trace context is still propagated.
	OTLPEndpoint string
	ServiceName  string
	SampleRatio  float64
}

func (e *env) tracing(defaultService string) Tracing {
	t := Tracing{
		OTLPEndpoint: e.str("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ServiceName:  e.str("OTEL_SERVICE_NAME", defaultService),
		SampleRatio:  e.float("TRACE_SAMPLE_RATIO", 1),
	}
	if t.SampleRatio < 0 || t.SampleRatio > 1 {
		e.errs = append(e.errs, errors.New("TRACE_SAMPLE_RATIO must be in [0, 1]"))
	}
	return t
}

// API is the configuration of cmd/api.
type API struct {
	HTTPAddr        string
	DatabaseURL     string
	RedisURL        string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	Rules           domain.Rules
	Tracing         Tracing
	// WebSocketOrigins lists extra browser origins (host patterns) allowed to open /v1/ws.
	WebSocketOrigins []string
}

// LoadAPI reads the API configuration from the environment.
func LoadAPI() (API, error) {
	return loadAPI(os.LookupEnv)
}

func loadAPI(lookup func(string) (string, bool)) (API, error) {
	e := env{lookup: lookup}
	rules := domain.DefaultRules()
	cfg := API{
		HTTPAddr:         e.str("HTTP_ADDR", ":8080"),
		DatabaseURL:      e.str("DATABASE_URL", "postgres://notify:notify@localhost:5432/notify?sslmode=disable"),
		RedisURL:         e.str("REDIS_URL", "redis://localhost:6379/0"),
		LogLevel:         e.level("LOG_LEVEL", slog.LevelInfo),
		ShutdownTimeout:  e.duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		Tracing:          e.tracing("notification-api"),
		WebSocketOrigins: e.list("WS_ALLOWED_ORIGINS"),
		Rules: domain.Rules{
			MaxContentLength: map[domain.Channel]int{
				domain.ChannelEmail: e.int("EMAIL_MAX_LENGTH", rules.MaxContentLength[domain.ChannelEmail]),
				domain.ChannelPush:  e.int("PUSH_MAX_LENGTH", rules.MaxContentLength[domain.ChannelPush]),
			},
			MaxSMSSegments:          e.int("SMS_MAX_SEGMENTS", rules.MaxSMSSegments),
			MaxBatchSize:            e.int("MAX_BATCH_SIZE", rules.MaxBatchSize),
			MaxScheduleAhead:        e.duration("MAX_SCHEDULE_AHEAD", rules.MaxScheduleAhead),
			MaxAttempts:             e.int("MAX_ATTEMPTS", rules.MaxAttempts),
			MaxIdempotencyKeyLength: rules.MaxIdempotencyKeyLength,
		},
	}
	if cfg.Rules.MaxBatchSize < 1 || cfg.Rules.MaxAttempts < 1 {
		e.errs = append(e.errs, errors.New("MAX_BATCH_SIZE and MAX_ATTEMPTS must be positive"))
	}
	// A concatenated SMS numbers its parts with one byte.
	if s := cfg.Rules.MaxSMSSegments; s < 1 || s > 255 {
		e.errs = append(e.errs, fmt.Errorf("SMS_MAX_SEGMENTS must be between 1 and 255, got %d", s))
	}
	e.positive(map[string]time.Duration{
		"SHUTDOWN_TIMEOUT": cfg.ShutdownTimeout, "MAX_SCHEDULE_AHEAD": cfg.Rules.MaxScheduleAhead,
	})
	if err := errors.Join(e.errs...); err != nil {
		return API{}, fmt.Errorf("loading config: %w", err)
	}
	return cfg, nil
}

// Worker is the configuration of cmd/worker.
type Worker struct {
	// HTTPAddr serves the worker's operational endpoints: /metrics, /healthz, /readyz.
	HTTPAddr        string
	DatabaseURL     string
	RedisURL        string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration

	ProviderURL     string
	ProviderTimeout time.Duration

	Channels       []domain.Channel
	Concurrency    int
	ClaimBatchSize int
	Lease          time.Duration
	PollInterval   time.Duration
	ReaperInterval time.Duration

	RateLimitPerSecond float64
	RateLimitBurst     int

	RetryBase time.Duration
	RetryMax  time.Duration

	// Circuit breaker (ADR-002): opens on BreakerFailures consecutive retryable failures, or
	// on a failure ratio of BreakerFailureRatio over at least BreakerMinRequests calls
	// within BreakerWindow.
	BreakerFailures     int
	BreakerFailureRatio float64
	BreakerMinRequests  int
	BreakerWindow       time.Duration
	BreakerOpenTimeout  time.Duration

	Tracing Tracing
}

// LoadWorker reads the worker configuration from the environment.
func LoadWorker() (Worker, error) {
	return loadWorker(os.LookupEnv)
}

func loadWorker(lookup func(string) (string, bool)) (Worker, error) {
	e := env{lookup: lookup}
	cfg := Worker{
		HTTPAddr:        e.str("HTTP_ADDR", ":9091"),
		DatabaseURL:     e.str("DATABASE_URL", "postgres://notify:notify@localhost:5432/notify?sslmode=disable"),
		RedisURL:        e.str("REDIS_URL", "redis://localhost:6379/0"),
		LogLevel:        e.level("LOG_LEVEL", slog.LevelInfo),
		ShutdownTimeout: e.duration("SHUTDOWN_TIMEOUT", 30*time.Second),

		ProviderURL:     e.str("PROVIDER_URL", "http://localhost:8090/send"),
		ProviderTimeout: e.duration("PROVIDER_TIMEOUT", 5*time.Second),

		Channels:       e.channels("CHANNELS", domain.Channels),
		Concurrency:    e.int("WORKER_CONCURRENCY", 8),
		ClaimBatchSize: e.int("CLAIM_BATCH_SIZE", 10),
		Lease:          e.duration("LEASE_DURATION", 60*time.Second),
		PollInterval:   e.duration("POLL_INTERVAL", 200*time.Millisecond),
		ReaperInterval: e.duration("REAPER_INTERVAL", 10*time.Second),

		RateLimitPerSecond: float64(e.int("RATE_LIMIT_PER_SECOND", 100)),
		RateLimitBurst:     e.int("RATE_LIMIT_BURST", 1),

		RetryBase: e.duration("RETRY_BASE", 2*time.Second),
		RetryMax:  e.duration("RETRY_MAX", 5*time.Minute),

		BreakerFailures:     e.int("BREAKER_FAILURES", 10),
		BreakerFailureRatio: e.float("BREAKER_FAILURE_RATIO", 0.5),
		BreakerMinRequests:  e.int("BREAKER_MIN_REQUESTS", 20),
		BreakerWindow:       e.duration("BREAKER_WINDOW", 10*time.Second),
		BreakerOpenTimeout:  e.duration("BREAKER_OPEN_TIMEOUT", 30*time.Second),

		Tracing: e.tracing("notification-worker"),
	}
	for name, v := range map[string]int{
		"WORKER_CONCURRENCY": cfg.Concurrency, "CLAIM_BATCH_SIZE": cfg.ClaimBatchSize,
		"RATE_LIMIT_PER_SECOND": int(cfg.RateLimitPerSecond), "RATE_LIMIT_BURST": cfg.RateLimitBurst,
		"BREAKER_FAILURES": cfg.BreakerFailures, "BREAKER_MIN_REQUESTS": cfg.BreakerMinRequests,
	} {
		if v < 1 {
			e.errs = append(e.errs, fmt.Errorf("%s must be positive", name))
		}
	}
	if cfg.BreakerFailureRatio <= 0 || cfg.BreakerFailureRatio > 1 {
		e.errs = append(e.errs, errors.New("BREAKER_FAILURE_RATIO must be in (0, 1]"))
	}
	// Zero would panic a ticker (REAPER_INTERVAL), busy-loop retries (RETRY_BASE) or time out
	// every send at once (PROVIDER_TIMEOUT).
	e.positive(map[string]time.Duration{
		"SHUTDOWN_TIMEOUT": cfg.ShutdownTimeout, "PROVIDER_TIMEOUT": cfg.ProviderTimeout,
		"LEASE_DURATION": cfg.Lease, "POLL_INTERVAL": cfg.PollInterval, "REAPER_INTERVAL": cfg.ReaperInterval,
		"RETRY_BASE": cfg.RetryBase, "RETRY_MAX": cfg.RetryMax,
		"BREAKER_WINDOW": cfg.BreakerWindow, "BREAKER_OPEN_TIMEOUT": cfg.BreakerOpenTimeout,
	})
	if cfg.RetryMax < cfg.RetryBase {
		e.errs = append(e.errs, errors.New("RETRY_MAX must be at least RETRY_BASE"))
	}
	// A lease shorter than the worst-case send would let the reaper hand live work to
	// another worker and cause duplicate sends.
	if cfg.Lease <= cfg.ProviderTimeout*time.Duration(max(cfg.ClaimBatchSize, 1)) {
		e.errs = append(e.errs, errors.New("LEASE_DURATION must exceed PROVIDER_TIMEOUT × CLAIM_BATCH_SIZE"))
	}
	if err := errors.Join(e.errs...); err != nil {
		return Worker{}, fmt.Errorf("loading config: %w", err)
	}
	return cfg, nil
}

// env collects parse errors so a misconfiguration reports every bad variable at once.
type env struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (e *env) str(key, def string) string {
	if v, ok := e.lookup(key); ok && v != "" {
		return v
	}
	return def
}

func (e *env) int(key string, def int) int {
	v, ok := e.lookup(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return n
}

// list splits a comma-separated variable, dropping empty entries.
func (e *env) list(key string) []string {
	v, ok := e.lookup(key)
	if !ok {
		return nil
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (e *env) float(key string, def float64) float64 {
	v, ok := e.lookup(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return f
}

// positive reports every duration that is zero or negative.
func (e *env) positive(durations map[string]time.Duration) {
	names := make([]string, 0, len(durations))
	for name, d := range durations {
		if d <= 0 {
			names = append(names, name)
		}
	}
	slices.Sort(names) // stable error messages
	for _, name := range names {
		e.errs = append(e.errs, fmt.Errorf("%s must be positive", name))
	}
}

func (e *env) duration(key string, def time.Duration) time.Duration {
	v, ok := e.lookup(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return d
}

func (e *env) channels(key string, def []domain.Channel) []domain.Channel {
	v, ok := e.lookup(key)
	if !ok || v == "" {
		return def
	}
	var out []domain.Channel
	for _, s := range strings.Split(v, ",") {
		ch, err := domain.ParseChannel(strings.TrimSpace(s))
		if err != nil {
			e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		out = append(out, ch)
	}
	return out
}

func (e *env) level(key string, def slog.Level) slog.Level {
	v, ok := e.lookup(key)
	if !ok || v == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return l
}
