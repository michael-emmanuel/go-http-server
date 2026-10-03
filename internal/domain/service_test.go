package domain

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/go-production-http-server/internal/worker"
)

var errBoom = errors.New("boom")

// fakeExecutor captures tasks so a test decides exactly when, and on which
// goroutine, they run. That removes scheduling nondeterminism from lifecycle
// tests.
type fakeExecutor struct {
	mu    sync.Mutex
	tasks []worker.Task
	err   error
}

func (f *fakeExecutor) Submit(task worker.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.tasks = append(f.tasks, task)
	return nil
}

func (f *fakeExecutor) runAll(ctx context.Context) {
	f.mu.Lock()
	tasks := f.tasks
	f.tasks = nil
	f.mu.Unlock()
	for _, task := range tasks {
		task(ctx)
	}
}

func testConfig() ServiceConfig {
	return ServiceConfig{MaxItems: 100, MaxItemDelay: time.Second, BatchConcurrency: 8}
}

func newTestService(t *testing.T, cfg ServiceConfig, deps ServiceDeps) *Service {
	t.Helper()
	if deps.Store == nil {
		deps.Store = NewStore(100)
	}
	if deps.Executor == nil {
		deps.Executor = &fakeExecutor{}
	}
	s, err := NewService(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sumOfSquares(n int) int64 {
	var s int64
	for i := 0; i < n; i++ {
		s += int64(i) * int64(i)
	}
	return s
}

func TestNewServiceValidatesArguments(t *testing.T) {
	if _, err := NewService(ServiceConfig{MaxItems: 0, BatchConcurrency: 1}, ServiceDeps{Store: NewStore(1), Executor: &fakeExecutor{}}); err == nil {
		t.Error("MaxItems 0 must be rejected")
	}
	if _, err := NewService(testConfig(), ServiceDeps{Executor: &fakeExecutor{}}); err == nil {
		t.Error("missing store must be rejected")
	}
	if _, err := NewService(testConfig(), ServiceDeps{Store: NewStore(1)}); err == nil {
		t.Error("missing executor must be rejected")
	}
}

func TestValidation(t *testing.T) {
	s := newTestService(t, testConfig(), ServiceDeps{})
	tests := []struct {
		name  string
		req   BatchRequest
		field string
	}{
		{"zero items", BatchRequest{Items: 0}, "items"},
		{"negative items", BatchRequest{Items: -3}, "items"},
		{"too many items", BatchRequest{Items: 101}, "items"},
		{"negative delay", BatchRequest{Items: 1, ItemDelay: -time.Millisecond}, "delay_ms"},
		{"excessive delay", BatchRequest{Items: 1, ItemDelay: 2 * time.Second}, "delay_ms"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.RunBatch(context.Background(), tc.req)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Field != tc.field {
				t.Fatalf("err = %v, want *ValidationError for %q", err, tc.field)
			}
			if _, err := s.SubmitJob(context.Background(), tc.req); !errors.As(err, &ve) {
				t.Fatalf("SubmitJob err = %v, want *ValidationError", err)
			}
		})
	}
}

func TestRunBatchChecksumIsExactUnderConcurrency(t *testing.T) {
	s := newTestService(t, testConfig(), ServiceDeps{})
	for _, n := range []int{1, 2, 10, 100} {
		res, err := s.RunBatch(context.Background(), BatchRequest{Items: n})
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if want := sumOfSquares(n); res.Checksum != want {
			t.Errorf("n=%d checksum = %d, want %d (work lost or duplicated)", n, res.Checksum, want)
		}
		if want := min(8, n); res.Concurrency != want {
			t.Errorf("n=%d concurrency = %d, want %d", n, res.Concurrency, want)
		}
	}
}

func TestRunBatchNeverExceedsConcurrencyLimitAndReachesIt(t *testing.T) {
	const limit = 3
	var (
		active, peak atomic.Int64
		barrier      sync.WaitGroup
	)
	barrier.Add(limit)

	work := func(ctx context.Context, index int, _ time.Duration) (int64, error) {
		cur := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		// The first `limit` items rendezvous. They can only all be released if
		// `limit` items really run at the same time, which proves parallelism
		// without any sleeping.
		if index < limit {
			barrier.Done()
			barrier.Wait()
		}
		return 0, nil
	}

	cfg := testConfig()
	cfg.BatchConcurrency = limit
	s := newTestService(t, cfg, ServiceDeps{Work: work})

	done := make(chan error, 1)
	go func() {
		_, err := s.RunBatch(context.Background(), BatchRequest{Items: 50})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: items did not run concurrently")
	}

	if got := peak.Load(); got != limit {
		t.Fatalf("peak concurrency = %d, want exactly %d", got, limit)
	}
}

func TestFirstErrorCancelsSiblingsAndWaitsForThem(t *testing.T) {
	const items = 6
	var exited atomic.Int64
	started := make(chan struct{}, items)

	work := func(ctx context.Context, index int, _ time.Duration) (int64, error) {
		defer exited.Add(1)
		started <- struct{}{}
		if index == 0 {
			// Fail only after every sibling is running, so cancellation of
			// blocked siblings is actually exercised.
			for i := 0; i < items; i++ {
				<-started
			}
			return 0, errBoom
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}

	cfg := testConfig()
	cfg.BatchConcurrency = items
	s := newTestService(t, cfg, ServiceDeps{Work: work})

	// Every item sends one token on `started` when it begins; item 0 collects
	// `items` tokens, so it fails only once all items (itself included) are
	// running. That guarantees the siblings are blocked on ctx when it fails.
	_, err := s.RunBatch(context.Background(), BatchRequest{Items: items})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want it to wrap errBoom (the first failure, not a sibling's context.Canceled)", err)
	}
	if got := exited.Load(); got != items {
		t.Fatalf("%d/%d item goroutines exited; RunBatch must not return while any still run", got, items)
	}
}

func TestParentCancellationStopsWork(t *testing.T) {
	const items = 4
	var running sync.WaitGroup
	running.Add(items)
	work := func(ctx context.Context, _ int, _ time.Duration) (int64, error) {
		running.Done()
		<-ctx.Done()
		return 0, ctx.Err()
	}
	cfg := testConfig()
	cfg.BatchConcurrency = items
	s := newTestService(t, cfg, ServiceDeps{Work: work})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := s.RunBatch(ctx, BatchRequest{Items: items})
		errc <- err
	}()

	running.Wait() // all items are blocked inside work
	cancel()

	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestAlreadyCancelledContextFailsFast(t *testing.T) {
	s := newTestService(t, testConfig(), ServiceDeps{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.RunBatch(ctx, BatchRequest{Items: 10}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestDeadlineExceededPropagates(t *testing.T) {
	work := func(ctx context.Context, _ int, _ time.Duration) (int64, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	s := newTestService(t, testConfig(), ServiceDeps{Work: work})
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if _, err := s.RunBatch(ctx, BatchRequest{Items: 3}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestPanicInWorkIsContainedAsError(t *testing.T) {
	work := func(_ context.Context, index int, _ time.Duration) (int64, error) {
		if index == 2 {
			panic("kaboom")
		}
		return 0, nil
	}
	s := newTestService(t, testConfig(), ServiceDeps{Work: work})
	_, err := s.RunBatch(context.Background(), BatchRequest{Items: 5})
	if err == nil {
		t.Fatal("a panicking item must surface as an error, not crash or vanish")
	}
}

func TestSimulatedWork(t *testing.T) {
	v, err := SimulatedWork(context.Background(), 7, 0)
	if err != nil || v != 49 {
		t.Fatalf("got (%d, %v), want (49, nil)", v, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SimulatedWork(ctx, 1, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("zero delay with cancelled ctx: err = %v", err)
	}
	// An hour-long delay returning immediately proves the wait is interruptible.
	if _, err := SimulatedWork(ctx, 1, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("long delay with cancelled ctx: err = %v", err)
	}
}

type fakeClock struct{ n atomic.Int64 }

func (c *fakeClock) Now() time.Time {
	return time.Unix(1_700_000_000, 0).Add(time.Duration(c.n.Add(1)) * time.Second)
}

func TestJobLifecycleSuccess(t *testing.T) {
	exec := &fakeExecutor{}
	clock := &fakeClock{}
	s := newTestService(t, testConfig(), ServiceDeps{Executor: exec, Now: clock.Now, NewID: func() (string, error) { return "job-1", nil }})

	job, err := s.SubmitJob(context.Background(), BatchRequest{Items: 10})
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "job-1" || job.Status != StatusQueued {
		t.Fatalf("submitted job = %+v", job)
	}
	if got, _ := s.GetJob(context.Background(), "job-1"); got.Status != StatusQueued {
		t.Fatalf("before run: status = %s, want queued", got.Status)
	}

	exec.runAll(context.Background())

	got, err := s.GetJob(context.Background(), "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSucceeded || got.Result == nil || got.Result.Checksum != sumOfSquares(10) {
		t.Fatalf("after run: %+v", got)
	}
	if got.StartedAt.IsZero() || got.FinishedAt.IsZero() || !got.FinishedAt.After(got.StartedAt) {
		t.Fatalf("timestamps not recorded sensibly: %+v", got)
	}
	if st := s.Stats(); st.JobsSucceeded != 1 || st.ItemsProcessed != 10 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestJobFailureDoesNotStoreInternalError(t *testing.T) {
	exec := &fakeExecutor{}
	work := func(context.Context, int, time.Duration) (int64, error) {
		return 0, errors.New("dial tcp 10.0.0.5:5432: connection refused")
	}
	s := newTestService(t, testConfig(), ServiceDeps{Executor: exec, Work: work, NewID: func() (string, error) { return "j", nil }})

	if _, err := s.SubmitJob(context.Background(), BatchRequest{Items: 2}); err != nil {
		t.Fatal(err)
	}
	exec.runAll(context.Background())

	got, _ := s.GetJob(context.Background(), "j")
	if got.Status != StatusFailed || got.Failure != FailureInternal {
		t.Fatalf("job = %+v", got)
	}
	if s.Stats().JobsFailed != 1 {
		t.Fatalf("stats = %+v", s.Stats())
	}
}

func TestJobCancelledByShutdownContext(t *testing.T) {
	exec := &fakeExecutor{}
	s := newTestService(t, testConfig(), ServiceDeps{Executor: exec, NewID: func() (string, error) { return "j", nil }})

	if _, err := s.SubmitJob(context.Background(), BatchRequest{Items: 5, ItemDelay: time.Second}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the pool's context after a missed shutdown deadline
	exec.runAll(ctx)

	got, _ := s.GetJob(context.Background(), "j")
	if got.Status != StatusCanceled || got.Failure != FailureCanceled {
		t.Fatalf("job = %+v", got)
	}
	if s.Stats().JobsCanceled != 1 {
		t.Fatalf("stats = %+v", s.Stats())
	}
}

func TestSubmitJobMapsExecutorErrorsAndRollsBack(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"queue full", worker.ErrQueueFull, ErrOverloaded},
		{"closed", worker.ErrClosed, ErrUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(10)
			s := newTestService(t, testConfig(), ServiceDeps{Store: store, Executor: &fakeExecutor{err: tc.err}})
			_, err := s.SubmitJob(context.Background(), BatchRequest{Items: 1})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if store.Len() != 0 {
				t.Fatalf("rejected job left in store (len=%d)", store.Len())
			}
		})
	}

	other := errors.New("weird")
	s := newTestService(t, testConfig(), ServiceDeps{Executor: &fakeExecutor{err: other}})
	if _, err := s.SubmitJob(context.Background(), BatchRequest{Items: 1}); !errors.Is(err, other) {
		t.Fatalf("unexpected executor errors must be preserved, got %v", err)
	}
}

func TestSubmitJobIDGenerationFailure(t *testing.T) {
	s := newTestService(t, testConfig(), ServiceDeps{NewID: func() (string, error) { return "", errBoom }})
	if _, err := s.SubmitJob(context.Background(), BatchRequest{Items: 1}); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestGetJobErrors(t *testing.T) {
	s := newTestService(t, testConfig(), ServiceDeps{})
	if _, err := s.GetJob(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetJob(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := s.SubmitJob(ctx, BatchRequest{Items: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("SubmitJob err = %v, want context.Canceled", err)
	}
}

func TestSubmitJobStoreFullIsOverloaded(t *testing.T) {
	store := NewStore(1)
	exec := &fakeExecutor{}
	s := newTestService(t, testConfig(), ServiceDeps{Store: store, Executor: exec})
	if _, err := s.SubmitJob(context.Background(), BatchRequest{Items: 1}); err != nil {
		t.Fatal(err)
	}
	// The first job is still queued (never run), so it cannot be evicted.
	if _, err := s.SubmitJob(context.Background(), BatchRequest{Items: 1}); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("err = %v, want ErrOverloaded", err)
	}
}

// TestRealPoolEndToEnd wires the real worker pool and hammers it from many
// goroutines. It is primarily a target for the race detector.
func TestRealPoolEndToEnd(t *testing.T) {
	pool, err := worker.NewPool(worker.Config{Workers: 4, QueueSize: 256})
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(1000)
	s := newTestService(t, testConfig(), ServiceDeps{Store: store, Executor: pool})

	const submitters, perSubmitter = 8, 20
	ids := make(chan string, submitters*perSubmitter)
	var wg sync.WaitGroup
	for g := 0; g < submitters; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perSubmitter; i++ {
				job, err := s.SubmitJob(context.Background(), BatchRequest{Items: 5})
				if err != nil {
					t.Errorf("submit: %v", err)
					return
				}
				ids <- job.ID
			}
		}()
	}
	wg.Wait()
	close(ids)

	// Shutdown drains the queue, so after it returns every job is terminal.
	if err := pool.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	count := 0
	for id := range ids {
		count++
		j, err := s.GetJob(context.Background(), id)
		if err != nil || j.Status != StatusSucceeded || j.Result.Checksum != sumOfSquares(5) {
			t.Fatalf("job %s = %+v, err %v", id, j, err)
		}
	}
	if count != submitters*perSubmitter {
		t.Fatalf("collected %d ids, want %d", count, submitters*perSubmitter)
	}
	if st := s.Stats(); st.JobsSucceeded != int64(count) || st.ItemsProcessed != int64(count*5) {
		t.Fatalf("stats = %+v", st)
	}
}

func TestConcurrentRunBatchStatsAreExact(t *testing.T) {
	s := newTestService(t, testConfig(), ServiceDeps{})
	const requests, items = 40, 25
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.RunBatch(context.Background(), BatchRequest{Items: items})
			if err != nil || res.Checksum != sumOfSquares(items) {
				t.Errorf("res=%+v err=%v", res, err)
			}
		}()
	}
	wg.Wait()
	if got := s.Stats().ItemsProcessed; got != requests*items {
		t.Fatalf("ItemsProcessed = %d, want %d", got, requests*items)
	}
}

// TestConcurrencyLimitBlocksFurtherLaunches holds every item open so that, if
// the limit were not enforced, all items would start. The negative check is
// one-sided: it cannot fail on a correct implementation, and it reliably fails
// on one that launches more than the limit.
func TestConcurrencyLimitBlocksFurtherLaunches(t *testing.T) {
	const limit, items = 3, 20
	started := make(chan struct{}, items)
	release := make(chan struct{})
	work := func(ctx context.Context, _ int, _ time.Duration) (int64, error) {
		started <- struct{}{}
		select {
		case <-release:
			return 0, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	cfg := testConfig()
	cfg.BatchConcurrency = limit
	s := newTestService(t, cfg, ServiceDeps{Work: work})

	done := make(chan error, 1)
	go func() {
		_, err := s.RunBatch(context.Background(), BatchRequest{Items: items})
		done <- err
	}()

	for i := 0; i < limit; i++ {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d/%d items started", i, limit)
		}
	}
	select {
	case <-started:
		t.Fatalf("more than %d items started concurrently; the limit is not enforced", limit)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
