package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default() must be valid: %v", err)
	}
}

func TestLoadUsesDefaultsWhenEnvIsEmpty(t *testing.T) {
	got, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got != Default() {
		t.Fatalf("Load(empty env) = %+v, want defaults", got)
	}
}

func TestLoadOverrides(t *testing.T) {
	got, err := Load(env(map[string]string{
		"HTTP_ADDR":                 "127.0.0.1:9090",
		"HTTP_READ_HEADER_TIMEOUT":  "2s",
		"HTTP_READ_TIMEOUT":         "4s",
		"HTTP_WRITE_TIMEOUT":        "8s",
		"HTTP_REQUEST_TIMEOUT":      "6s",
		"HTTP_IDLE_TIMEOUT":         "30s",
		"HTTP_MAX_HEADER_BYTES":     "2048",
		"HTTP_MAX_BODY_BYTES":       "1024",
		"HTTP_SHUTDOWN_TIMEOUT":     "45s",
		"HTTP_SHUTDOWN_DRAIN_DELAY": "5s",
		"WORKER_COUNT":              "2",
		"AUTH_TOKEN":                "s3cret",
		"LOG_LEVEL":                 "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Addr != "127.0.0.1:9090" || got.ReadHeaderTimeout != 2*time.Second ||
		got.MaxHeaderBytes != 2048 || got.MaxBodyBytes != 1024 ||
		got.ShutdownDrainDelay != 5*time.Second || got.WorkerCount != 2 ||
		got.AuthToken != "s3cret" || got.LogLevel != slog.LevelDebug {
		t.Fatalf("unexpected config: %+v", got)
	}
}

func TestLoadReportsEveryParseError(t *testing.T) {
	_, err := Load(env(map[string]string{
		"HTTP_READ_TIMEOUT":     "soon",
		"HTTP_MAX_HEADER_BYTES": "lots",
		"LOG_LEVEL":             "loud",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"HTTP_READ_TIMEOUT", "HTTP_MAX_HEADER_BYTES", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error should mention %s, got: %v", key, err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"bad addr", func(c *Config) { c.Addr = "8080" }, "HTTP_ADDR"},
		{"zero header timeout", func(c *Config) { c.ReadHeaderTimeout = 0 }, "HTTP_READ_HEADER_TIMEOUT"},
		{"negative read timeout", func(c *Config) { c.ReadTimeout = -time.Second }, "HTTP_READ_TIMEOUT"},
		{"zero write timeout", func(c *Config) { c.WriteTimeout = 0 }, "HTTP_WRITE_TIMEOUT"},
		{"zero idle timeout", func(c *Config) { c.IdleTimeout = 0 }, "HTTP_IDLE_TIMEOUT"},
		{"header timeout exceeds read timeout", func(c *Config) { c.ReadHeaderTimeout = time.Minute }, "must not exceed"},
		{"tiny header limit", func(c *Config) { c.MaxHeaderBytes = 10 }, "HTTP_MAX_HEADER_BYTES"},
		{"request timeout not below write timeout", func(c *Config) { c.RequestTimeout = c.WriteTimeout }, "shorter than HTTP_WRITE_TIMEOUT"},
		{"zero body limit", func(c *Config) { c.MaxBodyBytes = 0 }, "HTTP_MAX_BODY_BYTES"},
		{"zero shutdown timeout", func(c *Config) { c.ShutdownTimeout = 0 }, "HTTP_SHUTDOWN_TIMEOUT"},
		{"drain delay above half of shutdown timeout", func(c *Config) { c.ShutdownDrainDelay = c.ShutdownTimeout/2 + time.Second }, "HTTP_SHUTDOWN_DRAIN_DELAY"},
		{"negative drain delay", func(c *Config) { c.ShutdownDrainDelay = -time.Second }, "HTTP_SHUTDOWN_DRAIN_DELAY"},
		{"zero workers", func(c *Config) { c.WorkerCount = 0 }, "WORKER_COUNT"},
		{"zero queue", func(c *Config) { c.WorkerQueueSize = 0 }, "WORKER_QUEUE_SIZE"},
		{"zero concurrency", func(c *Config) { c.BatchConcurrency = 0 }, "WORK_BATCH_CONCURRENCY"},
		{"zero max items", func(c *Config) { c.MaxWorkItems = 0 }, "WORK_MAX_ITEMS"},
		{"zero retention", func(c *Config) { c.JobRetention = 0 }, "WORK_JOB_RETENTION"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q should contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateReportsMultipleViolations(t *testing.T) {
	cfg := Default()
	cfg.WorkerCount = 0
	cfg.MaxBodyBytes = 0
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "WORKER_COUNT") || !strings.Contains(err.Error(), "HTTP_MAX_BODY_BYTES") {
		t.Fatalf("want both violations reported, got %v", err)
	}
}

func TestLogValueNeverContainsAuthToken(t *testing.T) {
	cfg := Default()
	cfg.AuthToken = "super-secret-token"

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "config", cfg)

	out := buf.String()
	if strings.Contains(out, cfg.AuthToken) {
		t.Fatalf("auth token leaked into logs: %s", out)
	}
	if !strings.Contains(out, `"auth_enabled":true`) {
		t.Fatalf("expected auth_enabled flag in %s", out)
	}
}

func TestDrainDelayAtExactlyHalfIsAllowed(t *testing.T) {
	cfg := Default()
	cfg.ShutdownDrainDelay = cfg.ShutdownTimeout / 2
	if err := cfg.Validate(); err != nil {
		t.Fatalf("half of the shutdown timeout should be the inclusive upper bound: %v", err)
	}
}
