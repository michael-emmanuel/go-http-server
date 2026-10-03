// Package worker provides a bounded pool for background tasks.
//
// It exists to demonstrate three production concerns that a bare "go func()"
// ignores:
//
//   - Bounded resources: the number of concurrent tasks and the queue length
//     are fixed, so overload turns into explicit rejection (backpressure)
//     instead of unbounded memory growth.
//   - Lifecycle: tasks run under a context owned by the pool, not by the HTTP
//     request that submitted them, because an accepted job must outlive the
//     202 response. Shutdown drains the queue and, at a deadline, cancels that
//     context.
//   - Panic containment: net/http recovers panics in handler goroutines, but
//     it cannot recover panics in goroutines we start ourselves. An unrecovered
//     panic here would terminate the whole process.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"
)

var (
	// ErrQueueFull is returned by Submit when the queue is at capacity.
	ErrQueueFull = errors.New("worker queue full")
	// ErrClosed is returned by Submit after Shutdown has begun.
	ErrClosed = errors.New("worker pool closed")
)

// Task is a unit of background work. It must honor ctx cancellation, because
// that is how Shutdown stops work that outlives the shutdown deadline.
type Task func(ctx context.Context)

// Config configures a Pool.
type Config struct {
	Workers   int
	QueueSize int
	Logger    *slog.Logger
}

// Pool runs Tasks on a fixed number of goroutines fed by a bounded queue.
type Pool struct {
	log *slog.Logger

	// mu guards closed and the close of tasks. Submit holds it for reading so
	// that a send can never race with close(p.tasks), which would panic.
	mu     sync.RWMutex
	closed bool
	tasks  chan Task

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

// NewPool starts cfg.Workers goroutines. The caller must call Shutdown to
// release them.
func NewPool(cfg Config) (*Pool, error) {
	if cfg.Workers < 1 {
		return nil, fmt.Errorf("worker pool: workers must be >= 1, got %d", cfg.Workers)
	}
	if cfg.QueueSize < 1 {
		return nil, fmt.Errorf("worker pool: queue size must be >= 1, got %d", cfg.QueueSize)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	// The pool's context is rooted in Background on purpose: it must not be
	// tied to any request. It is cancelled only by Shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		log:    log,
		tasks:  make(chan Task, cfg.QueueSize),
		ctx:    ctx,
		cancel: cancel,
	}
	p.wg.Add(cfg.Workers)
	for i := 0; i < cfg.Workers; i++ {
		go p.run()
	}
	return p, nil
}

// Submit enqueues task without blocking. A full queue returns ErrQueueFull
// immediately: blocking the caller would just move the unbounded queue into
// the pile of parked HTTP handler goroutines.
func (p *Pool) Submit(task Task) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return ErrClosed
	}
	select {
	case p.tasks <- task:
		return nil
	default:
		return ErrQueueFull
	}
}

// QueueDepth returns the number of tasks waiting to start.
func (p *Pool) QueueDepth() int { return len(p.tasks) }

// Shutdown stops accepting tasks, lets workers drain everything already
// queued, and waits for them. If ctx expires first, the pool's context is
// cancelled so tasks observe cancellation, and Shutdown still waits for the
// workers to return so no goroutine outlives the call. It is safe to call more
// than once.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.tasks)
	}
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.cancel()
		return nil
	case <-ctx.Done():
		p.cancel()
		<-done
		return fmt.Errorf("worker drain: %w", ctx.Err())
	}
}

func (p *Pool) run() {
	defer p.wg.Done()
	for task := range p.tasks {
		p.runTask(task)
	}
}

func (p *Pool) runTask(task Task) {
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("worker task panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	task(p.ctx)
}
