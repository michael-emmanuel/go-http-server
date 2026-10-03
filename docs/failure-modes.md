# Failure mode analysis

For each failure: what it is, how you would detect it, what the impact is, what
the service does about it, and what risk remains. "Verified by" points to the
test that exercises the mitigation where one exists. Where there is no test the
entry says so.

---

## 1. Slow client

**Failure.** A client connects and sends headers or a body very slowly, or reads
the response very slowly, holding a goroutine and a descriptor.

**Detection.** `active_connections` rising without a matching rise in
`requests_total`; connections long past typical request time; goroutine count
growth (`/api/v1/info` reports `goroutines`).

**Impact.** Each stalled connection costs a goroutine, buffers, and a descriptor.
Enough of them exhaust descriptors and starve real clients.

**Mitigation.** `ReadHeaderTimeout` (5 s), `ReadTimeout` (10 s), `WriteTimeout`
(15 s), `IdleTimeout` (60 s), `MaxHeaderBytes`. Verified by
`TestSlowHeaderClientIsDisconnected`.

**Remaining risk.** The timeouts bound how long each connection can be held, not
how many can be held at once. There is no connection cap or per-client limit.
Many slow clients each holding a connection for up to the timeout can still
exhaust descriptors. Needs a connection limit at the proxy and a raised
`ulimit -n`.

---

## 2. Slow downstream dependency

**Failure.** Work that the service depends on (in a real system: a database or an
API) becomes slow. Here it is simulated by `delay_ms`.

**Detection.** Rising p95/p99 with normal p50; `requests_in_flight` climbing;
`504` count increasing.

**Impact.** Requests pile up, each holding a goroutine. Without deadlines this
becomes an outage.

**Mitigation.** `RequestTimeout` puts a deadline on the request context and work
honors it. The result is `504` with the standard envelope. `MaxWorkItemDelay`
bounds per-item wait. Verified by `TestRequestDeadlineProduces504`.

**Remaining risk.** No circuit breaker, no retry budget, no bulkheading. A slow
dependency still consumes capacity until each request's deadline expires, so a
low deadline matters. Work that ignores its context cannot be stopped.

---

## 3. Handler panic

**Failure.** A bug panics inside a handler or a layer beneath `Recovery`.

**Detection.** `panic recovered` log line with stack and request ID; a 500 in the
access log and `server_errors_total`.

**Impact.** One failed request. Without recovery, `net/http` would drop the
connection with no response and no metrics.

**Mitigation.** `Recovery` returns a JSON 500 and logs the stack. It sits inside
the logging and metrics layers so they record the 500. Panics in goroutines the
service starts (`runItem`, `runTask`) are recovered and converted to errors.
Verified by `TestRecoveryReturnsJSON500AndProcessSurvives`,
`TestProductionChainRecordsPanicAs500`, `TestPanicInWorkIsContainedAndServerKeepsServing`.
If a panic occurs after the response has started, the connection is aborted
(`TestRecoveryAbortsConnectionWhenResponseAlreadyStarted`).

**Remaining risk.** A panic in a goroutine spawned by future code that does not
add its own `recover` will crash the process. A panic in `AccessLog` or
`Metrics` themselves is not caught by `Recovery`. A fatal runtime error
(`concurrent map writes`, out of memory) cannot be recovered at all.

---

## 4. Context cancellation (client disconnects)

**Failure.** The client goes away before the response.

**Detection.** Status `499` in the access log and metrics, and a `request failed`
line with code `client_closed_request` at info level.

**Impact.** If work ignored cancellation, it would continue and waste capacity.
This is amplified under overload, when slow responses make clients retry.

**Mitigation.** The request context reaches the fan-out and every item; items
stop at their next wait. Verified by `TestClientDisconnectCancelsServerSideWork`.

**Remaining risk.** Only cooperative code stops. A CPU-bound loop without a
`ctx.Done()` check runs to completion. Server-side side effects already
performed are not rolled back.

---

## 5. Server overload

**Failure.** Requests arrive faster than they complete.

**Detection.** `worker_queue_depth` near `WORKER_QUEUE_SIZE`; `503 overloaded`
responses; `requests_in_flight` and latency rising.

**Impact.** Unbounded queueing would turn overload into memory growth and
ever-increasing latency for everyone.

**Mitigation.** The async path has a bounded queue and rejects with
`503 overloaded` plus `Retry-After: 1`. The job store refuses new jobs rather
than evicting active ones. Per-request fan-out is capped. Verified by
`TestQueueFullReturns503WithRetryAfter`,
`TestStoreRefusesRatherThanEvictActiveJobs`,
`TestConcurrencyLimitBlocksFurtherLaunches`.

**Remaining risk.** The synchronous path (`GET /api/v1/work`) has no global
limit; in-flight work is bounded by request count times per-request concurrency.
There is no per-client fairness or rate limiting. Overload also does not affect
readiness by design (see ADR-006), so instances under pressure keep receiving
traffic; shedding is per request.

---

## 6. Goroutine leak

**Failure.** Goroutines that never exit accumulate.

**Detection.** `goroutines` in `/api/v1/info` growing monotonically under steady
load; a `SIGQUIT` goroutine dump or `pprof` showing many identical stacks.

**Impact.** Memory growth, then descriptor or scheduler pressure.

**Mitigation.** By construction: `RunBatch` joins its goroutines before
returning; `Pool.Shutdown` waits for workers even after cancelling; `Serve`
receives from its goroutine's channel; semaphore acquisition selects on
`ctx.Done()`. Tests fail if the join is removed (mutation-checked).

**Remaining risk.** There is no automated goroutine-leak assertion at the end of
each test (that would require a dependency such as `goleak` or polling). The
protection is the join discipline plus the mutation-checked tests, not a
universal detector. A `WorkFunc` that blocks forever ignoring its context will
leak its goroutine (`TestShutdownForcesCloseWhenHandlersIgnoreCancellation`
deliberately leaks one and releases it in cleanup).

---

## 7. Deadlock

**Failure.** Goroutines block on each other forever.

**Detection.** Requests that never complete; `requests_in_flight` stuck above
zero; a goroutine dump showing goroutines waiting on channels or locks; the
runtime's "all goroutines are asleep" abort only fires when *every* goroutine is
blocked, which a server rarely is.

**Impact.** Stuck requests, then shutdown that hits its deadline.

**Mitigation.** Lock use is minimal and never nested: `Store` and `Pool` each
hold one mutex around short critical sections that call no foreign code.
`Metrics.Snapshot` copies gauge functions out before calling them so no callback
runs under its lock. Channel operations that can block select on a context.

**Remaining risk.** Nothing enforces lock ordering in future code; the property
is maintained by review. There is no watchdog. A test that deadlocks fails by
timeout (`go test -timeout`); the mutation checks showed that removing a
cancellation path surfaces as a test timeout rather than a silent pass.

---

## 8. Race condition

**Failure.** Unsynchronized concurrent access to shared state.

**Detection.** `go test -race ./...` in CI; in production, only by symptom
(wrong counts, `fatal error: concurrent map writes`).

**Impact.** Undefined behavior: lost updates, corrupted state, or a process
crash.

**Mitigation.** Shared state uses atomics or a mutex as tabulated in
[architecture.md](architecture.md). `-race` runs in CI and passed across repeated
runs and varied `GOMAXPROCS`.

**Remaining risk.** The race detector only sees races in the executed schedule.
We found and fixed a case where a race test passed because it accidentally
synchronized (see [concurrency.md](concurrency.md)). The development sandbox had
one CPU, so multi-core interleavings were exercised only by CI. The detector
also cannot find logic races (check-then-act across atomics).

---

## 9. Excessive request body

**Failure.** A client sends a huge body to consume memory or bandwidth.

**Detection.** `413` rate; `payload_too_large` code.

**Impact.** Without a limit the decoder would buffer whatever arrives.

**Mitigation.** `MaxBytesReader` (default 64 KiB) rejects with `413`. After the
response, `net/http` discards at most 256 KiB of the unread body before reusing
the connection, and closes it otherwise. `ReadTimeout` bounds
how long a slow upload can take. Verified by `TestSubmitWorkRejectsBadBodies`
(body too large) and `TestRequestHardening`.

**Remaining risk.** `MaxBodyBytes` applies to the JSON decode path. A future
handler that reads the body another way must also apply it. Bandwidth is still
consumed up to the limit.

---

## 10. Malformed JSON

**Failure.** Truncated, non-JSON, wrong-typed, unknown-field, or multi-value
bodies.

**Detection.** `400` with `malformed_json` or `invalid_request`.

**Impact.** Silent misinterpretation if accepted (ignored typos, trailing data).

**Mitigation.** `DisallowUnknownFields`, single-value enforcement, empty-body
check, type-error mapping, no echo of the offending content. Verified by nine
cases in `TestSubmitWorkRejectsBadBodies`.

**Remaining risk.** The unknown-field check matches the error message prefix
because `encoding/json` exposes no typed error for it; a future change to that
message would degrade the response to a generic 500, which the test would catch.
Deeply nested JSON is bounded only by the body limit.

---

## 11. Shutdown during an active request

**Failure.** `SIGTERM` arrives while requests are in flight.

**Detection.** `shutdown initiated` then `shutdown complete` or `shutdown finished
with errors` in logs; exit code.

**Impact.** Killing the process would reset every in-flight connection.

**Mitigation.** Readiness flips, listeners close, in-flight requests finish for
80% of the budget; then contexts are cancelled with cause `ErrUnavailable` and
handlers answer `503`; then connections are closed. Verified by
`TestGracefulShutdownLetsInFlightRequestFinish`,
`TestShutdownDeadlineCancelsStuckRequestsCooperatively`,
`TestShutdownForcesCloseWhenHandlersIgnoreCancellation`, and by manual runs of the
built binary with real signals.

**Remaining risk.** Requests that do not honor their context are cut off at the
deadline. If the orchestrator's grace period is shorter than
`HTTP_SHUTDOWN_TIMEOUT`, `SIGKILL` arrives first. Non-idempotent requests retried
after a `503` may repeat side effects; this service's operations are safe to
retry, others might not be.

---

## 12. Shutdown during long-running background work

**Failure.** `SIGTERM` while jobs are queued or running.

**Detection.** Jobs recorded as `canceled` (`jobs_canceled_total`); a shutdown
error naming `worker drain`.

**Impact.** Accepted work may be lost.

**Mitigation.** After HTTP drains, the pool stops accepting, finishes queued
jobs, and at the deadline cancels its context; tasks observe cancellation.
Verified by `TestShutdownDrainsAcceptedAsyncJobs`,
`TestShutdownDeadlineCancelsRunningTasks`, `TestJobCancelledByShutdownContext`.

**Remaining risk.** Jobs are in memory. Anything still queued or cancelled at exit
is gone, and a crash loses everything. The `202 Accepted` is a promise the process
cannot keep across a restart. A durable queue is required if that matters.

---

## 13. Readiness failure

**Failure.** An instance should stop receiving traffic (starting, draining) but
does not, or the reverse.

**Detection.** Errors during rollouts; `503` from `/health/ready` while traffic
still arrives.

**Impact.** Requests routed to an instance that is not ready.

**Mitigation.** A three-state machine; `draining` is terminal; readiness flips
before the listener closes; `HTTP_SHUTDOWN_DRAIN_DELAY` covers routing
convergence. Verified by `TestReadinessTransitions`, `TestDrainingIsTerminal`,
`TestReadinessFlipsBeforeListenerCloses`.

**Remaining risk.** With the default drain delay of `0` the listener closes
immediately, so requests arriving in the orchestrator's propagation window are
refused. Readiness does not reflect dependency health because there are no
dependencies.

---

## 14. Process crash

**Failure.** The process exits unexpectedly (fatal runtime error, OOM kill,
node failure).

**Detection.** Liveness probe failures; container restart count; missing logs
after a point.

**Impact.** All in-flight requests fail, in-memory jobs are lost.

**Mitigation.** Liveness lets an orchestrator restart it; instances are
stateless apart from jobs; the `healthcheck` subcommand gives the container
runtime a probe.

**Remaining risk.** No persistence, no supervisor beyond the runtime, no crash
reporting. Unrecoverable runtime errors (concurrent map writes, stack exhaustion,
OOM) bypass all `recover` logic by design.
