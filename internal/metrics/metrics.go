// Package metrics is a deliberately small in-process metrics registry.
//
// It exists to show the primitives behind tools like Prometheus client
// libraries: atomic counters for values written by many goroutines, a
// fixed-bucket histogram for latency, and gauges read on demand. It is not a
// replacement for a real metrics library. It has no labels, no exposition
// format standard, and no cardinality protection.
package metrics

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultBuckets are the latency histogram upper bounds. They are spaced
// roughly exponentially because latency distributions are long-tailed: fine
// resolution is needed near the median, coarse resolution is fine in the tail.
var DefaultBuckets = []time.Duration{
	1 * time.Millisecond,
	2 * time.Millisecond,
	5 * time.Millisecond,
	10 * time.Millisecond,
	25 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2500 * time.Millisecond,
	5 * time.Second,
	10 * time.Second,
}

// Metrics aggregates counters shared by every request goroutine.
//
// Every field written on the request path is an atomic. A plain int64 with
// "count++" would be a data race because net/http runs handlers concurrently;
// a mutex would work but would serialize every request on one lock just to
// bump a number. Atomics are the right tool for independent counters. They
// are the wrong tool when several values must change together consistently,
// which is why Snapshot is documented as approximate.
//
// A Metrics value must not be copied after first use.
type Metrics struct {
	requestsTotal atomic.Int64
	inFlight      atomic.Int64
	activeConns   atomic.Int64
	// byClass is indexed by status/100; index 0 collects out-of-range codes.
	byClass [6]atomic.Int64

	bounds   []time.Duration
	counts   []atomic.Int64 // len(bounds)+1; the last slot is the overflow bucket
	sumNanos atomic.Int64

	mu     sync.RWMutex
	gauges map[string]func() int64
}

// New returns a Metrics using DefaultBuckets.
func New() *Metrics {
	return NewWithBuckets(DefaultBuckets)
}

// NewWithBuckets returns a Metrics with custom, strictly ascending bounds.
func NewWithBuckets(bounds []time.Duration) *Metrics {
	b := append([]time.Duration(nil), bounds...)
	return &Metrics{
		bounds: b,
		counts: make([]atomic.Int64, len(b)+1),
		gauges: make(map[string]func() int64),
	}
}

// IncInFlight records a request entering the server.
func (m *Metrics) IncInFlight() { m.inFlight.Add(1) }

// DecInFlight records a request leaving the server.
func (m *Metrics) DecInFlight() { m.inFlight.Add(-1) }

// Observe records one completed request.
func (m *Metrics) Observe(status int, d time.Duration) {
	m.requestsTotal.Add(1)

	class := status / 100
	if class < 1 || class > 5 {
		class = 0
	}
	m.byClass[class].Add(1)

	// First bucket whose upper bound is >= d; len(bounds) means overflow.
	i := sort.Search(len(m.bounds), func(i int) bool { return d <= m.bounds[i] })
	m.counts[i].Add(1)
	m.sumNanos.Add(int64(d))
}

// ConnState is an http.Server.ConnState hook that tracks open connections.
//
// A TCP connection is not an HTTP request: with keep-alive one connection
// carries many requests, and an idle connection carries none. Tracking both
// makes that difference observable.
func (m *Metrics) ConnState(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		m.activeConns.Add(1)
	case http.StateHijacked, http.StateClosed:
		m.activeConns.Add(-1)
	}
}

// RegisterGauge registers a value read lazily at snapshot time. fn must be
// cheap and safe for concurrent use.
func (m *Metrics) RegisterGauge(name string, fn func() int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[name] = fn
}

// Bucket is one cumulative histogram bucket.
type Bucket struct {
	LeSeconds float64 `json:"le_seconds"`
	Count     int64   `json:"count"`
}

// Latency summarizes the request duration histogram.
type Latency struct {
	Count      int64    `json:"count"`
	SumSeconds float64  `json:"sum_seconds"`
	Buckets    []Bucket `json:"buckets"`
	// Quantiles are upper-bound estimates: the upper edge of the bucket in
	// which the quantile falls. They saturate at the largest finite bound.
	P50Seconds float64 `json:"p50_seconds"`
	P95Seconds float64 `json:"p95_seconds"`
	P99Seconds float64 `json:"p99_seconds"`
}

// Snapshot is a point-in-time copy of all metrics. Individual values are read
// atomically but not as one atomic cut, so totals may differ by in-flight
// updates. That is acceptable for monitoring and unacceptable for accounting.
type Snapshot struct {
	RequestsTotal     int64            `json:"requests_total"`
	RequestsInFlight  int64            `json:"requests_in_flight"`
	ActiveConnections int64            `json:"active_connections"`
	ResponsesByClass  map[string]int64 `json:"responses_by_class"`
	ClientErrorsTotal int64            `json:"client_errors_total"`
	ServerErrorsTotal int64            `json:"server_errors_total"`
	Latency           Latency          `json:"latency"`
	Gauges            map[string]int64 `json:"gauges"`
}

// Snapshot copies the current values.
func (m *Metrics) Snapshot() Snapshot {
	counts := make([]int64, len(m.counts))
	var total int64
	for i := range m.counts {
		counts[i] = m.counts[i].Load()
		total += counts[i]
	}

	buckets := make([]Bucket, len(m.bounds))
	var cumulative int64
	for i, b := range m.bounds {
		cumulative += counts[i]
		buckets[i] = Bucket{LeSeconds: b.Seconds(), Count: cumulative}
	}

	s := Snapshot{
		RequestsTotal:     m.requestsTotal.Load(),
		RequestsInFlight:  m.inFlight.Load(),
		ActiveConnections: m.activeConns.Load(),
		ResponsesByClass: map[string]int64{
			"1xx": m.byClass[1].Load(),
			"2xx": m.byClass[2].Load(),
			"3xx": m.byClass[3].Load(),
			"4xx": m.byClass[4].Load(),
			"5xx": m.byClass[5].Load(),
		},
		ClientErrorsTotal: m.byClass[4].Load(),
		ServerErrorsTotal: m.byClass[5].Load(),
		Latency: Latency{
			Count:      total,
			SumSeconds: time.Duration(m.sumNanos.Load()).Seconds(),
			Buckets:    buckets,
			P50Seconds: m.quantile(counts, total, 0.50),
			P95Seconds: m.quantile(counts, total, 0.95),
			P99Seconds: m.quantile(counts, total, 0.99),
		},
		Gauges: make(map[string]int64),
	}

	// Copy the gauge functions out before calling them so no foreign code
	// runs while holding the lock.
	m.mu.RLock()
	fns := make(map[string]func() int64, len(m.gauges))
	for k, fn := range m.gauges {
		fns[k] = fn
	}
	m.mu.RUnlock()
	for k, fn := range fns {
		s.Gauges[k] = fn()
	}
	return s
}

// quantile returns the upper bound, in seconds, of the bucket containing the
// q-th quantile. It returns 0 when nothing has been observed.
func (m *Metrics) quantile(counts []int64, total int64, q float64) float64 {
	if total == 0 || len(m.bounds) == 0 {
		return 0
	}
	rank := int64(math.Ceil(q * float64(total)))
	if rank < 1 {
		rank = 1
	}
	var cumulative int64
	for i, c := range counts {
		cumulative += c
		if cumulative >= rank {
			if i >= len(m.bounds) {
				return m.bounds[len(m.bounds)-1].Seconds()
			}
			return m.bounds[i].Seconds()
		}
	}
	return m.bounds[len(m.bounds)-1].Seconds()
}

// Handler serves the snapshot as JSON.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := json.Marshal(m.Snapshot())
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		// A failed write means the client went away; there is nothing to do.
		_, _ = w.Write(append(body, '\n'))
	})
}
