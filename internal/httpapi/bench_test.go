package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/michael-emmanuel/go-production-http-server/internal/domain"
)

func benchRouter(b *testing.B) http.Handler {
	b.Helper()
	w := fakeWork{
		runBatch: func(context.Context, domain.BatchRequest) (domain.BatchResult, error) {
			return domain.BatchResult{Items: 10, Checksum: 285, Concurrency: 8}, nil
		},
		submitJob: func(_ context.Context, req domain.BatchRequest) (domain.Job, error) {
			return domain.Job{ID: "abc", Request: req, Status: domain.StatusQueued}, nil
		},
	}
	return newRouter(b, w, nil)
}

// BenchmarkRequestHandling measures routing, handler logic, and JSON encoding
// with a stub service, isolating the transport layer from application cost.
// It excludes the network stack and the middleware chain.
func BenchmarkRequestHandling(b *testing.B) {
	router := benchRouter(b)

	b.Run("GET_work", func(b *testing.B) {
		b.ReportAllocs()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/work?items=10", nil)
		for i := 0; i < b.N; i++ {
			router.ServeHTTP(httptest.NewRecorder(), req)
		}
	})

	b.Run("POST_work_json_decode", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/work", strings.NewReader(`{"items":10,"delay_ms":5}`))
			router.ServeHTTP(httptest.NewRecorder(), req)
		}
	})

	b.Run("404_via_probe_writer", func(b *testing.B) {
		b.ReportAllocs()
		req := httptest.NewRequest(http.MethodGet, "/nope", nil)
		for i := 0; i < b.N; i++ {
			router.ServeHTTP(httptest.NewRecorder(), req)
		}
	})
}
