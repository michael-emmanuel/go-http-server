package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/michael-emmanuel/go-production-http-server/internal/domain"
	"github.com/michael-emmanuel/go-production-http-server/internal/health"
	"github.com/michael-emmanuel/go-production-http-server/internal/requestid"
)

// fakeWork lets each test script the application layer's behavior.
type fakeWork struct {
	runBatch  func(ctx context.Context, req domain.BatchRequest) (domain.BatchResult, error)
	submitJob func(ctx context.Context, req domain.BatchRequest) (domain.Job, error)
	getJob    func(ctx context.Context, id string) (domain.Job, error)
}

func (f fakeWork) RunBatch(ctx context.Context, req domain.BatchRequest) (domain.BatchResult, error) {
	return f.runBatch(ctx, req)
}
func (f fakeWork) SubmitJob(ctx context.Context, req domain.BatchRequest) (domain.Job, error) {
	return f.submitJob(ctx, req)
}
func (f fakeWork) GetJob(ctx context.Context, id string) (domain.Job, error) {
	return f.getJob(ctx, id)
}

func newHandler(t testing.TB, w WorkService, maxBody int64) *Handler {
	t.Helper()
	h, err := NewHandler(Deps{
		Work:         w,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBodyBytes: maxBody,
		Version:      "test-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func okBatch(context.Context, domain.BatchRequest) (domain.BatchResult, error) {
	return domain.BatchResult{Items: 3, Checksum: 5, Concurrency: 3, Elapsed: 1500 * time.Microsecond}, nil
}

func newRouter(t testing.TB, w WorkService, api func(http.Handler) http.Handler) http.Handler {
	t.Helper()
	stub := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write([]byte("metrics")) })
	return Routes(RouteDeps{Handler: newHandler(t, w, 1024), Health: health.New(), Metrics: stub, API: api})
}

func do(h http.Handler, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type errEnvelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) errEnvelope {
	t.Helper()
	var e errEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body is not the error envelope: %v\n%s", err, rec.Body)
	}
	return e
}

func TestNewHandlerRequiresDependencies(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := NewHandler(Deps{Logger: log, MaxBodyBytes: 1}); err == nil {
		t.Error("missing Work must fail")
	}
	if _, err := NewHandler(Deps{Work: fakeWork{}, MaxBodyBytes: 1}); err == nil {
		t.Error("missing Logger must fail")
	}
	if _, err := NewHandler(Deps{Work: fakeWork{}, Logger: log}); err == nil {
		t.Error("non-positive MaxBodyBytes must fail")
	}
}

func TestInfo(t *testing.T) {
	rec := do(newRouter(t, fakeWork{}, nil), http.MethodGet, "/api/v1/info", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got infoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Service != "go-production-http-server" || got.Version != "test-1" || got.GoVersion == "" || got.Goroutines < 1 || got.GOMAXPROCS < 1 {
		t.Fatalf("unexpected info: %+v", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff header")
	}
}

func TestRunWorkPassesParametersAndRequestContext(t *testing.T) {
	type ctxKey struct{}
	var gotReq domain.BatchRequest
	var sawContextValue any
	w := fakeWork{runBatch: func(ctx context.Context, req domain.BatchRequest) (domain.BatchResult, error) {
		gotReq, sawContextValue = req, ctx.Value(ctxKey{})
		return okBatch(ctx, req)
	}}
	h := newHandler(t, w, 1024)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work?items=3&delay_ms=25", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, "from-request"))
	rec := httptest.NewRecorder()
	h.RunWork(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if gotReq.Items != 3 || gotReq.ItemDelay != 25*time.Millisecond {
		t.Errorf("request = %+v", gotReq)
	}
	if sawContextValue != "from-request" {
		t.Error("handler must pass the request's context (not context.Background) to the service")
	}
	var body batchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Items != 3 || body.Checksum != 5 || body.Concurrency != 3 || body.ElapsedMS != 1.5 {
		t.Errorf("body = %+v", body)
	}
}

func TestRunWorkDefaults(t *testing.T) {
	var got domain.BatchRequest
	w := fakeWork{runBatch: func(ctx context.Context, req domain.BatchRequest) (domain.BatchResult, error) {
		got = req
		return okBatch(ctx, req)
	}}
	if rec := do(newRouter(t, w, nil), http.MethodGet, "/api/v1/work", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got.Items != 10 || got.ItemDelay != 10*time.Millisecond {
		t.Fatalf("defaults = %+v, want items=10 delay=10ms", got)
	}
}

func TestRunWorkRejectsBadQueryParameters(t *testing.T) {
	called := false
	w := fakeWork{runBatch: func(ctx context.Context, req domain.BatchRequest) (domain.BatchResult, error) {
		called = true
		return okBatch(ctx, req)
	}}
	router := newRouter(t, w, nil)
	for _, target := range []string{
		"/api/v1/work?items=abc",
		"/api/v1/work?items=1.5",
		"/api/v1/work?items=", // empty means default, so used below as a control
		"/api/v1/work?delay_ms=fast",
		"/api/v1/work?items=99999999999999999999",
	} {
		called = false
		rec := do(router, http.MethodGet, target, "")
		if strings.HasSuffix(target, "items=") {
			if rec.Code != http.StatusOK {
				t.Errorf("%s: empty value should fall back to default, got %d", target, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, rec.Code)
		}
		if e := decodeErr(t, rec); e.Error.Code != "invalid_request" {
			t.Errorf("%s: code = %q", target, e.Error.Code)
		}
		if called {
			t.Errorf("%s: service must not be called for unparseable input", target)
		}
	}
}

func TestServiceValidationErrorBecomes400WithFieldName(t *testing.T) {
	w := fakeWork{runBatch: func(context.Context, domain.BatchRequest) (domain.BatchResult, error) {
		return domain.BatchResult{}, fmt.Errorf("run: %w", &domain.ValidationError{Field: "items", Reason: "must be between 1 and 10"})
	}}
	rec := do(newRouter(t, w, nil), http.MethodGet, "/api/v1/work?items=99", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if e := decodeErr(t, rec); !strings.Contains(e.Error.Message, "items must be between 1 and 10") {
		t.Fatalf("message = %q", e.Error.Message)
	}
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		status     int
		code       string
		retryAfter bool
	}{
		{"not found", fmt.Errorf("job x: %w", domain.ErrNotFound), 404, "not_found", false},
		{"overloaded", fmt.Errorf("submit: %w", domain.ErrOverloaded), 503, "overloaded", true},
		{"unavailable", fmt.Errorf("submit: %w", domain.ErrUnavailable), 503, "unavailable", true},
		{"client canceled", fmt.Errorf("run: %w", context.Canceled), StatusClientClosedRequest, "client_closed_request", false},
		{"deadline", fmt.Errorf("run: %w", context.DeadlineExceeded), 504, "timeout", false},
		{"internal", errors.New("pq: password authentication failed for user \"svc\""), 500, "internal_error", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := fakeWork{runBatch: func(context.Context, domain.BatchRequest) (domain.BatchResult, error) {
				return domain.BatchResult{}, tc.err
			}}
			rec := do(newRouter(t, w, nil), http.MethodGet, "/api/v1/work", "")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if e := decodeErr(t, rec); e.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", e.Error.Code, tc.code)
			}
			if got := rec.Header().Get("Retry-After") != ""; got != tc.retryAfter {
				t.Errorf("Retry-After present = %v, want %v", got, tc.retryAfter)
			}
		})
	}
}

func TestInternalErrorsNeverLeakToClients(t *testing.T) {
	const secret = "postgres://admin:hunter2@10.1.2.3/db"
	w := fakeWork{runBatch: func(context.Context, domain.BatchRequest) (domain.BatchResult, error) {
		return domain.BatchResult{}, fmt.Errorf("load work item: %w", errors.New(secret))
	}}
	rec := do(newRouter(t, w, nil), http.MethodGet, "/api/v1/work", "")
	if rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "load work item") {
		t.Fatalf("internal detail leaked: %s", rec.Body)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassifyNetworkTimeoutAfterDeadlineExceeded(t *testing.T) {
	if f := classify(timeoutErr{}); f.status != http.StatusRequestTimeout {
		t.Errorf("net timeout status = %d, want 408", f.status)
	}
	// context.DeadlineExceeded also implements Timeout() == true; it must still
	// map to 504, which depends on case ordering in classify.
	if f := classify(context.DeadlineExceeded); f.status != http.StatusGatewayTimeout {
		t.Errorf("deadline status = %d, want 504", f.status)
	}
}

func TestErrorEnvelopeIncludesRequestID(t *testing.T) {
	w := fakeWork{runBatch: func(context.Context, domain.BatchRequest) (domain.BatchResult, error) {
		return domain.BatchResult{}, domain.ErrNotFound
	}}
	h := newRouter(t, w, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/work", nil)
	req = req.WithContext(requestid.WithContext(req.Context(), "rid-42"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if e := decodeErr(t, rec); e.Error.RequestID != "rid-42" {
		t.Fatalf("request_id = %q", e.Error.RequestID)
	}
}

func sampleJob() domain.Job {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return domain.Job{
		ID:         "abc",
		Request:    domain.BatchRequest{Items: 5, ItemDelay: 20 * time.Millisecond},
		Status:     domain.StatusSucceeded,
		CreatedAt:  created,
		StartedAt:  created.Add(time.Second),
		FinishedAt: created.Add(2 * time.Second),
		Result:     &domain.BatchResult{Items: 5, Checksum: 30, Concurrency: 5, Elapsed: 2 * time.Millisecond},
	}
}

func TestSubmitWorkAccepted(t *testing.T) {
	var got domain.BatchRequest
	w := fakeWork{submitJob: func(_ context.Context, req domain.BatchRequest) (domain.Job, error) {
		got = req
		return domain.Job{ID: "abc", Request: req, Status: domain.StatusQueued, CreatedAt: time.Now()}, nil
	}}
	rec := do(newRouter(t, w, nil), http.MethodPost, "/api/v1/work", `{"items":7,"delay_ms":30}`, "Content-Type", "application/json")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); loc != "/api/v1/work/abc" {
		t.Errorf("Location = %q", loc)
	}
	if got.Items != 7 || got.ItemDelay != 30*time.Millisecond {
		t.Errorf("request = %+v", got)
	}
	var job jobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.ID != "abc" || job.Status != "queued" || job.StartedAt != nil || job.FinishedAt != nil || job.Result != nil {
		t.Errorf("job = %+v", job)
	}
}

func TestSubmitWorkAppliesDefaultsForEmptyObject(t *testing.T) {
	var got domain.BatchRequest
	w := fakeWork{submitJob: func(_ context.Context, req domain.BatchRequest) (domain.Job, error) {
		got = req
		return domain.Job{ID: "x", Request: req, Status: domain.StatusQueued}, nil
	}}
	if rec := do(newRouter(t, w, nil), http.MethodPost, "/api/v1/work", `{}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	if got.Items != 10 || got.ItemDelay != 10*time.Millisecond {
		t.Fatalf("defaults = %+v", got)
	}
}

func TestSubmitWorkRejectsBadBodies(t *testing.T) {
	called := false
	w := fakeWork{submitJob: func(context.Context, domain.BatchRequest) (domain.Job, error) {
		called = true
		return domain.Job{}, nil
	}}
	router := newRouter(t, w, nil)

	tests := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"truncated json", `{"items":`, 400, "malformed_json"},
		{"not json", `hello`, 400, "malformed_json"},
		{"empty body", ``, 400, "invalid_request"},
		{"unknown field", `{"items":1,"colour":"red"}`, 400, "invalid_request"},
		{"wrong type", `{"items":"ten"}`, 400, "invalid_request"},
		{"array not object", `[1,2]`, 400, "invalid_request"},
		{"two objects", `{"items":1}{"items":2}`, 400, "invalid_request"},
		{"trailing garbage", `{"items":1} nope`, 400, "invalid_request"},
		{"body too large", `{"items":1,"pad":"` + strings.Repeat("x", 2048) + `"}`, 413, "payload_too_large"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			rec := do(router, http.MethodPost, "/api/v1/work", tc.body, "Content-Type", "application/json")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body)
			}
			if e := decodeErr(t, rec); e.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", e.Error.Code, tc.code)
			}
			if called {
				t.Error("service must not be called for a rejected body")
			}
		})
	}
}

func TestSubmitWorkServiceErrors(t *testing.T) {
	w := fakeWork{submitJob: func(context.Context, domain.BatchRequest) (domain.Job, error) {
		return domain.Job{}, fmt.Errorf("submit job: %w", domain.ErrOverloaded)
	}}
	rec := do(newRouter(t, w, nil), http.MethodPost, "/api/v1/work", `{"items":1}`)
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d, Retry-After = %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestGetWork(t *testing.T) {
	var gotID string
	w := fakeWork{getJob: func(_ context.Context, id string) (domain.Job, error) {
		gotID = id
		if id == "missing" {
			return domain.Job{}, fmt.Errorf("job %q: %w", id, domain.ErrNotFound)
		}
		return sampleJob(), nil
	}}
	router := newRouter(t, w, nil)

	rec := do(router, http.MethodGet, "/api/v1/work/abc", "")
	if rec.Code != 200 || gotID != "abc" {
		t.Fatalf("status = %d, id = %q", rec.Code, gotID)
	}
	var job jobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Status != "succeeded" || job.Result == nil || job.Result.Checksum != 30 || job.StartedAt == nil || job.FinishedAt == nil || job.DelayMS != 20 {
		t.Fatalf("job = %+v", job)
	}

	if rec := do(router, http.MethodGet, "/api/v1/work/missing", ""); rec.Code != 404 {
		t.Fatalf("missing job status = %d, want 404", rec.Code)
	}
}

func TestRouterReturnsJSONFor404And405(t *testing.T) {
	router := newRouter(t, fakeWork{}, nil)

	rec := do(router, http.MethodGet, "/nope", "")
	if rec.Code != 404 || decodeErr(t, rec).Error.Code != "not_found" {
		t.Fatalf("404 case: %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("404 Content-Type = %q, want JSON", ct)
	}

	rec = do(router, http.MethodDelete, "/api/v1/work", "")
	if rec.Code != 405 || decodeErr(t, rec).Error.Code != "method_not_allowed" {
		t.Fatalf("405 case: %d %s", rec.Code, rec.Body)
	}
	allow := rec.Header().Get("Allow")
	if !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
		t.Errorf("Allow = %q, want it to list GET and POST", allow)
	}

	rec = do(router, http.MethodPost, "/health/live", "")
	if rec.Code != 405 {
		t.Errorf("POST to health status = %d, want 405", rec.Code)
	}
}

func TestAPIPolicyWrapsAPIRoutesButNotProbes(t *testing.T) {
	var wrapped []string
	api := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wrapped = append(wrapped, r.URL.Path)
			next.ServeHTTP(w, r)
		})
	}
	w := fakeWork{runBatch: okBatch, getJob: func(context.Context, string) (domain.Job, error) { return sampleJob(), nil }}
	router := newRouter(t, w, api)

	for _, p := range []string{"/health/live", "/health/ready", "/metrics"} {
		do(router, http.MethodGet, p, "")
	}
	if len(wrapped) != 0 {
		t.Fatalf("operational endpoints must bypass API policy, but it ran for %v", wrapped)
	}
	for _, p := range []string{"/api/v1/info", "/api/v1/work", "/api/v1/work/abc"} {
		do(router, http.MethodGet, p, "")
	}
	if len(wrapped) != 3 {
		t.Fatalf("API policy ran for %v, want all three API routes", wrapped)
	}
}

func TestHealthAndMetricsAreRouted(t *testing.T) {
	router := newRouter(t, fakeWork{}, nil)
	if rec := do(router, http.MethodGet, "/health/live", ""); rec.Code != 200 {
		t.Errorf("live = %d", rec.Code)
	}
	if rec := do(router, http.MethodGet, "/health/ready", ""); rec.Code != 503 {
		t.Errorf("ready before MarkReady = %d, want 503", rec.Code)
	}
	if rec := do(router, http.MethodGet, "/metrics", ""); rec.Body.String() != "metrics" {
		t.Errorf("metrics body = %q", rec.Body)
	}
}

func TestProbeWriter(t *testing.T) {
	p := &probeWriter{header: make(http.Header), status: http.StatusOK}
	p.WriteHeader(http.StatusTeapot)
	p.WriteHeader(http.StatusOK) // second call must not override
	if _, err := p.Write([]byte("x")); err != nil || p.status != http.StatusTeapot {
		t.Fatalf("status = %d, err = %v", p.status, err)
	}
}

func TestCancellationCauseDistinguishesShutdownFromClientDisconnect(t *testing.T) {
	w := fakeWork{runBatch: func(context.Context, domain.BatchRequest) (domain.BatchResult, error) {
		return domain.BatchResult{}, fmt.Errorf("run batch: %w", context.Canceled)
	}}
	h := newRouter(t, w, nil)

	run := func(cause error) *httptest.ResponseRecorder {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/work", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Server-initiated cancellation: the client is still waiting, so tell it
	// to retry elsewhere.
	rec := run(domain.ErrUnavailable)
	if rec.Code != 503 || decodeErr(t, rec).Error.Code != "unavailable" || rec.Header().Get("Retry-After") == "" {
		t.Errorf("shutdown cause: %d %s", rec.Code, rec.Body)
	}
	// Plain cancellation (client went away): 499 for our logs and metrics.
	rec = run(nil)
	if rec.Code != StatusClientClosedRequest {
		t.Errorf("client disconnect: %d %s", rec.Code, rec.Body)
	}
}

func TestRespondEncodingFailureBecomesCleanInternalError(t *testing.T) {
	h := newHandler(t, fakeWork{}, 1024)
	rec := httptest.NewRecorder()
	// A channel cannot be marshalled to JSON. Because respond encodes before
	// touching the ResponseWriter, the client gets a well-formed 500, not a
	// half-written 200.
	h.respond(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, make(chan int))
	if rec.Code != 500 || decodeErr(t, rec).Error.Code != "internal_error" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

// TestDelayOverflowCannotBypassValidation: 18446744073710 ms overflows int64
// nanoseconds and wraps to roughly 0.45 ms. Converting before validating would
// let it through as a "valid" delay.
func TestDelayOverflowCannotBypassValidation(t *testing.T) {
	called := false
	w := fakeWork{
		runBatch: func(ctx context.Context, req domain.BatchRequest) (domain.BatchResult, error) {
			called = true
			return okBatch(ctx, req)
		},
		submitJob: func(context.Context, domain.BatchRequest) (domain.Job, error) {
			called = true
			return domain.Job{}, nil
		},
	}
	router := newRouter(t, w, nil)

	for _, ms := range []string{"18446744073710", "9223372036854775807", "-1"} {
		called = false
		rec := do(router, http.MethodGet, "/api/v1/work?delay_ms="+ms, "")
		if rec.Code != http.StatusBadRequest || called {
			t.Errorf("GET delay_ms=%s: status %d, service called=%v; want 400 and no call", ms, rec.Code, called)
		}
		called = false
		rec = do(router, http.MethodPost, "/api/v1/work", `{"delay_ms":`+ms+`}`)
		if rec.Code != http.StatusBadRequest || called {
			t.Errorf("POST delay_ms=%s: status %d, service called=%v; want 400 and no call", ms, rec.Code, called)
		}
	}
}
