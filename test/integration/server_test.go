// Package integration tests the assembled server over real TCP connections.
//
// Where a test must wait for something inside the server, it waits on a
// channel (a work function signalling it started, a health.Checker channel
// closing) rather than sleeping. The single exception is noted where it
// occurs.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michael-emmanuel/go-production-http-server/internal/config"
	"github.com/michael-emmanuel/go-production-http-server/internal/domain"
	"github.com/michael-emmanuel/go-production-http-server/internal/logging"
	"github.com/michael-emmanuel/go-production-http-server/internal/server"
)

// guard bounds how long a test waits for an event that must happen. It is a
// failure detector, not a synchronization mechanism: on a correct run it is
// never reached.
const guard = 15 * time.Second

type harness struct {
	t      *testing.T
	srv    *server.Server
	base   string
	addr   string
	client *http.Client
	cancel context.CancelFunc
	done   chan error

	stopOnce sync.Once
	stopErr  error
}

func testConfig() config.Config {
	cfg := config.Default()
	cfg.ShutdownTimeout = 10 * time.Second
	cfg.WorkerCount = 2
	cfg.WorkerQueueSize = 4
	return cfg
}

func start(t *testing.T, cfg config.Config, opts ...server.Option) *harness {
	t.Helper()
	srv, err := server.New(cfg, logging.New(io.Discard, cfg.LogLevel), opts...)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{
		t:      t,
		srv:    srv,
		addr:   ln.Addr().String(),
		base:   "http://" + ln.Addr().String(),
		client: &http.Client{Timeout: guard, Transport: &http.Transport{}},
		cancel: cancel,
		done:   make(chan error, 1),
	}
	go func() { h.done <- srv.Serve(ctx, ln) }()

	select {
	case <-srv.Health().Ready():
	case <-time.After(guard):
		t.Fatal("server did not become ready")
	}
	// Whatever a test does, never leave the server or its goroutines running.
	t.Cleanup(func() {
		h.client.CloseIdleConnections()
		h.stop()
	})
	return h
}

// stop triggers shutdown and returns Serve's result. It is idempotent so tests
// that assert on shutdown themselves can coexist with the cleanup hook.
func (h *harness) stop() error {
	h.stopOnce.Do(func() {
		h.cancel()
		select {
		case h.stopErr = <-h.done:
		case <-time.After(guard):
			h.stopErr = errors.New("server did not stop in time")
		}
	})
	return h.stopErr
}

func (h *harness) get(path string, headers ...string) (*http.Response, []byte) {
	h.t.Helper()
	return h.do(http.MethodGet, path, "", headers...)
}

func (h *harness) do(method, path, body string, headers ...string) (*http.Response, []byte) {
	h.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.base+path, r)
	if err != nil {
		h.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return resp, b
}

func (h *harness) postJSON(path, body string) (*http.Response, []byte) {
	h.t.Helper()
	return h.do(http.MethodPost, path, body, "Content-Type", "application/json")
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not an error envelope: %s", body)
	}
	return e.Error.Code
}

func sumOfSquares(n int) int64 {
	var s int64
	for i := 0; i < n; i++ {
		s += int64(i) * int64(i)
	}
	return s
}

// gatedWork returns a work function that reports each start on started and
// blocks until gate is closed or the context is cancelled.
func gatedWork(started chan<- struct{}, gate <-chan struct{}) server.Option {
	return server.WithWorkFunc(func(ctx context.Context, i int, _ time.Duration) (int64, error) {
		started <- struct{}{}
		select {
		case <-gate:
			return int64(i) * int64(i), nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	})
}

func wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(guard):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// ---- Basic behaviour ------------------------------------------------------

func TestEndpointsEndToEnd(t *testing.T) {
	h := start(t, testConfig(), server.WithVersion("it-1.0"))

	resp, body := h.get("/health/live")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "ok") {
		t.Errorf("live: %d %s", resp.StatusCode, body)
	}
	resp, body = h.get("/health/ready")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "ready") {
		t.Errorf("ready: %d %s", resp.StatusCode, body)
	}

	resp, body = h.get("/api/v1/info")
	var info struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &info); err != nil || resp.StatusCode != 200 || info.Version != "it-1.0" {
		t.Errorf("info: %d %s", resp.StatusCode, body)
	}

	resp, body = h.get("/api/v1/work?items=10&delay_ms=0")
	var work struct {
		Items    int   `json:"items"`
		Checksum int64 `json:"checksum"`
	}
	if err := json.Unmarshal(body, &work); err != nil || resp.StatusCode != 200 || work.Items != 10 || work.Checksum != sumOfSquares(10) {
		t.Errorf("work: %d %s", resp.StatusCode, body)
	}
}

func TestRequestIDEchoedAndHonored(t *testing.T) {
	h := start(t, testConfig())

	resp, _ := h.get("/api/v1/info")
	if id := resp.Header.Get("X-Request-ID"); len(id) != 32 {
		t.Errorf("generated id = %q", id)
	}
	resp, body := h.get("/api/v1/work?items=abc", "X-Request-ID", "trace-me-123")
	if got := resp.Header.Get("X-Request-ID"); got != "trace-me-123" {
		t.Errorf("header id = %q", got)
	}
	if !strings.Contains(string(body), `"request_id":"trace-me-123"`) {
		t.Errorf("error body must carry the request id: %s", body)
	}
}

func TestAuthAppliesToAPIButNotOperationalEndpoints(t *testing.T) {
	cfg := testConfig()
	cfg.AuthToken = "s3cret"
	h := start(t, cfg)

	for path, want := range map[string]int{"/health/live": 200, "/health/ready": 200, "/metrics": 200} {
		if resp, _ := h.get(path); resp.StatusCode != want {
			t.Errorf("%s = %d, want %d", path, resp.StatusCode, want)
		}
	}
	if resp, body := h.get("/api/v1/info"); resp.StatusCode != 401 || errorCode(t, body) != "unauthorized" {
		t.Errorf("no token: %d %s", resp.StatusCode, body)
	}
	if resp, _ := h.get("/api/v1/info", "Authorization", "Bearer wrong"); resp.StatusCode != 401 {
		t.Errorf("wrong token = %d", resp.StatusCode)
	}
	if resp, _ := h.get("/api/v1/info", "Authorization", "Bearer s3cret"); resp.StatusCode != 200 {
		t.Errorf("right token = %d", resp.StatusCode)
	}
}

func TestRequestHardening(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBodyBytes = 64
	cfg.MaxHeaderBytes = 1024
	h := start(t, cfg)

	if resp, body := h.do(http.MethodPost, "/api/v1/work", `{"items":1}`); resp.StatusCode != 415 || errorCode(t, body) != "unsupported_media_type" {
		t.Errorf("missing content type: %d %s", resp.StatusCode, body)
	}
	big := `{"items":1,"pad":"` + strings.Repeat("x", 500) + `"}`
	if resp, body := h.postJSON("/api/v1/work", big); resp.StatusCode != 413 || errorCode(t, body) != "payload_too_large" {
		t.Errorf("oversized body: %d %s", resp.StatusCode, body)
	}
	// MaxHeaderBytes is approximate: net/http adds ~4 KiB of read-buffer slop,
	// and on a reused keep-alive connection up to ~4 KiB more of the next
	// request may already be buffered before the limit is applied. The header
	// here is far beyond both so the assertion is not sensitive to that.
	if resp, _ := h.get("/api/v1/info", "X-Padding", strings.Repeat("h", 64<<10)); resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("oversized headers = %d, want 431", resp.StatusCode)
	}
	if resp, body := h.do(http.MethodDelete, "/api/v1/work", ""); resp.StatusCode != 405 || errorCode(t, body) != "method_not_allowed" {
		t.Errorf("405: %d %s", resp.StatusCode, body)
	}
}

// TestSlowHeaderClientIsDisconnected demonstrates what ReadHeaderTimeout
// protects against: a client that opens a connection and dribbles headers
// forever would otherwise hold a connection and goroutine indefinitely.
func TestSlowHeaderClientIsDisconnected(t *testing.T) {
	cfg := testConfig()
	cfg.ReadHeaderTimeout = 100 * time.Millisecond
	cfg.ReadTimeout = time.Second
	h := start(t, cfg)

	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// An incomplete header block: no terminating blank line.
	if _, err := conn.Write([]byte("GET /health/live HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}

	// The server must close the connection on its own. Our own read deadline
	// is only a backstop; hitting it would mean the server never gave up.
	if err := conn.SetReadDeadline(time.Now().Add(guard)); err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(conn)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("server never closed a connection stuck in the header phase")
	}
}

func TestPanicInWorkIsContainedAndServerKeepsServing(t *testing.T) {
	h := start(t, testConfig(), server.WithWorkFunc(func(context.Context, int, time.Duration) (int64, error) {
		panic("bug in work function")
	}))
	resp, body := h.get("/api/v1/work?items=3")
	if resp.StatusCode != 500 || errorCode(t, body) != "internal_error" || strings.Contains(string(body), "bug in work") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if resp, _ := h.get("/health/live"); resp.StatusCode != 200 {
		t.Fatalf("server must survive a panic, liveness = %d", resp.StatusCode)
	}
}

// ---- Concurrency ----------------------------------------------------------

func TestConcurrentRequestsAreCorrectAndCounted(t *testing.T) {
	h := start(t, testConfig())

	const clients, items = 64, 20
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, body := h.get(fmt.Sprintf("/api/v1/work?items=%d&delay_ms=0", items))
			var got struct {
				Checksum int64 `json:"checksum"`
			}
			if resp.StatusCode != 200 || json.Unmarshal(body, &got) != nil || got.Checksum != sumOfSquares(items) {
				t.Errorf("bad response: %d %s", resp.StatusCode, body)
			}
		}()
	}
	wg.Wait()

	_, body := h.get("/metrics")
	var m struct {
		RequestsTotal    int64            `json:"requests_total"`
		RequestsInFlight int64            `json:"requests_in_flight"`
		Gauges           map[string]int64 `json:"gauges"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	// The /metrics request itself is in flight but not yet counted.
	if m.RequestsTotal != clients || m.RequestsInFlight != 1 {
		t.Errorf("total/in-flight = %d/%d, want %d/1 (lost or double-counted updates)", m.RequestsTotal, m.RequestsInFlight, clients)
	}
	if got := m.Gauges["work_items_processed_total"]; got != clients*items {
		t.Errorf("items processed = %d, want %d", got, clients*items)
	}
}

// ---- Context: cancellation and deadlines ---------------------------------

func TestClientDisconnectCancelsServerSideWork(t *testing.T) {
	started := make(chan struct{}, 1)
	observed := make(chan error, 1)
	h := start(t, testConfig(), server.WithWorkFunc(func(ctx context.Context, _ int, _ time.Duration) (int64, error) {
		started <- struct{}{}
		<-ctx.Done() // blocks until the request context is cancelled
		observed <- ctx.Err()
		return 0, ctx.Err()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/api/v1/work?items=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	clientErr := make(chan error, 1)
	go func() {
		resp, err := h.client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		clientErr <- err
	}()

	wait(t, started, "work to start")
	cancel() // the client hangs up mid-request

	select {
	case err := <-observed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("server-side work saw %v, want context.Canceled", err)
		}
	case <-time.After(guard):
		t.Fatal("server-side work kept running after the client disconnected")
	}
	if err := <-clientErr; !errors.Is(err, context.Canceled) {
		t.Errorf("client error = %v", err)
	}
}

func TestRequestDeadlineProduces504(t *testing.T) {
	cfg := testConfig()
	cfg.RequestTimeout = 50 * time.Millisecond
	h := start(t, cfg, server.WithWorkFunc(func(ctx context.Context, _ int, _ time.Duration) (int64, error) {
		<-ctx.Done() // never finishes on its own; only the deadline ends it
		return 0, ctx.Err()
	}))

	resp, body := h.get("/api/v1/work?items=2")
	if resp.StatusCode != http.StatusGatewayTimeout || errorCode(t, body) != "timeout" {
		t.Fatalf("got %d %s, want 504 timeout", resp.StatusCode, body)
	}
}

// ---- Async jobs and backpressure -----------------------------------------

func TestAsyncJobLifecycle(t *testing.T) {
	started := make(chan struct{}, 8)
	gate := make(chan struct{})
	h := start(t, testConfig(), gatedWork(started, gate))

	resp, body := h.postJSON("/api/v1/work", `{"items":3,"delay_ms":0}`)
	if resp.StatusCode != 202 {
		t.Fatalf("submit: %d %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/api/v1/work/") {
		t.Fatalf("Location = %q", loc)
	}
	for i := 0; i < 3; i++ {
		wait(t, started, "job items to start")
	}

	type job struct {
		Status string `json:"status"`
		Result *struct {
			Checksum int64 `json:"checksum"`
		} `json:"result"`
	}
	fetch := func() job {
		_, b := h.get(loc)
		var j job
		if err := json.Unmarshal(b, &j); err != nil {
			t.Fatalf("job body: %s", b)
		}
		return j
	}
	if j := fetch(); j.Status != "running" {
		t.Fatalf("while items are blocked, status = %q, want running", j.Status)
	}

	close(gate)

	// The one place this suite polls: observing a state change made by a
	// background goroutine through a black-box HTTP interface leaves no other
	// synchronization point. It is bounded by guard.
	deadline := time.Now().Add(guard)
	for {
		j := fetch()
		if j.Status == "succeeded" {
			if j.Result == nil || j.Result.Checksum != sumOfSquares(3) {
				t.Fatalf("result = %+v", j.Result)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never finished, last status %q", j.Status)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestQueueFullReturns503WithRetryAfter(t *testing.T) {
	cfg := testConfig()
	cfg.WorkerCount = 1
	cfg.WorkerQueueSize = 1
	started := make(chan struct{}, 8)
	gate := make(chan struct{})
	h := start(t, cfg, gatedWork(started, gate))
	defer close(gate)

	if resp, body := h.postJSON("/api/v1/work", `{"items":1}`); resp.StatusCode != 202 {
		t.Fatalf("first job: %d %s", resp.StatusCode, body)
	}
	wait(t, started, "the only worker to pick up job 1") // worker busy, queue empty

	if resp, body := h.postJSON("/api/v1/work", `{"items":1}`); resp.StatusCode != 202 {
		t.Fatalf("second job should fill the queue: %d %s", resp.StatusCode, body)
	}

	resp, body := h.postJSON("/api/v1/work", `{"items":1}`)
	if resp.StatusCode != 503 || errorCode(t, body) != "overloaded" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("third job: %d %s (Retry-After %q), want 503 overloaded", resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
}

func TestUnknownJobIs404(t *testing.T) {
	h := start(t, testConfig())
	if resp, body := h.get("/api/v1/work/does-not-exist"); resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

// ---- Lifecycle: readiness and graceful shutdown --------------------------

func TestReadinessFlipsBeforeListenerCloses(t *testing.T) {
	cfg := testConfig()
	cfg.ShutdownDrainDelay = time.Second // must be > 0 for the drain wait to run
	entered := make(chan struct{})
	release := make(chan struct{})
	h := start(t, cfg, server.WithDrainWait(func(ctx context.Context, _ time.Duration) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))

	h.cancel() // SIGTERM equivalent
	wait(t, entered, "shutdown to reach the drain-delay window")

	// In the window between "not ready" and "listener closed" the instance
	// must refuse new traffic via readiness while still answering probes.
	if resp, body := h.get("/health/ready"); resp.StatusCode != 503 || !strings.Contains(string(body), "draining") {
		t.Errorf("readiness during drain window: %d %s", resp.StatusCode, body)
	}
	if resp, _ := h.get("/health/live"); resp.StatusCode != 200 {
		t.Errorf("liveness must stay 200 while draining, got %d", resp.StatusCode)
	}

	close(release)
	if err := h.stop(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestGracefulShutdownLetsInFlightRequestFinish(t *testing.T) {
	started := make(chan struct{}, 4)
	gate := make(chan struct{})
	h := start(t, testConfig(), gatedWork(started, gate))

	type result struct {
		status int
		body   []byte
	}
	res := make(chan result, 1)
	go func() {
		resp, body := h.get("/api/v1/work?items=1")
		res <- result{resp.StatusCode, body}
	}()
	wait(t, started, "request to be in flight")

	h.cancel() // SIGTERM arrives while the request is mid-flight
	wait(t, h.srv.Health().Draining(), "readiness to flip")

	notShutDownYet(t, h, "a request was still in flight")

	close(gate) // let the in-flight request complete

	r := <-res
	if r.status != 200 || !strings.Contains(string(r.body), `"checksum":0`) {
		t.Fatalf("in-flight request was not completed cleanly: %d %s", r.status, r.body)
	}
	if err := h.stop(); err != nil {
		t.Fatalf("clean drain must return nil, got %v", err)
	}
	if conn, err := net.DialTimeout("tcp", h.addr, time.Second); err == nil {
		conn.Close()
		t.Fatal("listener still accepting connections after shutdown")
	}
}

// notShutDownYet asserts, with a bounded wait, that Serve has not returned.
// The wait is one-sided: on correct code it can never fail spuriously, because
// Serve genuinely must not return; it only makes a broken implementation that
// returns early fail reliably.
func notShutDownYet(t *testing.T, h *harness, why string) {
	t.Helper()
	select {
	case err := <-h.done:
		t.Fatalf("Serve returned early (%v) while %s", err, why)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestShutdownDeadlineCancelsStuckRequestsCooperatively covers phase 2 of
// shutdown: a request that outlives the drain window has its context
// cancelled with a cause, notices, and answers 503 so the client can retry
// elsewhere, instead of having its connection cut.
func TestShutdownDeadlineCancelsStuckRequestsCooperatively(t *testing.T) {
	cfg := testConfig()
	cfg.ShutdownTimeout = 500 * time.Millisecond
	started := make(chan struct{}, 1)
	cause := make(chan error, 1)
	h := start(t, cfg, server.WithWorkFunc(func(ctx context.Context, _ int, _ time.Duration) (int64, error) {
		started <- struct{}{}
		<-ctx.Done() // only the forced cancellation can end this
		cause <- context.Cause(ctx)
		return 0, ctx.Err()
	}))

	type result struct {
		status int
		body   []byte
	}
	res := make(chan result, 1)
	go func() {
		resp, body := h.get("/api/v1/work?items=1")
		res <- result{resp.StatusCode, body}
	}()
	wait(t, started, "request to be in flight")

	h.cancel()
	err := h.stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Serve error = %v, want it to wrap context.DeadlineExceeded (drain did not complete)", err)
	}
	if got := <-cause; !errors.Is(got, domain.ErrUnavailable) {
		t.Errorf("handler saw cancellation cause %v, want domain.ErrUnavailable (server shutdown)", got)
	}
	r := <-res
	if r.status != http.StatusServiceUnavailable || errorCode(t, r.body) != "unavailable" {
		t.Fatalf("client got %d %s; want a clean 503 unavailable, proving the handler responded after cancellation and before the connection was closed", r.status, r.body)
	}
}

// TestShutdownForcesCloseWhenHandlersIgnoreCancellation covers phase 3: a
// handler that never checks its context cannot be stopped cooperatively, so
// its connection is closed and shutdown still meets its deadline.
func TestShutdownForcesCloseWhenHandlersIgnoreCancellation(t *testing.T) {
	cfg := testConfig()
	cfg.ShutdownTimeout = 300 * time.Millisecond
	started := make(chan struct{}, 1)
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) }) // release the leaked handler goroutine at test end
	h := start(t, cfg, server.WithWorkFunc(func(context.Context, int, time.Duration) (int64, error) {
		started <- struct{}{}
		<-stuck // deliberately ignores ctx
		return 0, nil
	}))

	clientErr := make(chan error, 1)
	go func() {
		resp, err := h.client.Get(h.base + "/api/v1/work?items=1")
		if err == nil {
			resp.Body.Close()
			err = fmt.Errorf("unexpected response %d", resp.StatusCode)
		}
		clientErr <- err
	}()
	wait(t, started, "request to be in flight")

	h.cancel()
	begin := time.Now()
	err := h.stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Serve error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("shutdown took %v; it must honor its deadline even with a stuck handler", took)
	}
	// The forced close must actually reach the client promptly. Waiting on the
	// client's own (long) timeout would let a missing Close() pass unnoticed.
	select {
	case err := <-clientErr:
		if strings.HasPrefix(err.Error(), "unexpected response") {
			t.Fatalf("client should see a dropped connection, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("connection was never closed after shutdown gave up on the stuck handler")
	}
}

func TestShutdownDrainsAcceptedAsyncJobs(t *testing.T) {
	started := make(chan struct{}, 8)
	gate := make(chan struct{})
	var completed atomic.Int64
	h := start(t, testConfig(), server.WithWorkFunc(func(ctx context.Context, i int, _ time.Duration) (int64, error) {
		started <- struct{}{}
		select {
		case <-gate:
			completed.Add(1)
			return int64(i) * int64(i), nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}))

	if resp, body := h.postJSON("/api/v1/work", `{"items":3}`); resp.StatusCode != 202 {
		t.Fatalf("submit: %d %s", resp.StatusCode, body)
	}
	for i := 0; i < 3; i++ {
		wait(t, started, "job items to start")
	}

	h.cancel()
	wait(t, h.srv.Health().Draining(), "readiness to flip")
	// No HTTP request is in flight, so only the worker pool drain can be
	// holding shutdown open. If Serve returns now, accepted work was abandoned.
	notShutDownYet(t, h, "accepted jobs were still running")
	close(gate)

	if err := h.stop(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// Serve only returns after the worker pool drained, so every accepted
	// item must have completed by now.
	if got := completed.Load(); got != 3 {
		t.Fatalf("%d/3 items completed; shutdown abandoned accepted work", got)
	}
}

func TestServeFailsFastOnBrokenListener(t *testing.T) {
	cfg := testConfig()
	srv, err := server.New(cfg, logging.New(io.Discard, cfg.LogLevel))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // Serve will fail immediately

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(context.Background(), ln) }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected an error from a closed listener")
		}
	case <-time.After(guard):
		t.Fatal("Serve did not return after the listener failed")
	}
}

func TestListenAndServeReportsBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	cfg := testConfig()
	cfg.Addr = occupied.Addr().String()
	srv, err := server.New(cfg, logging.New(io.Discard, cfg.LogLevel))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.ListenAndServe(context.Background()); err == nil || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("err = %v, want a wrapped listen error", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cfg := testConfig()
	cfg.ReadHeaderTimeout = 0
	if _, err := server.New(cfg, logging.New(io.Discard, cfg.LogLevel)); err == nil {
		t.Fatal("invalid configuration must fail at construction, not at first request")
	}
}
