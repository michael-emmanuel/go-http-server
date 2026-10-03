# Operational guide

This guide describes how to configure, run, observe, load test, and reason about
the service. It states what has and has not been verified. Nothing here claims
the service has been deployed to production, to Kubernetes, or to a cloud.

## Configuration

All configuration is environment-driven and validated at startup. An invalid
value prints every problem and exits with status 1; the process never starts
half-configured.

| Variable | Default | Meaning and validation |
|---|---|---|
| `HTTP_ADDR` | `:8080` | Listen address. Must be `host:port`. |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | Max time to read request headers. Must be > 0 and <= `HTTP_READ_TIMEOUT`. |
| `HTTP_READ_TIMEOUT` | `10s` | Max time to read the entire request. Must be > 0. |
| `HTTP_WRITE_TIMEOUT` | `15s` | Max time from end of header read to end of response write. Must be > 0. |
| `HTTP_IDLE_TIMEOUT` | `60s` | Keep-alive idle lifetime. Must be > 0. Keep above any load balancer's idle timeout. |
| `HTTP_MAX_HEADER_BYTES` | `1048576` | Approximate header size cap (see [http-lifecycle.md](http-lifecycle.md)). Must be >= 1024. |
| `HTTP_REQUEST_TIMEOUT` | `10s` | Deadline on the request context for `/api/*`. Must be > 0 and **< `HTTP_WRITE_TIMEOUT`**. |
| `HTTP_MAX_BODY_BYTES` | `65536` | Max JSON body. Must be > 0. |
| `HTTP_SHUTDOWN_TIMEOUT` | `30s` | Whole shutdown budget. Must be > 0. Keep below the orchestrator's grace period. |
| `HTTP_SHUTDOWN_DRAIN_DELAY` | `0s` | Time to keep serving after readiness fails. Must be >= 0 and <= half of `HTTP_SHUTDOWN_TIMEOUT` (the delay is spent from the drain phase). |
| `WORK_MAX_ITEMS` | `1000` | Max `items` per batch or job. |
| `WORK_MAX_ITEM_DELAY` | `1s` | Max per-item simulated delay. |
| `WORK_BATCH_CONCURRENCY` | `8` | Max concurrent items within one batch. |
| `WORKER_COUNT` | `4` | Background worker goroutines. |
| `WORKER_QUEUE_SIZE` | `64` | Queued jobs before `503 overloaded`. |
| `WORK_JOB_RETENTION` | `1000` | Jobs kept in memory; oldest finished job evicted first. |
| `AUTH_TOKEN` | empty | Enables the **placeholder** bearer check on `/api/*` when set. Empty disables auth and logs a warning. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |

### Why the timeouts are set the way they are

The defaults are starting points, not universal answers. They encode these
relationships:

- `read_header (5s) <= read (10s)`: headers are part of the read.
- `request (10s) < write (15s)`: a `504` must still be writable when the
  deadline fires.
- `shutdown (30s)`: matches a common orchestrator default grace period; lower it
  if yours differs.

Raise `ReadTimeout` and `WriteTimeout` if you accept large uploads or stream
large responses; per-endpoint control needs `http.ResponseController`
(`SetReadDeadline`, `SetWriteDeadline`), which the recorder's `Unwrap()` keeps
reachable. Multiple layers of timeout are deliberate: server timeouts protect
the process from slow *clients*; the context deadline protects it from slow
*work*; the shutdown deadline protects the deployment from both.

## Running

```
go run ./cmd/server                          # defaults, port 8080
HTTP_ADDR=127.0.0.1:9090 LOG_LEVEL=debug go run ./cmd/server
AUTH_TOKEN=dev-secret go run ./cmd/server    # enable the placeholder auth
```

`server healthcheck` performs `GET /health/live` against `HTTP_ADDR` and exits
0 or 1. It exists because the container image has no shell or curl.

Signals: `SIGINT` and `SIGTERM` start graceful shutdown; a second signal
terminates immediately. When testing from a script, start the process with
`SIGTERM`-based control: shells launch background jobs with `SIGINT` ignored,
and Go preserves that.

## Observability

### Logs

JSON, one object per line, to stdout. Each request produces one
`request completed` line:

```json
{"time":"...","level":"INFO","msg":"request completed","method":"GET","path":"/api/v1/work","status":200,"bytes":71,"duration_ms":41.6,"remote_addr":"10.0.0.7:51322","request_id":"2675ff58..."}
```

- Level: `ERROR` for 5xx, `WARN` for 4xx, `INFO` otherwise; successful
  `/health/*` requests log at `DEBUG`, failing ones stay at `ERROR`/`WARN`.
- The query string and headers are never logged; they routinely carry secrets.
- `remote_addr` is the TCP peer. Behind a proxy it is the proxy's address;
  this service does not parse `X-Forwarded-For` because trusting it without
  knowing the proxy topology would let clients spoof their address.
- The startup line logs the configuration through `Config.LogValue`, which
  reports `auth_enabled` but never the token.
- Use the `*Context` logging methods (`log.InfoContext(ctx, ...)`) so the
  request ID is attached. `log.Info(...)` without a context has none.

### Metrics

`GET /metrics` returns a JSON snapshot:

```json
{
  "requests_total": 64, "requests_in_flight": 1, "active_connections": 3,
  "responses_by_class": {"1xx":0,"2xx":64,"3xx":0,"4xx":0,"5xx":0},
  "client_errors_total": 0, "server_errors_total": 0,
  "latency": {"count": 64, "sum_seconds": 0.21,
              "buckets": [{"le_seconds":0.001,"count":10}, ...],
              "p50_seconds": 0.005, "p95_seconds": 0.05, "p99_seconds": 0.1},
  "gauges": {"worker_queue_depth": 0, "jobs_succeeded_total": 0, ...}
}
```

- The quantiles are **upper-bound estimates**: the upper edge of the histogram
  bucket containing the quantile. A p99 of `0.1` means "at or below 100 ms and
  above the previous bucket bound", not "exactly 100 ms". They saturate at the
  largest bucket (10 s).
- Values are read atomically one by one, not as one consistent cut. Fine for
  monitoring, unsuitable for accounting.
- `requests_in_flight` includes the `/metrics` request that is reading it.
- The endpoint is JSON, not Prometheus exposition format; it needs an adapter to
  be scraped by Prometheus. That is a deliberate scope limit.

### Signals worth alerting on

`server_errors_total` rate (5xx); `responses_by_class.4xx` with a rising 499/503
split in logs; `worker_queue_depth` approaching `WORKER_QUEUE_SIZE`;
`requests_in_flight` growing without bound (something is not finishing);
`p99_seconds` rising while `p50_seconds` is flat (tail latency).

## Security

### What is implemented

- Server timeouts and a header size cap (slow-client and oversized-header
  defense).
- Bounded request bodies (`MaxBytesReader`), `DisallowUnknownFields`, rejection
  of trailing data, `Content-Type` enforcement.
- Input validation with explicit upper bounds (`items`, `delay_ms`).
- Panic recovery; internal errors are logged, never returned. Error messages
  are generic and carry a request ID.
- Request IDs from clients are validated before being echoed.
- `X-Content-Type-Options: nosniff` and `Cache-Control: no-store` on API
  responses.
- Constant-time token comparison.
- Non-root container user, static binary, minimal base image.

### Authentication is a placeholder

`AUTH_TOKEN` enables a single static shared secret compared in constant time.
It has no user identity, no expiry, no rotation, no revocation, no rate limiting
on failures, and travels in a header over whatever transport you provide. Do not
treat it as production authentication. Use mTLS, OIDC/JWT validation, or an
authenticating gateway. Without `AUTH_TOKEN` the API is open.

### Assumptions

- TLS terminates in front of the service (load balancer or ingress). The server
  speaks plain HTTP.
- `/health/*` and `/metrics` are unauthenticated and must be reachable only by
  the orchestrator and monitoring, enforced by network policy. `/metrics`
  reveals traffic volume and internal state.
- A reverse proxy or load balancer enforces global connection and rate limits.

### Remaining attack surface

- **No rate limiting.** One client can consume all worker and batch capacity.
  `503 overloaded` protects the process, not fairness among clients.
- **No connection limit.** `net/http` accepts connections until descriptors run
  out. Set `ulimit -n` and limit at the proxy.
- **Cheap amplification.** `items=1000&delay_ms=1000` with concurrency 8 holds a
  request for up to about two minutes of nominal work; the 10 s request deadline
  and per-item cap bound it, but many such requests still occupy goroutines.
- **Unauthenticated job reads when auth is disabled.** Job IDs are 128-bit
  random, so guessing is impractical, but IDs are not secrets to rely on.
- **In-memory state** is lost on restart and grows to `WORK_JOB_RETENTION`.
- **No TLS, CORS, CSRF, or security headers beyond `nosniff`.** Not relevant to a
  JSON API behind a gateway, but they would be for a browser-facing one.
- **HTTP/2** is not enabled (no TLS termination here). If terminated by the
  server later, HTTP/2 brings its own resource-exhaustion classes that the
  standard library mitigates but which should be re-reviewed.

## Benchmarks

```
make bench        # go test -run '^$' -bench . -benchmem ./...
```

Benchmarks in this repo: `BenchmarkRequestHandling` (routing, handler, JSON, with
a stub service, no network), `BenchmarkMiddlewareOverhead` and its parallel
variant (bare handler versus full production stack), `BenchmarkRunBatch` and
`BenchmarkRunBatchConcurrentRequests` (fan-out machinery with zero-delay work),
and `BenchmarkObserveParallel` (atomic counter contention).

What they can tell you: relative cost between two implementations on the same
machine; allocations per operation (`-benchmem`); whether a change made a hot
path slower. What they cannot tell you: real-world latency or throughput
(they skip the network, TLS, the kernel, and real I/O), behavior under a
realistic mix, or anything portable across machines. Micro-benchmarks also run
on shared CI runners with noisy neighbours, which is why CI runs them but never
fails on their results. No benchmark figures are published in this repository,
because a number without its hardware and load model is not a claim anyone can
check.

## Load testing (optional; not run in CI, not run for this repository)

Any of these can drive the service. None is a dependency of the build.

```
# hey: fixed duration, 50 concurrent connections
hey -z 30s -c 50 "http://localhost:8080/api/v1/work?items=10&delay_ms=20"

# wrk: 4 threads, 64 connections, with latency distribution
wrk -t4 -c64 -d30s --latency "http://localhost:8080/api/v1/work?items=10&delay_ms=20"

# vegeta: fixed *arrival rate* (open-loop), which is what real traffic is
echo "GET http://localhost:8080/api/v1/work?items=10&delay_ms=20" | \
  vegeta attack -duration=30s -rate=200 | vegeta report
```

Use your own results; this repository does not include any. Read them like this:

- **Throughput**: completed requests per second. Only meaningful alongside the
  latency at that throughput and the error rate.
- **Latency percentiles**: p50 is the typical request, p95 and p99 the tail.
  Averages hide the tail: 99 requests at 10 ms and one at 5 s average about
  60 ms, a number that describes no request anyone experienced. Users notice the
  slow ones, and a page or API call that fans out to many backends is bound by
  the slowest of them, so tail latency is amplified.
- **Error rate**: distinguish 5xx (server faults), 503 (deliberate shedding), and
  client-side timeouts. A run with 0 errors where latency simply grows is a
  queue building somewhere.
- **Closed-loop versus open-loop.** `hey` and `wrk` (fixed connections) slow down
  when the server slows down, which hides overload (coordinated omission).
  `vegeta` at a fixed rate keeps arriving regardless and shows what really
  happens past saturation.

Ways to see the repository's own mechanisms under load: raise `-c` until you see
`503 overloaded` from `POST /api/v1/work` (backpressure); watch `/metrics`
`worker_queue_depth`; send `SIGTERM` mid-run and confirm in-flight requests
finish and new connections are refused.

## Docker

The `Dockerfile` is a multi-stage build:

1. **Build stage** (`golang:1.22-alpine`): copies `go.mod` first for layer
   caching, then the source, and builds with `CGO_ENABLED=0 -trimpath -ldflags
   "-s -w -X main.version=..."`, producing a static binary.
2. **Runtime stage** (`gcr.io/distroless/static-debian12:nonroot`): contains the
   binary only, plus CA certificates and tzdata; no shell, package manager, or
   libc. Runs as `nonroot`. `EXPOSE 8080`.
3. **Health check**: `HEALTHCHECK CMD ["/server", "healthcheck"]`, because the
   image has no curl.

Caveat: the Dockerfile and the `docker` CI job were written to this design but
**not built in the environment where this repository was authored** (no Docker
daemon was available). The binary they package, the `healthcheck` subcommand,
and the signal behavior were verified natively.

Note that a container's PID 1 receives signals directly, which is what we want:
the binary is the entrypoint and handles `SIGTERM` itself. Wrapping it in
`sh -c` would swallow the signal.

## Scaling and bottlenecks

**Horizontally.** The service is stateless except for the in-memory job store and
queue. Sync endpoints (`GET /api/v1/work`, `/info`) scale by adding instances
behind a load balancer. **`POST /api/v1/work` followed by
`GET /api/v1/work/{id}` does not**: a job lives in one instance's memory, so a
poll routed to another instance returns 404. Either pin by job ID (consistent
hashing) or move jobs to shared storage (a database plus a real queue). This is
the largest gap between this project and a horizontally scalable service, and it
is deliberate: the task was to demonstrate HTTP and concurrency mechanics, not
to simulate a database.

**What limits a single instance first**, in the likely order:
1. The per-request batch fan-out and worker pool sizes (`WORK_BATCH_CONCURRENCY`,
   `WORKER_COUNT`) relative to the downstream capacity they stand in for.
2. File descriptors and connection count.
3. CPU, from JSON encoding and logging, not from the handlers' logic.
4. Log volume; per-request JSON logging is I/O.

**If traffic grew 100x**, in rough order: move state out of process (shared job
store, real queue); add rate limiting per client at the edge; add a global
concurrency limiter so the product of concurrent requests and per-request
fan-out is bounded; sample access logs and switch metrics to a real library with
labels; add tracing for cross-service latency; tune GC and `GOMAXPROCS` to the
container's CPU limit; and load-test again, because the bottleneck moves.
