// Package config loads and validates process configuration.
//
// Configuration is read from environment variables because that is the
// lowest-common-denominator interface for containers and orchestrators. All
// validation happens once at startup so that a bad value fails the process
// immediately instead of surfacing as odd behaviour under load.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"
)

// Config holds every tunable of the service. It is a plain value type: it is
// built once in main, validated, and then passed by value to constructors.
type Config struct {
	// Addr is the TCP address the HTTP server listens on.
	Addr string

	// Server-level timeouts. See docs/operational-guide.md for the reasoning
	// behind each default.
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int

	// RequestTimeout is the per-request deadline attached to the request
	// context for API routes. It must be shorter than WriteTimeout so the
	// handler has time to write a timeout response before the connection's
	// write deadline fires.
	RequestTimeout time.Duration

	// MaxBodyBytes bounds JSON request bodies.
	MaxBodyBytes int64

	// ShutdownTimeout bounds the whole graceful shutdown sequence.
	ShutdownTimeout time.Duration
	// ShutdownDrainDelay is how long the server keeps accepting traffic after
	// readiness has flipped to false, so load balancers can notice.
	ShutdownDrainDelay time.Duration

	// Work engine settings.
	MaxWorkItems     int
	MaxWorkItemDelay time.Duration
	BatchConcurrency int
	WorkerCount      int
	WorkerQueueSize  int
	JobRetention     int

	// AuthToken enables the placeholder bearer-token check when non-empty.
	// It is NOT production authentication; see docs/operational-guide.md.
	AuthToken string

	LogLevel slog.Level
}

// Default returns a valid configuration suitable for local development.
func Default() Config {
	return Config{
		Addr:               ":8080",
		ReadHeaderTimeout:  5 * time.Second,
		ReadTimeout:        10 * time.Second,
		WriteTimeout:       15 * time.Second,
		IdleTimeout:        60 * time.Second,
		MaxHeaderBytes:     1 << 20,
		RequestTimeout:     10 * time.Second,
		MaxBodyBytes:       64 << 10,
		ShutdownTimeout:    30 * time.Second,
		ShutdownDrainDelay: 0,
		MaxWorkItems:       1000,
		MaxWorkItemDelay:   time.Second,
		BatchConcurrency:   8,
		WorkerCount:        4,
		WorkerQueueSize:    64,
		JobRetention:       1000,
		LogLevel:           slog.LevelInfo,
	}
}

// Load builds a Config from getenv (normally os.Getenv), applying defaults for
// unset variables and validating the result. Every parse error is reported at
// once so an operator can fix the whole environment in one pass.
func Load(getenv func(string) string) (Config, error) {
	cfg := Default()
	var errs []error

	str := func(key string, dst *string) {
		if v := getenv(key); v != "" {
			*dst = v
		}
	}
	dur := func(key string, dst *time.Duration) {
		v := getenv(key)
		if v == "" {
			return
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return
		}
		*dst = d
	}
	integer := func(key string, dst *int) {
		v := getenv(key)
		if v == "" {
			return
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return
		}
		*dst = n
	}
	integer64 := func(key string, dst *int64) {
		v := getenv(key)
		if v == "" {
			return
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return
		}
		*dst = n
	}

	str("HTTP_ADDR", &cfg.Addr)
	dur("HTTP_READ_HEADER_TIMEOUT", &cfg.ReadHeaderTimeout)
	dur("HTTP_READ_TIMEOUT", &cfg.ReadTimeout)
	dur("HTTP_WRITE_TIMEOUT", &cfg.WriteTimeout)
	dur("HTTP_IDLE_TIMEOUT", &cfg.IdleTimeout)
	integer("HTTP_MAX_HEADER_BYTES", &cfg.MaxHeaderBytes)
	dur("HTTP_REQUEST_TIMEOUT", &cfg.RequestTimeout)
	integer64("HTTP_MAX_BODY_BYTES", &cfg.MaxBodyBytes)
	dur("HTTP_SHUTDOWN_TIMEOUT", &cfg.ShutdownTimeout)
	dur("HTTP_SHUTDOWN_DRAIN_DELAY", &cfg.ShutdownDrainDelay)
	integer("WORK_MAX_ITEMS", &cfg.MaxWorkItems)
	dur("WORK_MAX_ITEM_DELAY", &cfg.MaxWorkItemDelay)
	integer("WORK_BATCH_CONCURRENCY", &cfg.BatchConcurrency)
	integer("WORKER_COUNT", &cfg.WorkerCount)
	integer("WORKER_QUEUE_SIZE", &cfg.WorkerQueueSize)
	integer("WORK_JOB_RETENTION", &cfg.JobRetention)
	str("AUTH_TOKEN", &cfg.AuthToken)
	if v := getenv("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
		}
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("parse environment: %w", errors.Join(errs...))
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks each field and the relationships between fields. It reports
// every violation, not just the first.
func (c Config) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}

	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		errs = append(errs, fmt.Errorf("HTTP_ADDR %q is not a host:port address: %w", c.Addr, err))
	}

	// A zero timeout in net/http means "no timeout". For a server exposed to
	// untrusted clients that is almost never what you want, so it is rejected
	// rather than silently accepted.
	check(c.ReadHeaderTimeout > 0, "HTTP_READ_HEADER_TIMEOUT must be positive (zero disables slow-header protection)")
	check(c.ReadTimeout > 0, "HTTP_READ_TIMEOUT must be positive")
	check(c.WriteTimeout > 0, "HTTP_WRITE_TIMEOUT must be positive")
	check(c.IdleTimeout > 0, "HTTP_IDLE_TIMEOUT must be positive")
	check(c.ReadHeaderTimeout <= c.ReadTimeout,
		"HTTP_READ_HEADER_TIMEOUT (%s) must not exceed HTTP_READ_TIMEOUT (%s)", c.ReadHeaderTimeout, c.ReadTimeout)
	check(c.MaxHeaderBytes >= 1024, "HTTP_MAX_HEADER_BYTES must be at least 1024, got %d", c.MaxHeaderBytes)

	check(c.RequestTimeout > 0, "HTTP_REQUEST_TIMEOUT must be positive")
	check(c.RequestTimeout < c.WriteTimeout,
		"HTTP_REQUEST_TIMEOUT (%s) must be shorter than HTTP_WRITE_TIMEOUT (%s) so a timeout response can still be written",
		c.RequestTimeout, c.WriteTimeout)
	check(c.MaxBodyBytes > 0, "HTTP_MAX_BODY_BYTES must be positive, got %d", c.MaxBodyBytes)

	check(c.ShutdownTimeout > 0, "HTTP_SHUTDOWN_TIMEOUT must be positive")
	check(c.ShutdownDrainDelay >= 0, "HTTP_SHUTDOWN_DRAIN_DELAY must not be negative")
	// The drain delay is spent from the drain phase, which is 80% of the
	// shutdown budget. Capping the delay at half the budget guarantees the
	// requests still in flight keep a meaningful window to finish before they
	// are cancelled.
	check(c.ShutdownDrainDelay <= c.ShutdownTimeout/2,
		"HTTP_SHUTDOWN_DRAIN_DELAY (%s) must not exceed half of HTTP_SHUTDOWN_TIMEOUT (%s)", c.ShutdownDrainDelay, c.ShutdownTimeout)

	check(c.MaxWorkItems > 0, "WORK_MAX_ITEMS must be positive, got %d", c.MaxWorkItems)
	check(c.MaxWorkItemDelay >= 0, "WORK_MAX_ITEM_DELAY must not be negative")
	check(c.BatchConcurrency > 0, "WORK_BATCH_CONCURRENCY must be positive, got %d", c.BatchConcurrency)
	check(c.WorkerCount > 0, "WORKER_COUNT must be positive, got %d", c.WorkerCount)
	check(c.WorkerQueueSize > 0, "WORKER_QUEUE_SIZE must be positive, got %d", c.WorkerQueueSize)
	check(c.JobRetention > 0, "WORK_JOB_RETENTION must be positive, got %d", c.JobRetention)

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return nil
}

// LogValue implements slog.LogValuer so the configuration can be logged at
// startup without ever printing the auth token.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("addr", c.Addr),
		slog.String("read_header_timeout", c.ReadHeaderTimeout.String()),
		slog.String("read_timeout", c.ReadTimeout.String()),
		slog.String("write_timeout", c.WriteTimeout.String()),
		slog.String("idle_timeout", c.IdleTimeout.String()),
		slog.Int("max_header_bytes", c.MaxHeaderBytes),
		slog.String("request_timeout", c.RequestTimeout.String()),
		slog.Int64("max_body_bytes", c.MaxBodyBytes),
		slog.String("shutdown_timeout", c.ShutdownTimeout.String()),
		slog.String("shutdown_drain_delay", c.ShutdownDrainDelay.String()),
		slog.Int("worker_count", c.WorkerCount),
		slog.Int("worker_queue_size", c.WorkerQueueSize),
		slog.Bool("auth_enabled", c.AuthToken != ""),
		slog.String("log_level", c.LogLevel.String()),
	)
}
