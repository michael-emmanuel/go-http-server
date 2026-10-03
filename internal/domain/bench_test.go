package domain

import (
	"context"
	"strconv"
	"testing"
)

// BenchmarkRunBatch measures the fan-out machinery itself (goroutine launch,
// semaphore, WaitGroup) using zero-delay work. It does not model real I/O
// latency, where the concurrency limit, not scheduling overhead, dominates.
func BenchmarkRunBatch(b *testing.B) {
	for _, items := range []int{1, 10, 100} {
		b.Run(strconv.Itoa(items)+"_items", func(b *testing.B) {
			s := newBenchService(b)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := s.RunBatch(context.Background(), BatchRequest{Items: items}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRunBatchConcurrentRequests runs many batches at once, the shape of
// real traffic, and exercises the shared atomic counter under contention.
func BenchmarkRunBatchConcurrentRequests(b *testing.B) {
	s := newBenchService(b)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := s.RunBatch(context.Background(), BatchRequest{Items: 20}); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func newBenchService(b *testing.B) *Service {
	b.Helper()
	s, err := NewService(testConfig(), ServiceDeps{Store: NewStore(10), Executor: &fakeExecutor{}})
	if err != nil {
		b.Fatal(err)
	}
	return s
}
