// Package config loads runtime configuration from environment variables.
// Every value has a default suitable for local development.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/umutsahin222/insider/internal/domain"
)

// API is the configuration of cmd/api.
type API struct {
	HTTPAddr        string
	DatabaseURL     string
	RedisURL        string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	Rules           domain.Rules
}

// LoadAPI reads the API configuration from the environment.
func LoadAPI() (API, error) {
	return loadAPI(os.LookupEnv)
}

func loadAPI(lookup func(string) (string, bool)) (API, error) {
	e := env{lookup: lookup}
	rules := domain.DefaultRules()
	cfg := API{
		HTTPAddr:        e.str("HTTP_ADDR", ":8080"),
		DatabaseURL:     e.str("DATABASE_URL", "postgres://notify:notify@localhost:5432/notify?sslmode=disable"),
		RedisURL:        e.str("REDIS_URL", "redis://localhost:6379/0"),
		LogLevel:        e.level("LOG_LEVEL", slog.LevelInfo),
		ShutdownTimeout: e.duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		Rules: domain.Rules{
			MaxContentLength: map[domain.Channel]int{
				domain.ChannelSMS:   e.int("SMS_MAX_LENGTH", rules.MaxContentLength[domain.ChannelSMS]),
				domain.ChannelEmail: e.int("EMAIL_MAX_LENGTH", rules.MaxContentLength[domain.ChannelEmail]),
				domain.ChannelPush:  e.int("PUSH_MAX_LENGTH", rules.MaxContentLength[domain.ChannelPush]),
			},
			MaxBatchSize:            e.int("MAX_BATCH_SIZE", rules.MaxBatchSize),
			MaxScheduleAhead:        e.duration("MAX_SCHEDULE_AHEAD", rules.MaxScheduleAhead),
			MaxAttempts:             e.int("MAX_ATTEMPTS", rules.MaxAttempts),
			MaxIdempotencyKeyLength: rules.MaxIdempotencyKeyLength,
		},
	}
	if cfg.Rules.MaxBatchSize < 1 || cfg.Rules.MaxAttempts < 1 {
		e.errs = append(e.errs, errors.New("MAX_BATCH_SIZE and MAX_ATTEMPTS must be positive"))
	}
	if err := errors.Join(e.errs...); err != nil {
		return API{}, fmt.Errorf("loading config: %w", err)
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
