package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/michael-emmanuel/go-production-http-server/internal/metrics"
)

// discardLogger keeps the benchmark measuring middleware cost, not terminal I/O.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func noopHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

// BenchmarkMiddlewareOverhead compares a bare handler with the production
// middleware stack around the same handler. The difference is the per-request
// cost of request ID generation, access logging, metrics, and the recover
// wrapper. It says nothing about network or scheduler behavior.
func BenchmarkMiddlewareOverhead(b *testing.B) {
	log := discardLogger()
	stack := Chain(noopHandler(), RequestID(), AccessLog(log), Metrics(metrics.New()), Recovery(log))

	for name, h := range map[string]http.Handler{"bare": noopHandler(), "full_stack": stack} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
			for i := 0; i < b.N; i++ {
				h.ServeHTTP(httptest.NewRecorder(), req)
			}
		})
	}
}

// BenchmarkMiddlewareOverheadParallel shows the same stack under contention,
// where shared atomics in the metrics middleware become the interesting part.
func BenchmarkMiddlewareOverheadParallel(b *testing.B) {
	log := discardLogger()
	h := Chain(noopHandler(), RequestID(), AccessLog(log), Metrics(metrics.New()), Recovery(log))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
		for pb.Next() {
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
	})
}
