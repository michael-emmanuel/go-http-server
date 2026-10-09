package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/michael-emmanuel/go-production-http-server/internal/logging"
	"github.com/michael-emmanuel/go-production-http-server/internal/metrics"
	"github.com/michael-emmanuel/go-production-http-server/internal/requestid"
)

func serve(h http.Handler, method, target string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func panicHandler() http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("kaboom") })
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestChainOrderOutermostFirst(t *testing.T) {
	var trace []string
	mark := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				trace = append(trace, name+":in")
				next.ServeHTTP(w, r)
				trace = append(trace, name+":out")
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { trace = append(trace, "handler") }),
		mark("A"), mark("B"), mark("C"))
	serve(h, http.MethodGet, "/")

	want := "A:in B:in C:in handler C:out B:out A:out"
	if got := strings.Join(trace, " "); got != want {
		t.Fatalf("trace = %s\nwant    %s", got, want)
	}
}

func TestChainWithNoMiddlewareReturnsHandler(t *testing.T) {
	if rec := serve(Chain(okHandler()), http.MethodGet, "/"); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestRequestIDGeneratedPropagatedAndEchoed(t *testing.T) {
	var inCtx string
	h := Chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { inCtx = requestid.FromContext(r.Context()) }), RequestID())
	rec := serve(h, http.MethodGet, "/")

	got := rec.Header().Get(requestid.Header)
	if !requestid.Valid(got) || got != inCtx {
		t.Fatalf("header %q, context %q: want the same valid id in both", got, inCtx)
	}
}

func TestRequestIDHonorsValidInboundAndReplacesInvalid(t *testing.T) {
	var inCtx string
	h := Chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { inCtx = requestid.FromContext(r.Context()) }), RequestID())

	serve(h, http.MethodGet, "/", requestid.Header, "caller-supplied-1")
	if inCtx != "caller-supplied-1" {
		t.Errorf("valid inbound id was not honored, got %q", inCtx)
	}

	rec := serve(h, http.MethodGet, "/", requestid.Header, "not valid: has spaces")
	if inCtx == "not valid: has spaces" || !requestid.Valid(inCtx) || rec.Header().Get(requestid.Header) != inCtx {
		t.Errorf("invalid inbound id must be replaced, got %q", inCtx)
	}
}

func TestRecoveryReturnsJSON500AndProcessSurvives(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, slog.LevelInfo)
	h := Chain(panicHandler(), RequestID(), Recovery(log))

	rec := serve(h, http.MethodGet, "/")
	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "internal_error" || body.Error.RequestID == "" {
		t.Errorf("body = %+v", body)
	}
	if strings.Contains(rec.Body.String(), "kaboom") || strings.Contains(rec.Body.String(), "goroutine") {
		t.Errorf("panic value or stack leaked to the client: %s", rec.Body)
	}

	lines := logLines(t, &buf)
	if len(lines) != 1 || lines[0]["panic"] != "kaboom" || lines[0]["request_id"] != body.Error.RequestID || !strings.Contains(lines[0]["stack"].(string), "goroutine") {
		t.Errorf("panic log should carry value, stack and the same request id: %v", lines)
	}

	// The same middleware instance keeps serving after a panic.
	if rec := serve(Chain(okHandler(), Recovery(log)), http.MethodGet, "/"); rec.Code != 200 {
		t.Errorf("post-panic request status = %d", rec.Code)
	}
}

func mustPanicWith(t *testing.T, want any, fn func()) {
	t.Helper()
	defer func() {
		if got := recover(); got != want {
			t.Fatalf("recovered %v, want %v", got, want)
		}
	}()
	fn()
	t.Fatal("expected a panic")
}

func TestRecoveryRepanicsErrAbortHandler(t *testing.T) {
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }),
		Recovery(logging.New(&bytes.Buffer{}, slog.LevelInfo)))
	mustPanicWith(t, http.ErrAbortHandler, func() { serve(h, http.MethodGet, "/") })
}

func TestRecoveryAbortsConnectionWhenResponseAlreadyStarted(t *testing.T) {
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("late failure")
	}), Recovery(logging.New(&bytes.Buffer{}, slog.LevelInfo)))
	// A 500 cannot replace an already-sent 200, so the only honest signal to
	// the client is to abort the connection.
	mustPanicWith(t, http.ErrAbortHandler, func() { serve(h, http.MethodGet, "/") })
}

// TestMiddlewareOrderingDeterminesWhatIsObserved is the executable form of
// ADR-003: the same panicking handler is logged as 500 when Recovery is inside
// AccessLog, but as a misleading 200 when Recovery is outside it.
func TestMiddlewareOrderingDeterminesWhatIsObserved(t *testing.T) {
	statusLogged := func(order func(log *slog.Logger) http.Handler) (logged any, responded int) {
		var buf bytes.Buffer
		log := logging.New(&buf, slog.LevelDebug)
		rec := serve(order(log), http.MethodGet, "/x")
		for _, l := range logLines(t, &buf) {
			if l["msg"] == "request completed" {
				logged = l["status"]
			}
		}
		return logged, rec.Code
	}

	logged, responded := statusLogged(func(log *slog.Logger) http.Handler {
		return Chain(panicHandler(), RequestID(), AccessLog(log), Recovery(log)) // Recovery inside
	})
	if responded != 500 || logged != float64(500) {
		t.Errorf("Recovery inside AccessLog: responded %d, logged %v; want both 500", responded, logged)
	}

	logged, responded = statusLogged(func(log *slog.Logger) http.Handler {
		return Chain(panicHandler(), RequestID(), Recovery(log), AccessLog(log)) // Recovery outside
	})
	if responded != 500 || logged != float64(200) {
		t.Errorf("Recovery outside AccessLog: responded %d, logged %v; want 500 responded but 200 logged", responded, logged)
	}
}

func TestAccessLogFields(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, slog.LevelDebug)
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("12345"))
	}), RequestID(), AccessLog(log))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/work?token=SECRET", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("Authorization", "Bearer SECRET")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(buf.String(), "SECRET") {
		t.Fatalf("query string or headers leaked into the access log: %s", buf.String())
	}
	lines := logLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("want exactly one access log line, got %d", len(lines))
	}
	l := lines[0]
	if l["method"] != "POST" || l["path"] != "/api/v1/work" || l["status"] != float64(201) || l["bytes"] != float64(5) ||
		l["remote_addr"] != "203.0.113.9:5555" || l["request_id"] == nil || l["duration_ms"] == nil {
		t.Fatalf("unexpected fields: %v", l)
	}
}

func TestAccessLogLevels(t *testing.T) {
	levelFor := func(path string, status int, min slog.Level) (string, bool) {
		var buf bytes.Buffer
		h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }), AccessLog(logging.New(&buf, min)))
		serve(h, http.MethodGet, path)
		lines := logLines(t, &buf)
		if len(lines) == 0 {
			return "", false
		}
		return lines[0]["level"].(string), true
	}

	if lvl, _ := levelFor("/api/v1/info", 200, slog.LevelInfo); lvl != "INFO" {
		t.Errorf("200 level = %s", lvl)
	}
	if lvl, _ := levelFor("/api/v1/info", 404, slog.LevelInfo); lvl != "WARN" {
		t.Errorf("404 level = %s", lvl)
	}
	if lvl, _ := levelFor("/api/v1/info", 503, slog.LevelInfo); lvl != "ERROR" {
		t.Errorf("503 level = %s", lvl)
	}
	if _, logged := levelFor("/health/live", 200, slog.LevelInfo); logged {
		t.Error("successful probe requests must not be logged at info level")
	}
	if lvl, logged := levelFor("/health/ready", 200, slog.LevelDebug); !logged || lvl != "DEBUG" {
		t.Errorf("probe at debug level: %s, logged=%v", lvl, logged)
	}
	if lvl, _ := levelFor("/health/ready", 503, slog.LevelInfo); lvl != "ERROR" {
		t.Errorf("failing probe level = %s, want ERROR so it is never hidden", lvl)
	}
}

func TestAccessLogDefaultsStatusTo200(t *testing.T) {
	var buf bytes.Buffer
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), AccessLog(logging.New(&buf, slog.LevelInfo)))
	serve(h, http.MethodGet, "/")
	if l := logLines(t, &buf); l[0]["status"] != float64(200) {
		t.Fatalf("a handler that writes nothing implicitly responds 200, got %v", l[0]["status"])
	}
}

func TestMetricsMiddleware(t *testing.T) {
	m := metrics.New()
	var inFlightDuring int64
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inFlightDuring = m.Snapshot().RequestsInFlight
		w.WriteHeader(http.StatusTeapot)
	}), Metrics(m))

	serve(h, http.MethodGet, "/")
	serve(Chain(okHandler(), Metrics(m)), http.MethodGet, "/")

	s := m.Snapshot()
	if inFlightDuring != 1 || s.RequestsInFlight != 0 {
		t.Errorf("in flight during/after = %d/%d, want 1/0", inFlightDuring, s.RequestsInFlight)
	}
	if s.RequestsTotal != 2 || s.ResponsesByClass["4xx"] != 1 || s.ResponsesByClass["2xx"] != 1 || s.Latency.Count != 2 {
		t.Errorf("snapshot = %+v", s)
	}
}

func TestMetricsMiddlewareDecrementsInFlightOnPanic(t *testing.T) {
	m := metrics.New()
	mustPanicWith(t, "kaboom", func() { serve(Chain(panicHandler(), Metrics(m)), http.MethodGet, "/") })
	if got := m.Snapshot().RequestsInFlight; got != 0 {
		t.Fatalf("in flight = %d after panic, want 0 (gauge must not leak)", got)
	}
}

func TestAuth(t *testing.T) {
	protected := Chain(okHandler(), Auth("s3cret"))

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"missing", "", 401},
		{"wrong scheme", "Basic s3cret", 401},
		{"wrong token", "Bearer nope", 401},
		{"prefix of token", "Bearer s3cre", 401},
		{"empty token", "Bearer ", 401},
		{"correct", "Bearer s3cret", 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var rec *httptest.ResponseRecorder
			if tc.header == "" {
				rec = serve(protected, http.MethodGet, "/")
			} else {
				rec = serve(protected, http.MethodGet, "/", "Authorization", tc.header)
			}
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == 401 {
				if rec.Header().Get("WWW-Authenticate") == "" {
					t.Error("401 must carry WWW-Authenticate")
				}
				if !strings.Contains(rec.Body.String(), `"unauthorized"`) {
					t.Errorf("401 must use the standard error envelope: %s", rec.Body)
				}
			}
		})
	}
}

func TestAuthDisabledWhenTokenEmpty(t *testing.T) {
	if rec := serve(Chain(okHandler(), Auth("")), http.MethodGet, "/"); rec.Code != 200 {
		t.Fatalf("status = %d, want 200 with auth disabled", rec.Code)
	}
}

func TestRequireJSON(t *testing.T) {
	h := Chain(okHandler(), RequireJSON())
	tests := []struct {
		method, ct string
		want       int
	}{
		{http.MethodPost, "application/json", 200},
		{http.MethodPost, "application/json; charset=utf-8", 200},
		{http.MethodPost, "APPLICATION/JSON", 200},
		{http.MethodPost, "", 415},
		{http.MethodPost, "text/plain", 415},
		{http.MethodPost, "application/x-www-form-urlencoded", 415},
		{http.MethodPost, "application/jsonx", 415},
		{http.MethodPut, "text/plain", 415},
		{http.MethodPatch, "", 415},
		{http.MethodGet, "", 200},
		{http.MethodDelete, "", 200},
	}
	for _, tc := range tests {
		var rec *httptest.ResponseRecorder
		if tc.ct == "" {
			rec = serve(h, tc.method, "/")
		} else {
			rec = serve(h, tc.method, "/", "Content-Type", tc.ct)
		}
		if rec.Code != tc.want {
			t.Errorf("%s with %q: status %d, want %d", tc.method, tc.ct, rec.Code, tc.want)
		}
	}
}

func TestRequestTimeoutSetsDeadlineAndReleasesIt(t *testing.T) {
	var (
		deadline time.Time
		hasDl    bool
		captured context.Context
	)
	h := Chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = r.Context()
		deadline, hasDl = r.Context().Deadline()
	}), RequestTimeout(time.Minute))

	before := time.Now()
	serve(h, http.MethodGet, "/")

	if !hasDl || deadline.Before(before.Add(59*time.Second)) || deadline.After(before.Add(61*time.Second)) {
		t.Fatalf("deadline = %v (has=%v), want ~1m from now", deadline, hasDl)
	}
	if captured.Err() == nil {
		t.Fatal("context must be cancelled when the handler returns, releasing its timer")
	}
}

func TestRequestTimeoutExpiryIsObservedByHandler(t *testing.T) {
	var got error
	h := Chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		got = r.Context().Err()
	}), RequestTimeout(time.Millisecond))
	serve(h, http.MethodGet, "/")
	if got != context.DeadlineExceeded {
		t.Fatalf("ctx err = %v, want DeadlineExceeded", got)
	}
}

func TestResponseRecorder(t *testing.T) {
	under := httptest.NewRecorder()
	rr := recorderFor(under)
	if rr.status != 200 {
		t.Fatalf("initial status = %d, want 200", rr.status)
	}
	if again := recorderFor(rr); again != rr {
		t.Fatal("recorderFor must reuse an existing recorder, not stack wrappers")
	}

	rr.WriteHeader(http.StatusEarlyHints) // 103 must not be mistaken for the final status
	if rr.wroteHeader {
		t.Fatal("1xx informational response marked the header as written")
	}
	rr.WriteHeader(http.StatusAccepted)
	rr.WriteHeader(http.StatusTeapot) // ignored, as net/http ignores it
	if rr.status != http.StatusAccepted {
		t.Fatalf("status = %d, want first final status 202", rr.status)
	}

	if rr.Unwrap() != under {
		t.Error("Unwrap must return the underlying writer")
	}
	// http.ResponseController reaches Flush through Unwrap.
	if err := http.NewResponseController(rr).Flush(); err != nil {
		t.Errorf("Flush via ResponseController: %v", err)
	}
}

// TestProductionChainRecordsPanicAs500 tests the real production ordering (not
// a hand-built chain): a panic must be visible as a 500 to the access log and
// the metrics, and the response must carry a request ID.
func TestProductionChainRecordsPanicAs500(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, slog.LevelDebug)
	m := metrics.New()
	h := newRootHandler(panicHandler(), log, m)

	rec := serve(h, http.MethodGet, "/boom")
	if rec.Code != 500 || rec.Header().Get(requestid.Header) == "" {
		t.Fatalf("status %d, request id %q", rec.Code, rec.Header().Get(requestid.Header))
	}
	var status, loggedID any
	for _, l := range logLines(t, &buf) {
		if l["msg"] == "request completed" {
			status, loggedID = l["status"], l["request_id"]
		}
	}
	if want := rec.Header().Get(requestid.Header); loggedID != want {
		t.Errorf("access log request_id = %v, want %q: RequestID must run before AccessLog or logs cannot be correlated", loggedID, want)
	}
	if status != float64(500) {
		t.Errorf("access log recorded status %v, want 500 (Recovery must sit inside AccessLog)", status)
	}
	if got := m.Snapshot().ResponsesByClass["5xx"]; got != 1 {
		t.Errorf("metrics recorded %d 5xx responses, want 1 (Recovery must sit inside Metrics)", got)
	}
}

func TestSleepContext(t *testing.T) {
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("uninterrupted sleep: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// An hour-long wait returning at once proves cancellation interrupts it.
	if err := sleepContext(ctx, time.Hour); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
