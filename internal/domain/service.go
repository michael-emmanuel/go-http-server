package domain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/michael-emmanuel/go-production-http-server/internal/worker"
)

// Executor runs background tasks. It is satisfied by *worker.Pool. The
// interface exists because the domain needs exactly one capability
// (submit a task) and tests substitute a fake that runs tasks
// deterministically on the calling goroutine.
type Executor interface {
	Submit(task worker.Task) error
}

// WorkFunc performs one simulated unit of work. It must return promptly when
// ctx is cancelled.
type WorkFunc func(ctx context.Context, index int, delay time.Duration) (int64, error)

// SimulatedWork waits for delay (or cancellation) and returns index squared.
// Waiting on a timer inside a select is the canonical way to make a blocking
// operation cancellable; time.Sleep cannot be interrupted.
func SimulatedWork(ctx context.Context, index int, delay time.Duration) (int64, error) {
	if delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	} else if err := ctx.Err(); err != nil {
		return 0, err
	}
	return int64(index) * int64(index), nil
}

// ServiceConfig holds the business limits enforced by the Service.
type ServiceConfig struct {
	MaxItems         int
	MaxItemDelay     time.Duration
	BatchConcurrency int
}

// ServiceDeps are the collaborators of a Service. Nil optional fields get
// production defaults.
type ServiceDeps struct {
	Store    *Store
	Executor Executor
	Work     WorkFunc               // default: SimulatedWork
	Logger   *slog.Logger           // default: discard
	Now      func() time.Time       // default: time.Now
	NewID    func() (string, error) // default: 128-bit crypto/rand hex
}

// Stats are cumulative counters for observability.
type Stats struct {
	ItemsProcessed int64
	JobsSucceeded  int64
	JobsFailed     int64
	JobsCanceled   int64
	JobsStored     int64
}

// Service implements the work use cases. It is safe for concurrent use: all
// mutable state is either behind Store's mutex or in atomics.
type Service struct {
	cfg   ServiceConfig
	store *Store
	exec  Executor
	work  WorkFunc
	log   *slog.Logger
	now   func() time.Time
	newID func() (string, error)

	// Shared across every request and worker goroutine, hence atomics.
	itemsProcessed atomic.Int64
	jobsSucceeded  atomic.Int64
	jobsFailed     atomic.Int64
	jobsCanceled   atomic.Int64
}

// NewService builds a Service.
func NewService(cfg ServiceConfig, deps ServiceDeps) (*Service, error) {
	if cfg.MaxItems < 1 || cfg.BatchConcurrency < 1 {
		return nil, errors.New("domain: MaxItems and BatchConcurrency must be >= 1")
	}
	if deps.Store == nil || deps.Executor == nil {
		return nil, errors.New("domain: Store and Executor are required")
	}
	s := &Service{
		cfg:   cfg,
		store: deps.Store,
		exec:  deps.Executor,
		work:  deps.Work,
		log:   deps.Logger,
		now:   deps.Now,
		newID: deps.NewID,
	}
	if s.work == nil {
		s.work = SimulatedWork
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newID == nil {
		s.newID = randomID
	}
	return s, nil
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Stats returns cumulative counters.
func (s *Service) Stats() Stats {
	return Stats{
		ItemsProcessed: s.itemsProcessed.Load(),
		JobsSucceeded:  s.jobsSucceeded.Load(),
		JobsFailed:     s.jobsFailed.Load(),
		JobsCanceled:   s.jobsCanceled.Load(),
		JobsStored:     int64(s.store.Len()),
	}
}

func (s *Service) validate(req BatchRequest) error {
	if req.Items < 1 || req.Items > s.cfg.MaxItems {
		return &ValidationError{Field: "items", Reason: fmt.Sprintf("must be between 1 and %d", s.cfg.MaxItems)}
	}
	if req.ItemDelay < 0 || req.ItemDelay > s.cfg.MaxItemDelay {
		return &ValidationError{Field: "delay_ms", Reason: fmt.Sprintf("must be between 0 and %d", s.cfg.MaxItemDelay.Milliseconds())}
	}
	return nil
}

// RunBatch processes the items concurrently and synchronously. It returns when
// every item has finished, the first item fails, or ctx is done. Because ctx
// is the caller's request context, a disconnected client or an expired request
// deadline stops the work instead of leaving it burning CPU for nobody.
func (s *Service) RunBatch(ctx context.Context, req BatchRequest) (BatchResult, error) {
	if err := s.validate(req); err != nil {
		return BatchResult{}, err
	}
	start := s.now()
	values, err := s.process(ctx, req)
	if err != nil {
		return BatchResult{}, fmt.Errorf("run batch: %w", err)
	}
	var checksum int64
	for _, v := range values {
		checksum += v
	}
	return BatchResult{
		Items:       req.Items,
		Checksum:    checksum,
		Concurrency: min(s.cfg.BatchConcurrency, req.Items),
		Elapsed:     s.now().Sub(start),
	}, nil
}

// process fans the items out over at most BatchConcurrency goroutines.
//
// Why bounded rather than one goroutine per item: goroutines are cheap but not
// free, and the resources the work touches (CPU, downstream connections) are
// finite. An unbounded fan-out lets one request with a large "items" value
// starve every other request.
//
// Why no mutex around values: each goroutine writes only its own slice
// element and the results are read after wg.Wait(), which establishes a
// happens-before edge. Disjoint writes need no lock; a shared accumulator
// would.
func (s *Service) process(parent context.Context, req BatchRequest) ([]int64, error) {
	// Derive a cancellable context so the first failure stops sibling items.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	values := make([]int64, req.Items)
	sem := make(chan struct{}, s.cfg.BatchConcurrency)

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error // written once inside once.Do, read after wg.Wait()
	)
	fail := func(err error) {
		once.Do(func() {
			firstErr = err
			cancel()
		})
	}

launch:
	for i := 0; i < req.Items; i++ {
		// Acquiring the semaphore can block; selecting on ctx.Done() means a
		// cancelled request stops launching work instead of waiting for a slot.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break launch
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			v, err := s.runItem(ctx, i, req.ItemDelay)
			if err != nil {
				fail(fmt.Errorf("item %d: %w", i, err))
				return
			}
			values[i] = v
			s.itemsProcessed.Add(1)
		}(i)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	// The launch loop may have stopped early because the parent was cancelled
	// before any item observed it.
	if err := parent.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

// runItem converts a panic in the work function into an error. net/http's own
// recovery only covers the handler goroutine; a panic in a goroutine we spawn
// would terminate the process and bypass the Recovery middleware.
func (s *Service) runItem(ctx context.Context, index int, delay time.Duration) (v int64, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.log.ErrorContext(ctx, "work item panicked", "item", index, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = fmt.Errorf("work item panicked: %v", r)
		}
	}()
	return s.work(ctx, index, delay)
}

// SubmitJob validates req, records a queued job, and hands it to the executor.
// It returns immediately; progress is observed through GetJob.
//
// The task deliberately does not use ctx. ctx is the request context, which is
// cancelled as soon as the 202 response is sent; a job derived from it would be
// killed before it started. The executor supplies a context owned by the
// process lifecycle instead.
func (s *Service) SubmitJob(ctx context.Context, req BatchRequest) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, fmt.Errorf("submit job: %w", err)
	}
	if err := s.validate(req); err != nil {
		return Job{}, err
	}
	id, err := s.newID()
	if err != nil {
		return Job{}, fmt.Errorf("generate job id: %w", err)
	}
	job := Job{ID: id, Request: req, Status: StatusQueued, CreatedAt: s.now()}
	if err := s.store.Create(job); err != nil {
		return Job{}, fmt.Errorf("store job: %w", err)
	}

	if err := s.exec.Submit(func(jobCtx context.Context) { s.execute(jobCtx, id, req) }); err != nil {
		s.store.Delete(id) // roll back so a rejected job is not left "queued" forever
		switch {
		case errors.Is(err, worker.ErrQueueFull):
			return Job{}, fmt.Errorf("submit job: %w", ErrOverloaded)
		case errors.Is(err, worker.ErrClosed):
			return Job{}, fmt.Errorf("submit job: %w", ErrUnavailable)
		default:
			return Job{}, fmt.Errorf("submit job: %w", err)
		}
	}
	return job, nil
}

// GetJob returns a snapshot of the job.
func (s *Service) GetJob(ctx context.Context, id string) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, fmt.Errorf("get job: %w", err)
	}
	j, ok := s.store.Get(id)
	if !ok {
		return Job{}, fmt.Errorf("job %q: %w", id, ErrNotFound)
	}
	return j, nil
}

// execute runs on a worker goroutine and records the job's progress.
func (s *Service) execute(ctx context.Context, id string, req BatchRequest) {
	started := s.now()
	s.store.Update(id, func(j *Job) {
		j.Status = StatusRunning
		j.StartedAt = started
	})

	res, err := s.RunBatch(ctx, req)
	finished := s.now()

	switch {
	case err == nil:
		s.jobsSucceeded.Add(1)
		s.store.Update(id, func(j *Job) {
			j.Status = StatusSucceeded
			j.FinishedAt = finished
			j.Result = &res
		})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		s.jobsCanceled.Add(1)
		s.store.Update(id, func(j *Job) {
			j.Status = StatusCanceled
			j.FinishedAt = finished
			j.Failure = FailureCanceled
		})
	default:
		s.jobsFailed.Add(1)
		s.log.ErrorContext(ctx, "job failed", "job_id", id, "error", err)
		s.store.Update(id, func(j *Job) {
			j.Status = StatusFailed
			j.FinishedAt = finished
			j.Failure = FailureInternal
		})
	}
}
