# Engineering Review

Questions this repository can answer, with concise answers and the code or test
that demonstrates each. The answers are deliberately about tradeoffs, not
slogans. Where "it depends" is the honest answer, the guide says what it depends
on.

## 1. What happens when an HTTP request reaches a Go server?

The kernel completes the TCP handshake and queues the connection. `net/http`'s
accept loop takes it and starts a goroutine for the connection. That goroutine
reads the request line and headers (under `ReadHeaderTimeout`), builds an
`*http.Request` whose context derives from `BaseContext`, calls
`Handler.ServeHTTP`, flushes the response when the handler returns, then either
waits for the next request (keep-alive, under `IdleTimeout`) or closes.
See [http-lifecycle.md](http-lifecycle.md); configured in `server.New`.

## 2. How does `net/http` handle concurrent requests?

One goroutine per connection; on HTTP/1.1 requests on one connection are served
one after another, so concurrency comes from multiple connections. Nothing limits
the number of concurrent handlers. Consequence: everything reachable from a
handler must be safe for concurrent use, and capacity limits have to be added
deliberately (this repo bounds fan-out and the job queue, and documents the
absence of a connection cap).

## 3. What is `http.Handler`?

An interface with one method, `ServeHTTP(ResponseWriter, *Request)`. Routers,
middleware, and endpoints are all handlers, which is why they compose.
[handlers.md](handlers.md).

## 4. Why does `HandlerFunc` exist?

To let a plain function satisfy `Handler` without declaring a type per handler.
It is a function type with a `ServeHTTP` method that calls itself. The
conversion `http.HandlerFunc(fn)` is an adapter, not a call. Used in
`httpapi.Routes`.

## 5. How does middleware work?

A middleware is `func(http.Handler) http.Handler`: it wraps the next handler and
runs code before and after calling it. A stack is nested function application;
`Chain(h, A, B, C)` is `A(B(C(h)))`, so `A` sees the request first and the
response last. [middleware.md](middleware.md);
`TestChainOrderOutermostFirst`.

## 6. How would you prevent a slow client from exhausting server resources?

Layers, since no single control is enough:

- Server timeouts: `ReadHeaderTimeout` against slow headers, `ReadTimeout`
  against stalled bodies, `WriteTimeout` against non-reading clients,
  `IdleTimeout` against idle keep-alives. (`TestSlowHeaderClientIsDisconnected`.)
- Size limits: `MaxHeaderBytes` (approximate), `MaxBytesReader` on bodies.
- A connection cap and rate limiting at the edge. The timeouts bound how long a
  connection is held, not how many; this repo does not implement the cap and says
  so ([failure-modes.md](failure-modes.md), item 1).

## 7. What happens when a client disconnects?

`net/http` notices the closed connection and cancels the request context. Code
that passes `r.Context()` down and waits on `ctx.Done()` stops; code that uses
`context.Background()` or ignores the context keeps running for nobody. Here the
fan-out, each item, and the semaphore acquisition all observe it.
`TestClientDisconnectCancelsServerSideWork`. It is logged as `499`.

## 8. Why should context be propagated?

So cancellation and deadlines reach the work. Without it, a timed-out or
abandoned request still consumes CPU and downstream capacity, and under overload
(when clients retry) that waste compounds. Propagate explicitly as the first
parameter; don't store it in structs (a struct outlives operations, so it would
carry a stale context). [context.md](context.md).

## 9. What is the difference between a timeout and cancellation?

Cancellation is an explicit "stop" (`cancel()`, client disconnect, shutdown).
A timeout is a cancellation triggered automatically by a deadline. Both
close `ctx.Done()`; `ctx.Err()` reports `Canceled` versus `DeadlineExceeded`, and
`context.Cause` can carry a specific reason. We map them differently: deadline
to `504`, client cancel to `499`, shutdown cancel to `503`.
(`TestErrorMapping`, `TestCancellationCauseDistinguishesShutdownFromClientDisconnect`.)

## 10. Why do HTTP servers need graceful shutdown?

Because deployments stop instances constantly. Killing the process resets every
in-flight connection and abandons accepted background work. Graceful shutdown
stops new work, finishes existing work within a budget, and exits.
[graceful-shutdown.md](graceful-shutdown.md).

## 11. What happens to in-flight requests during a deployment?

In this server: readiness flips to 503, the listener closes (after an optional
drain delay), and in-flight requests get 80% of `HTTP_SHUTDOWN_TIMEOUT` to
finish. Anything left has its context cancelled and answers `503` with
`Retry-After`; whatever still remains after the final 20% has its connection
closed. Verified with the real binary and real `SIGTERM`, and by
`TestGracefulShutdownLetsInFlightRequestFinish` and its siblings.

## 12. Why do we need readiness and liveness separately?

They trigger different remedies. Liveness failure means "restart me"; readiness
failure means "stop sending traffic". If they were one endpoint, a dependency
outage would restart healthy processes (or a draining instance would keep
receiving traffic). Draining must be visible as not-ready while the process is
still alive and finishing work. Readiness here is a state machine whose
draining state is terminal. ADR-006.

## 13. How do data races occur?

Two goroutines access the same memory without synchronization and at least one
writes. `count++` is load-add-store, so concurrent increments can be lost, and
under Go's memory model the behavior is undefined, not merely inexact. Prevent
with a mutex, an atomic, confinement to one goroutine, or disjoint access joined
by a `WaitGroup`. [concurrency.md](concurrency.md);
`TestConcurrentObserveIsExact`.

## 14. When would you use a mutex vs atomic?

Atomic for one independent word (a counter, a flag). Mutex when several values
must change consistently together or when the operation is check-then-act (the
job store: a map plus an eviction-order slice). Atomics compose badly: two atomic
fields cannot be updated as one unit. `Metrics` uses atomics and documents its
snapshot as approximate for that reason; `domain.Store` uses a mutex.

## 15. When would you use a channel?

To transfer ownership of a value, signal an event, or bound concurrency. Examples
here: `health.Draining()` (close-once broadcast), the worker queue, the batch
semaphore. Not to protect shared state; a mutex says that more plainly.

## 16. How would you identify a concurrency bug?

`go test -race` and stress runs (`-count`, varied `-cpu`); goroutine dumps
(`SIGQUIT` or `pprof`) for leaks and deadlocks, where many identical stacks or
stuck waits stand out; metrics such as `goroutines` and `requests_in_flight`
trending up; and reasoning about happens-before. Know the detector's limits: it
sees only executed schedules, and this repo found a race test that passed because
it accidentally synchronized. Mutation testing (break the code, confirm the test
fails) is how that was found.

## 17. Why is p99 latency more useful than average latency for some workloads?

The average is a number no request experienced and it hides the tail: 99 requests
at 10 ms plus one at 5 s average about 60 ms. Users and dependent services feel the
slow requests, and in fan-out systems the slowest sub-request dominates, so the
tail is amplified. p50 says what is typical, p99 what is bad; you want both, plus
the error rate. Caveat: p99 needs enough samples to mean anything, and
histogram-based percentiles are bucket-bounded estimates, as in `/metrics`.

## 18. How would you scale this server horizontally?

The synchronous endpoints scale by adding instances behind a load balancer. The
async job flow does not, yet: jobs live in one instance's memory, so a poll
routed elsewhere gets 404. Options are sticky routing by job ID or, properly,
moving jobs to shared storage with a real queue. Also needed: a load balancer that
honors readiness, `HTTP_IDLE_TIMEOUT` above the balancer's idle timeout,
`HTTP_SHUTDOWN_DRAIN_DELAY` for rollouts, and limits at the edge.

## 19. What becomes the bottleneck first?

It depends on the workload, so you measure. For this service, in likely order:
the fan-out and worker limits relative to what they stand in for downstream;
file descriptors and connection count; CPU spent on JSON and logging; log I/O.
The honest method: load test with an open-loop tool, find the knee where latency
rises faster than throughput, and profile there.

## 20. What would you change if traffic increased 100x?

Move state out of process (job store, queue); add per-client rate limiting and a
global concurrency limiter so per-request fan-out times request count is
bounded; sample access logs and adopt a real metrics library with labels;
add tracing; add a connection limit; tune GC and `GOMAXPROCS` to container CPU
limits; possibly shard hot counters; and load test again, because the bottleneck
moves. Not before measuring: many of these are solutions to problems this
service does not yet have.

---

## Questions about this repository's own choices

**Why not use Gin?** Framework types (`gin.Context`) replace `http.Handler` and
`context.Context` with their own, which would hide precisely the mechanics the
repo teaches. For a larger team API with heavy binding and validation, a
framework can be the right call. ADR-001.

**Why not a goroutine per work item?** Goroutines are cheap, not free, and what
they touch is finite. A caller-controlled `items` would let one request starve
others. The semaphore (`WORK_BATCH_CONCURRENCY`) makes overload queue inside the
request. Too small a limit serializes independent waits.

**Why atomic counters for metrics?** Independent single-word counters on the hot
path; a mutex would serialize every request on one lock to bump a number.

**Why not store the request context in a struct?** Struct lifetime exceeds
operation lifetime; you would eventually use a stale, cancelled context. Explicit
parameters make cancellation visible.

**Why graceful shutdown instead of killing the process?** Reset connections for
users, lost in-flight and queued work. The cost is complexity and a bounded delay
on deploys.

**Why limit request body size?** Otherwise the decoder buffers whatever a client
sends. It is a memory and bandwidth guard. The limit must be chosen per endpoint;
a file upload endpoint would need a different one.

**Why timeouts at multiple layers?** They protect against different failures: the
server timeouts protect the process from slow clients, the context deadline
protects it from slow work, and the shutdown deadline protects a deployment from
both. Any one alone leaves a gap.

**Why did shutdown get a cancel phase?** Mutation testing showed the original
"Shutdown, then Close at the deadline" design made the base-context cancellation
redundant (closing connections already cancels request contexts) and gave clients
a dropped connection. The two-phase design gives them a retryable 503.

**What is a weakness of this project?** No durable job storage, so `202` is
not a durable promise; no connection or rate limiting; authentication is a
placeholder; the Dockerfile was not built in the authoring environment; and the
service has never been deployed or load tested by the author. These are stated
in the README rather than glossed over.
