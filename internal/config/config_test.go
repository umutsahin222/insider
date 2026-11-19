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
	if cfg.HTTPAddr != ":8080" || cfg.LogLevel != slog.LevelInfo {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if cfg.Rules.MaxContentLength[domain.ChannelSMS] != 160 || cfg.Rules.MaxBatchSize != 1000 {
		t.Errorf("rules should default to domain.DefaultRules: %+v", cfg.Rules)
	}
}

func TestLoadAPI_Overrides(t *testing.T) {
	cfg, err := loadAPI(lookupFrom(map[string]string{
		"HTTP_ADDR":        ":9090",
		"LOG_LEVEL":        "debug",
		"SMS_MAX_LENGTH":   "320",
		"SHUTDOWN_TIMEOUT": "3s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9090" || cfg.LogLevel != slog.LevelDebug || cfg.ShutdownTimeout != 3*time.Second {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.Rules.MaxContentLength[domain.ChannelSMS] != 320 {
		t.Errorf("SMS_MAX_LENGTH = %d", cfg.Rules.MaxContentLength[domain.ChannelSMS])
	}
}

func TestLoadWorker(t *testing.T) {
	cfg, err := loadWorker(lookupFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimitPerSecond != 100 || len(cfg.Channels) != 3 || cfg.Concurrency != 8 || cfg.HTTPAddr != ":9091" {
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
	}
	for name, env := range tests {
		if _, err := loadWorker(lookupFrom(env)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoadAPI_ReportsEveryInvalidVariable(t *testing.T) {
	_, err := loadAPI(lookupFrom(map[string]string{
		"SMS_MAX_LENGTH":   "lots",
		"SHUTDOWN_TIMEOUT": "soon",
		"LOG_LEVEL":        "chatty",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, key := range []string{"SMS_MAX_LENGTH", "SHUTDOWN_TIMEOUT", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not mention %s", err, key)
		}
	}
}
