package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/example/go-production-http-server/internal/domain"
)

// cancelWindowFraction is the share of the shutdown budget reserved for the
// cooperative-cancellation phase. In-flight requests get the first 80% of the
// budget to finish on their own; if any remain, their contexts are cancelled
// and they get the last 20% to notice and respond (with a 503) before
// connections are severed.
const cancelWindowFraction = 5 // 1/5 = 20%

// shutdown runs the graceful shutdown sequence:
//
//  1. Mark readiness as draining so load balancers stop sending new traffic.
//  2. Optionally keep serving for ShutdownDrainDelay so that signal has time
//     to propagate (orchestrators update routing asynchronously).
//  3. Drain: http.Server.Shutdown closes listeners and idle keep-alive
//     connections and waits for in-flight requests to finish, for up to 80%
//     of ShutdownTimeout.
//  4. Cancel: if requests remain, cancel the base context (cause:
//     domain.ErrUnavailable). Handlers that honor their context unwind and
//     answer 503. They have the remaining 20% of the budget.
//  5. Force: connections still open after that are closed.
//  6. Drain the worker pool within whatever time is left.
//  7. Reap the Serve goroutine and report every failure, joined.
//
// One overall deadline covers the whole sequence, because an orchestrator's
// termination grace period is one wall-clock budget, not one per phase.
func (s *Server) shutdown(serveErr <-chan error) error {
	s.log.Info("shutdown initiated", "timeout", s.cfg.ShutdownTimeout.String(), "drain_delay", s.cfg.ShutdownDrainDelay.String())
	started := time.Now()

	// Step 1. Readiness flips before anything else, so the very first thing
	// an outside observer can see is "stop sending me traffic".
	s.health.MarkDraining()

	// The shutdown context is rooted in Background, not the signal context:
	// that one is already cancelled, and deriving from it would give us a
	// deadline of "immediately".
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	drainCtx, cancelDrain := context.WithTimeout(ctx, s.cfg.ShutdownTimeout-s.cfg.ShutdownTimeout/cancelWindowFraction)
	defer cancelDrain()

	var errs []error

	// Step 2.
	if d := s.cfg.ShutdownDrainDelay; d > 0 {
		if err := s.drainWait(drainCtx, d); err != nil {
			errs = append(errs, fmt.Errorf("drain delay: %w", err))
		}
	}

	// Steps 3 to 5.
	if err := s.http.Shutdown(drainCtx); err != nil {
		errs = append(errs, fmt.Errorf("drain http connections: %w", err))
		s.log.Warn("graceful drain incomplete, cancelling in-flight requests", "error", err)

		// Step 4: cancel, then give handlers the rest of the budget to react.
		s.baseCancel(domain.ErrUnavailable)
		if err := s.http.Shutdown(ctx); err != nil {
			// Step 5.
			s.log.Warn("in-flight requests ignored cancellation, forcing close", "error", err)
			if cerr := s.http.Close(); cerr != nil {
				errs = append(errs, fmt.Errorf("force close: %w", cerr))
			}
		}
	}

	// Step 6.
	if err := s.pool.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}

	// Release the base context on every path, including a clean shutdown.
	s.baseCancel(nil)

	// Step 7. Serve returns http.ErrServerClosed once Shutdown was called;
	// receiving it here guarantees the goroutine has exited (no leak).
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		errs = append(errs, fmt.Errorf("serve: %w", err))
	}

	if len(errs) > 0 {
		err := fmt.Errorf("shutdown incomplete after %s: %w", time.Since(started).Round(time.Millisecond), errors.Join(errs...))
		s.log.Error("shutdown finished with errors", "error", err)
		return err
	}
	s.log.Info("shutdown complete", "duration", time.Since(started).String())
	return nil
}

// sleepContext waits for d or until ctx is done, whichever is first.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
