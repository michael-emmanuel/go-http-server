package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func newPool(t *testing.T, workers, queue int) *Pool {
	t.Helper()
	p, err := NewPool(Config{Workers: workers, QueueSize: queue})
	if err != nil {
		t.Fatal(err)
	}
	// Guarantee no goroutine outlives the test, even on failure.
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = p.Shutdown(ctx)
	})
	return p
}

func TestNewPoolValidatesConfig(t *testing.T) {
	if _, err := NewPool(Config{Workers: 0, QueueSize: 1}); err == nil {
		t.Error("zero workers must be rejected")
	}
	if _, err := NewPool(Config{Workers: 1, QueueSize: 0}); err == nil {
		t.Error("zero queue size must be rejected")
	}
}

func TestRunsAllSubmittedTasks(t *testing.T) {
	p := newPool(t, 4, 100)
	var ran atomic.Int64
	for i := 0; i < 100; i++ {
		if err := p.Submit(func(context.Context) { ran.Add(1) }); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := ran.Load(); got != 100 {
		t.Fatalf("ran %d tasks, want 100 (Shutdown must drain the queue)", got)
	}
}

func TestSubmitRejectsWhenQueueFull(t *testing.T) {
	p := newPool(t, 1, 1)
	started := make(chan struct{})
	release := make(chan struct{})

	// Occupy the only worker.
	if err := p.Submit(func(context.Context) { close(started); <-release }); err != nil {
		t.Fatal(err)
	}
	<-started

	// Fill the one queue slot.
	if err := p.Submit(func(context.Context) {}); err != nil {
		t.Fatalf("queue slot should be free: %v", err)
	}
	if depth := p.QueueDepth(); depth != 1 {
		t.Fatalf("QueueDepth = %d, want 1", depth)
	}

	// Backpressure: rejected immediately rather than blocking.
	if err := p.Submit(func(context.Context) {}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}

	close(release)
}

func TestSubmitAfterShutdownIsRejected(t *testing.T) {
	p := newPool(t, 1, 1)
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Submit(func(context.Context) {}); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	p := newPool(t, 1, 1)
	for i := 0; i < 3; i++ {
		if err := p.Shutdown(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestShutdownDeadlineCancelsRunningTasks(t *testing.T) {
	p := newPool(t, 1, 1)
	started := make(chan struct{})
	sawCancel := make(chan error, 1)

	if err := p.Submit(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		sawCancel <- ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	<-started

	// An already-expired context models "the shutdown deadline has passed"
	// without depending on wall-clock timing.
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.Shutdown(expired)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown err = %v, want wrapped context.Canceled", err)
	}
	if got := <-sawCancel; !errors.Is(got, context.Canceled) {
		t.Fatalf("task saw %v, want context.Canceled", got)
	}
}

func TestPanickingTaskDoesNotKillWorker(t *testing.T) {
	p := newPool(t, 1, 2)
	done := make(chan struct{})

	if err := p.Submit(func(context.Context) { panic("boom") }); err != nil {
		t.Fatal(err)
	}
	if err := p.Submit(func(context.Context) { close(done) }); err != nil {
		t.Fatal(err)
	}
	<-done // the same single worker must survive the panic and run this task
}

// TestConcurrentSubmitAndShutdown guards against the classic "send on closed
// channel" panic. It is meaningful under -race and repeated runs.
func TestConcurrentSubmitAndShutdown(t *testing.T) {
	p := newPool(t, 4, 8)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				err := p.Submit(func(context.Context) {})
				if err != nil && !errors.Is(err, ErrQueueFull) && !errors.Is(err, ErrClosed) {
					t.Errorf("unexpected submit error: %v", err)
					return
				}
			}
		}()
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	wg.Wait()
}
