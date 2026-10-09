// Package server assembles the service: it builds the dependency graph, wraps
// it in middleware, configures net/http.Server, and owns the process lifecycle
// including graceful shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/michael-emmanuel/go-production-http-server/internal/config"
	"github.com/michael-emmanuel/go-production-http-server/internal/domain"
	"github.com/michael-emmanuel/go-production-http-server/internal/health"
	"github.com/michael-emmanuel/go-production-http-server/internal/httpapi"
	"github.com/michael-emmanuel/go-production-http-server/internal/metrics"
	"github.com/michael-emmanuel/go-production-http-server/internal/worker"
)

// Option customizes a Server. Options exist for the few seams that tests and
// build systems need; production wiring uses none of them except WithVersion.
type Option func(*options)

type options struct {
	work      domain.WorkFunc
	version   string
	drainWait func(ctx context.Context, d time.Duration) error
}

// WithWorkFunc replaces the simulated work function. Tests use it to make work
// block on channels so timing is controlled by the test, not by sleeps.
func WithWorkFunc(fn domain.WorkFunc) Option { return func(o *options) { o.work = fn } }

// WithVersion sets the version reported by /api/v1/info.
func WithVersion(v string) Option { return func(o *options) { o.version = v } }

// WithDrainWait replaces the pause between failing readiness and closing the
// listener. Tests use it to observe the "not ready but still listening" window
// deterministically instead of racing a real timer.
func WithDrainWait(fn func(ctx context.Context, d time.Duration) error) Option {
	return func(o *options) { o.drainWait = fn }
}

// Server is the assembled service.
type Server struct {
	cfg config.Config
	log *slog.Logger

	http    *http.Server
	health  *health.Checker
	metrics *metrics.Metrics
	pool    *worker.Pool

	// baseCancel cancels the context that every request context derives from.
	// It is how shutdown propagates cancellation into in-flight handlers when
	// graceful draining runs out of time. It takes a cause so handlers can
	// tell "the server is shutting down" apart from "the client went away".
	baseCancel context.CancelCauseFunc
	drainWait  func(ctx context.Context, d time.Duration) error
}

// New validates cfg and wires the service. Nothing is listening yet.
func New(cfg config.Config, log *slog.Logger, opts ...Option) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	o := options{drainWait: sleepContext}
	for _, opt := range opts {
		opt(&o)
	}

	m := metrics.New()

	pool, err := worker.NewPool(worker.Config{
		Workers:   cfg.WorkerCount,
		QueueSize: cfg.WorkerQueueSize,
		Logger:    log,
	})
	if err != nil {
		return nil, fmt.Errorf("create worker pool: %w", err)
	}

	// From here on the pool's goroutines are running. Every failure path must
	// stop them, or a construction error would leak them.
	abort := func() {
		expired, cancel := context.WithCancel(context.Background())
		cancel() // already expired: idle workers exit at once, nobody waits
		_ = pool.Shutdown(expired)
	}

	svc, err := domain.NewService(domain.ServiceConfig{
		MaxItems:         cfg.MaxWorkItems,
		MaxItemDelay:     cfg.MaxWorkItemDelay,
		BatchConcurrency: cfg.BatchConcurrency,
	}, domain.ServiceDeps{
		Store:    domain.NewStore(cfg.JobRetention),
		Executor: pool,
		Work:     o.work,
		Logger:   log,
	})
	if err != nil {
		abort()
		return nil, fmt.Errorf("create work service: %w", err)
	}

	m.RegisterGauge("worker_queue_depth", func() int64 { return int64(pool.QueueDepth()) })
	m.RegisterGauge("work_items_processed_total", func() int64 { return svc.Stats().ItemsProcessed })
	m.RegisterGauge("jobs_succeeded_total", func() int64 { return svc.Stats().JobsSucceeded })
	m.RegisterGauge("jobs_failed_total", func() int64 { return svc.Stats().JobsFailed })
	m.RegisterGauge("jobs_canceled_total", func() int64 { return svc.Stats().JobsCanceled })
	m.RegisterGauge("jobs_stored", func() int64 { return svc.Stats().JobsStored })

	handler, err := httpapi.NewHandler(httpapi.Deps{
		Work:         svc,
		Logger:       log,
		MaxBodyBytes: cfg.MaxBodyBytes,
		Version:      o.version,
	})
	if err != nil {
		abort()
		return nil, fmt.Errorf("create handler: %w", err)
	}

	checker := health.New()

	// Policy applied to API routes only. Order: authenticate first so
	// unauthenticated callers cost as little as possible, then check the media
	// type, then start the deadline clock for the actual work.
	apiPolicy := func(next http.Handler) http.Handler {
		return Chain(next, Auth(cfg.AuthToken), RequireJSON(), RequestTimeout(cfg.RequestTimeout))
	}
	routes := httpapi.Routes(httpapi.RouteDeps{
		Handler: handler,
		Health:  checker,
		Metrics: m.Handler(),
		API:     apiPolicy,
	})

	root := newRootHandler(routes, log, m)

	// Every request context is derived from baseCtx (via BaseContext below),
	// so cancelling it reaches every in-flight handler.
	baseCtx, baseCancel := context.WithCancelCause(context.Background())

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           root,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ConnState:         m.ConnState,
		// Route net/http's internal error output (accept failures, TLS
		// handshake errors) through the structured logger.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	return &Server{
		cfg:        cfg,
		log:        log,
		http:       srv,
		health:     checker,
		metrics:    m,
		pool:       pool,
		baseCancel: baseCancel,
		drainWait:  o.drainWait,
	}, nil
}

// newRootHandler wraps the route table in the cross-cutting middleware applied
// to every request. It is a separate function so the production ordering can
// be tested directly. See docs/middleware.md and ADR-003 for why this order:
// RequestID first (everything else reads it), Recovery innermost (so the
// logging and metrics layers observe the 500 it writes).
func newRootHandler(routes http.Handler, log *slog.Logger, m *metrics.Metrics) http.Handler {
	return Chain(routes,
		RequestID(),
		AccessLog(log),
		Metrics(m),
		Recovery(log),
	)
}

// Health exposes the readiness state, for tests and embedding code.
func (s *Server) Health() *health.Checker { return s.health }

// ListenAndServe binds cfg.Addr and serves until ctx is cancelled, then shuts
// down gracefully. It returns nil after a clean shutdown.
func (s *Server) ListenAndServe(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Addr)
	if err != nil {
		s.releaseUnstarted()
		return fmt.Errorf("listen on %s: %w", s.cfg.Addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves on ln until ctx is cancelled, then shuts down gracefully.
// Taking a listener (rather than an address) lets tests bind port 0 and learn
// the chosen port without races.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.log.InfoContext(ctx, "server starting", "addr", ln.Addr().String(), "config", s.cfg)
	if s.cfg.AuthToken == "" {
		s.log.WarnContext(ctx, "AUTH_TOKEN is empty: API authentication is disabled")
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.http.Serve(ln) }()

	s.health.MarkReady()

	select {
	case err := <-serveErr:
		// Serve returned without Shutdown being called: the listener failed.
		s.releaseUnstarted()
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		return s.shutdown(serveErr)
	}
}

// releaseUnstarted frees resources when the server never reached serving.
func (s *Server) releaseUnstarted() {
	s.health.MarkDraining()
	s.baseCancel(domain.ErrUnavailable)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already expired: do not wait for idle workers
	if err := s.pool.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.log.Error("release worker pool", "error", err)
	}
}
