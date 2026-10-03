package metrics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestObserveCountsStatusClasses(t *testing.T) {
	m := New()
	for _, s := range []int{200, 201, 404, 500, 503, 302, 99} {
		m.Observe(s, time.Millisecond)
	}
	s := m.Snapshot()
	if s.RequestsTotal != 7 {
		t.Errorf("RequestsTotal = %d, want 7", s.RequestsTotal)
	}
	want := map[string]int64{"2xx": 2, "3xx": 1, "4xx": 1, "5xx": 2, "1xx": 0}
	for k, v := range want {
		if s.ResponsesByClass[k] != v {
			t.Errorf("class %s = %d, want %d", k, s.ResponsesByClass[k], v)
		}
	}
	if s.ClientErrorsTotal != 1 || s.ServerErrorsTotal != 2 {
		t.Errorf("errors = %d client / %d server, want 1 / 2", s.ClientErrorsTotal, s.ServerErrorsTotal)
	}
}

func TestHistogramBucketsAndQuantiles(t *testing.T) {
	m := NewWithBuckets([]time.Duration{10 * time.Millisecond, 100 * time.Millisecond, time.Second})

	// 90 fast requests, 9 medium, 1 slow: p50 is fast, p95 is medium, p99 is
	// medium, and the single slow outlier is invisible to p99 but not to max.
	for i := 0; i < 90; i++ {
		m.Observe(200, 5*time.Millisecond)
	}
	for i := 0; i < 9; i++ {
		m.Observe(200, 50*time.Millisecond)
	}
	m.Observe(200, 800*time.Millisecond)

	l := m.Snapshot().Latency
	if l.Count != 100 {
		t.Fatalf("count = %d, want 100", l.Count)
	}
	if l.P50Seconds != 0.010 || l.P95Seconds != 0.100 || l.P99Seconds != 0.100 {
		t.Errorf("p50/p95/p99 = %v/%v/%v, want 0.01/0.1/0.1", l.P50Seconds, l.P95Seconds, l.P99Seconds)
	}
	wantCumulative := []int64{90, 99, 100}
	for i, b := range l.Buckets {
		if b.Count != wantCumulative[i] {
			t.Errorf("bucket %d cumulative = %d, want %d", i, b.Count, wantCumulative[i])
		}
	}
}

func TestOverflowBucketSaturatesQuantile(t *testing.T) {
	m := NewWithBuckets([]time.Duration{time.Millisecond})
	m.Observe(200, time.Hour)
	if got := m.Snapshot().Latency.P99Seconds; got != 0.001 {
		t.Fatalf("p99 = %v, want saturation at the largest bound 0.001", got)
	}
}

func TestEmptyMetricsHaveZeroQuantiles(t *testing.T) {
	l := New().Snapshot().Latency
	if l.P50Seconds != 0 || l.P99Seconds != 0 || l.Count != 0 {
		t.Fatalf("unexpected latency for empty metrics: %+v", l)
	}
}

// TestConcurrentObserveIsExact is the executable answer to "why atomics".
//
// It deliberately calls ONLY Observe from each goroutine. Mixing in other
// atomic operations on shared variables (for example the in-flight gauge)
// would create happens-before edges between goroutines and hide a
// non-atomic counter from the race detector. With a plain int64 and
// "count++" this test reports a data race under -race, and on multi-core
// hardware also loses increments.
func TestConcurrentObserveIsExact(t *testing.T) {
	m := New()
	const goroutines, per = 32, 1000

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				m.Observe(200, time.Millisecond)
			}
		}()
	}
	wg.Wait()

	s := m.Snapshot()
	if s.RequestsTotal != goroutines*per {
		t.Errorf("RequestsTotal = %d, want %d", s.RequestsTotal, goroutines*per)
	}
	if s.Latency.Count != goroutines*per || s.ResponsesByClass["2xx"] != goroutines*per {
		t.Errorf("histogram count / 2xx = %d / %d, want %d", s.Latency.Count, s.ResponsesByClass["2xx"], goroutines*per)
	}
}

func TestConcurrentInFlightReturnsToZero(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				m.IncInFlight()
				m.DecInFlight()
			}
		}()
	}
	wg.Wait()
	if got := m.Snapshot().RequestsInFlight; got != 0 {
		t.Fatalf("RequestsInFlight = %d, want 0", got)
	}
}

func TestConnStateTracksOpenConnections(t *testing.T) {
	m := New()
	m.ConnState(nil, http.StateNew)
	m.ConnState(nil, http.StateNew)
	m.ConnState(nil, http.StateActive) // transitions between live states are ignored
	m.ConnState(nil, http.StateIdle)
	m.ConnState(nil, http.StateClosed)
	m.ConnState(nil, http.StateHijacked)
	if got := m.Snapshot().ActiveConnections; got != 0 {
		t.Fatalf("ActiveConnections = %d, want 0", got)
	}
	m.ConnState(nil, http.StateNew)
	if got := m.Snapshot().ActiveConnections; got != 1 {
		t.Fatalf("ActiveConnections = %d, want 1", got)
	}
}

func TestGaugesAreReadAtSnapshotTime(t *testing.T) {
	m := New()
	v := int64(1)
	m.RegisterGauge("depth", func() int64 { return v })
	if got := m.Snapshot().Gauges["depth"]; got != 1 {
		t.Fatalf("gauge = %d, want 1", got)
	}
	v = 42
	if got := m.Snapshot().Gauges["depth"]; got != 42 {
		t.Fatalf("gauge = %d, want 42", got)
	}
}

func TestHandlerServesJSON(t *testing.T) {
	m := New()
	m.Observe(200, time.Millisecond)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var s Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("body is not a Snapshot: %v", err)
	}
	if s.RequestsTotal != 1 {
		t.Errorf("RequestsTotal = %d, want 1", s.RequestsTotal)
	}
}

func BenchmarkObserveParallel(b *testing.B) {
	m := New()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.Observe(200, 3*time.Millisecond)
		}
	})
}
