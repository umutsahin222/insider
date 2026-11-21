package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/umutsahin222/insider/internal/domain"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadAPI_Defaults(t *testing.T) {
	cfg, err := loadAPI(lookupFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebSocketOrigins != nil {
		t.Errorf("WS origins default = %v, want none", cfg.WebSocketOrigins)
	}
	if cfg.Tracing.OTLPEndpoint != "" || cfg.Tracing.ServiceName != "notification-api" || cfg.Tracing.SampleRatio != 1 {
		t.Errorf("tracing defaults = %+v, want disabled, notification-api, ratio 1", cfg.Tracing)
	}
	if cfg.HTTPAddr != ":8080" || cfg.LogLevel != slog.LevelInfo {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if cfg.Rules.MaxSMSSegments != 1 || cfg.Rules.MaxContentLength[domain.ChannelEmail] != 10_000 || cfg.Rules.MaxBatchSize != 1000 {
		t.Errorf("rules should default to domain.DefaultRules: %+v", cfg.Rules)
	}
}

func TestLoadAPI_Overrides(t *testing.T) {
	cfg, err := loadAPI(lookupFrom(map[string]string{
		"HTTP_ADDR":        ":9090",
		"LOG_LEVEL":        "debug",
		"SMS_MAX_SEGMENTS": "3",
		"SHUTDOWN_TIMEOUT": "3s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9090" || cfg.LogLevel != slog.LevelDebug || cfg.ShutdownTimeout != 3*time.Second {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.Rules.MaxSMSSegments != 3 {
		t.Errorf("SMS_MAX_SEGMENTS = %d", cfg.Rules.MaxSMSSegments)
	}
}

func TestLoadAPI_SMSSegmentsBounds(t *testing.T) {
	for _, v := range []string{"0", "-1", "256"} {
		_, err := loadAPI(lookupFrom(map[string]string{"SMS_MAX_SEGMENTS": v}))
		if err == nil || !strings.Contains(err.Error(), "SMS_MAX_SEGMENTS") {
			t.Errorf("SMS_MAX_SEGMENTS=%s: err = %v, want a bounds error", v, err)
		}
	}
	if _, err := loadAPI(lookupFrom(map[string]string{"SMS_MAX_SEGMENTS": "255"})); err != nil {
		t.Errorf("SMS_MAX_SEGMENTS=255: %v", err)
	}
}

func TestLoadWorker(t *testing.T) {
	cfg, err := loadWorker(lookupFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimitPerSecond != 100 || len(cfg.Channels) != 3 || cfg.Concurrency != 8 || cfg.HTTPAddr != ":9091" || cfg.PollInterval != time.Second || cfg.ClaimBatchSize != 2 ||
		cfg.BreakerFailures != 10 || cfg.BreakerFailureRatio != 0.5 || cfg.BreakerMinRequests != 20 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}

	cfg, err = loadWorker(lookupFrom(map[string]string{"CHANNELS": "sms, push"}))
	if err != nil || len(cfg.Channels) != 2 || cfg.Channels[1] != domain.ChannelPush {
		t.Errorf("CHANNELS: %v, %v", cfg.Channels, err)
	}

	tests := map[string]map[string]string{
		"unknown channel":                     {"CHANNELS": "sms,fax"},
		"zero concurrency":                    {"WORKER_CONCURRENCY": "0"},
		"lease shorter than a batch of sends": {"LEASE_DURATION": "10s", "PROVIDER_TIMEOUT": "5s", "CLAIM_BATCH_SIZE": "10"},
		"breaker ratio above 1":               {"BREAKER_FAILURE_RATIO": "1.5"},
		"breaker ratio not a number":          {"BREAKER_FAILURE_RATIO": "half"},
		"zero breaker window":                 {"BREAKER_WINDOW": "0s"},
		"zero reaper interval":                {"REAPER_INTERVAL": "0s"},
		"zero poll interval":                  {"POLL_INTERVAL": "0s"},
		"zero retry base":                     {"RETRY_BASE": "0s"},
		"retry max below base":                {"RETRY_BASE": "10s", "RETRY_MAX": "5s"},
		"negative provider timeout":           {"PROVIDER_TIMEOUT": "-1s"},
		"zero shutdown timeout":               {"SHUTDOWN_TIMEOUT": "0s"},
		"sample ratio above 1":                {"TRACE_SAMPLE_RATIO": "2"},
	}
	for name, env := range tests {
		if _, err := loadWorker(lookupFrom(env)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoadAPI_ReportsEveryInvalidVariable(t *testing.T) {
	_, err := loadAPI(lookupFrom(map[string]string{
		"SMS_MAX_SEGMENTS": "lots",
		"SHUTDOWN_TIMEOUT": "soon",
		"LOG_LEVEL":        "chatty",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, key := range []string{"SMS_MAX_SEGMENTS", "SHUTDOWN_TIMEOUT", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not mention %s", err, key)
		}
	}
}
