# Architecture

This document describes how the service is put together and, more importantly,
why the boundaries are where they are. Companion documents go deeper on
individual topics: [http-lifecycle.md](http-lifecycle.md),
[middleware.md](middleware.md), [concurrency.md](concurrency.md),
[context.md](context.md), [graceful-shutdown.md](graceful-shutdown.md),
[failure-modes.md](failure-modes.md), [decisions.md](decisions.md).

## Component diagram

```
                 ┌───────────────────────┐
                 │        Client         │
                 └───────────┬───────────┘
                             │  TCP connection(s), HTTP/1.1 requests
                             ▼
                 ┌───────────────────────┐
                 │   net/http.Server     │  timeouts, header limit, ConnState,
                 │   (internal/server)   │  BaseContext, one goroutine per conn
                 └───────────┬───────────┘
                             │  one handler call per request
                             ▼
                 ┌───────────────────────┐
                 │  Root middleware      │  RequestID → AccessLog → Metrics
                 │  (internal/server)    │  → Recovery
                 └───────────┬───────────┘
                             ▼
                 ┌───────────────────────┐
                 │  Router               │  Go 1.22 ServeMux patterns,
                 │  (internal/httpapi)   │  JSON 404/405
                 └─────┬───────────┬─────┘
        /health, /metrics│           │/api/v1/*
                         │           ▼
                         │   ┌───────────────────────┐
                         │   │  API middleware       │  Auth (placeholder) →
                         │   │  (internal/server)    │  RequireJSON →
                         │   └───────────┬───────────┘  RequestTimeout
                         │               ▼
                         │   ┌───────────────────────┐
                         │   │  Handlers             │  parse, validate, call
                         │   │  (internal/httpapi)   │  service, map errors
                         │   └───────────┬───────────┘
                         ▼               ▼
                 ┌──────────────┐  ┌───────────────────────┐   ┌────────────────┐
                 │ health,      │  │  Application logic    │──▶│  Worker pool   │
                 │ metrics      │  │  (internal/domain)    │   │ (internal/     │
                 └──────────────┘  │  batch fan-out, jobs  │   │  worker)       │
                                   └───────────────────────┘   └────────────────┘
```

## Layers and what belongs in each

The same three concerns are kept apart throughout the code.

| Concern | Examples | Lives in |
|---|---|---|
| Transport | methods, paths, headers, JSON, status codes, `Retry-After` | `internal/httpapi`, `internal/server/middleware.go` |
| Application | what a batch or job is, limits, execution rules, job lifecycle | `internal/domain` |
| Infrastructure | sockets, timeouts, goroutine pools, signals, logging setup | `internal/server`, `internal/worker`, `internal/logging`, `internal/metrics`, `cmd/server` |

A useful test: could this code run unchanged if the API were gRPC or a CLI?
If yes it is application logic and should not import `net/http`. `internal/domain`
does not.

## Package responsibilities

| Package | Responsibility |
|---|---|
| `cmd/server` | Translate process environment (env vars, signals, exit code) into calls. Contains no logic worth testing beyond the `healthcheck` subcommand. |
| `internal/config` | Load env vars, apply defaults, validate every field and cross-field relationship, fail fast. |
| `internal/server` | Composition root, `http.Server` configuration, middleware, graceful shutdown. |
| `internal/httpapi` | Handlers, routing, JSON encoding and decoding, the error envelope, error classification. |
| `internal/domain` | Batch execution (bounded fan-out), asynchronous jobs, in-memory job store, domain errors. |
| `internal/worker` | Bounded worker pool with backpressure and deadline-aware shutdown. |
| `internal/health` | Readiness state machine and probe handlers. |
| `internal/metrics` | Atomic counters, latency histogram, gauges, connection tracking. |
| `internal/requestid` | ID generation, validation, context carrying. |
| `internal/logging` | JSON logger that attaches the request ID from the context. |

## Dependency rules

Imports point inward and downward. There are no cycles and no package reaches
back up.

```
cmd/server ──▶ server ──▶ httpapi ──▶ domain ──▶ worker
     │           │           │  │
     │           │           │  └────▶ health
     ▼           ▼           ▼
  config      metrics     requestid ◀── logging
```

Two rules are worth stating because they are easy to violate:

1. `httpapi` must not import `server`. Middleware lives in `server`, so
   `httpapi.Routes` accepts the API policy as a `func(http.Handler) http.Handler`
   and `httpapi.WriteError` is exported so middleware can emit the same error
   envelope. Without this the two packages would import each other.
2. Interfaces are declared by the consumer. `httpapi.WorkService` is defined in
   `httpapi` (satisfied by `domain.Service`); `domain.Executor` is defined in
   `domain` (satisfied by `worker.Pool`). Each has exactly the methods its
   consumer needs, and each exists because a test substitutes a fake.

## Request lifecycle

1. The kernel accepts a TCP connection; `net/http` starts a goroutine for it and
   `ConnState` increments the active-connection gauge.
2. `ReadHeaderTimeout` starts counting. The request line and headers are read
   (bounded by `MaxHeaderBytes`).
3. The connection goroutine calls the root handler with a `Request` whose context
   derives from the server's `BaseContext`.
4. `RequestID` reads or generates the ID, sets the response header, stores it in
   the context.
5. `AccessLog` and `Metrics` note the start time and wrap the writer to record
   status and size. `Recovery` installs its `defer`.
6. The router matches the pattern. `/health/*` and `/metrics` go straight to
   their handlers. `/api/*` first passes through `Auth`, `RequireJSON`, and
   `RequestTimeout`, which attaches a deadline to the context.
7. The handler parses input, calls the service with `r.Context()`, and writes one
   JSON response through `respond` or `fail`.
8. On the way out, `Metrics` records status class and duration, `AccessLog`
   writes one line, and the connection returns to keep-alive idle (or closes).

See [http-lifecycle.md](http-lifecycle.md) for what happens before step 1 and
below the handler.

## Concurrency model

- `net/http` runs one goroutine per connection, and each request on that
  connection is handled serially by that goroutine. Any code reachable from a
  handler must be safe for concurrent use. `Handler`, `Service`, `Metrics`,
  `Checker`, and `Store` are.
- Synchronous work (`GET /api/v1/work`) fans out inside the request under a
  semaphore (`WORK_BATCH_CONCURRENCY`), tied to the request context.
- Asynchronous work (`POST /api/v1/work`) is queued to a fixed pool of
  `WORKER_COUNT` goroutines through a queue of `WORKER_QUEUE_SIZE`. A full queue
  is rejected with `503` and `Retry-After`: backpressure is explicit.
- Shared state and its protection:

| State | Protection | Why |
|---|---|---|
| request/status/in-flight counters | `sync/atomic` | independent single-word counters on the hot path |
| latency histogram buckets | one atomic per bucket | same |
| job store | `sync.RWMutex` | read-modify-write on a struct plus an eviction-order slice needs one critical section |
| worker pool `closed` + channel close | `sync.RWMutex` | a send must never race with `close(ch)` |
| readiness state | atomic + `sync.Once` channel close | one word, plus close-exactly-once |
| per-batch result slice | none | each goroutine writes only its own index; read after `WaitGroup.Wait` |

Details and counter-examples are in [concurrency.md](concurrency.md).

## Shutdown sequence

```
 Running
   │  SIGTERM / SIGINT
   ▼
 readiness = draining              (health.MarkDraining)
   ▼
 optional drain delay              (HTTP_SHUTDOWN_DRAIN_DELAY)
   ▼
 http.Server.Shutdown              close listeners, wait for in-flight
   │  for 80% of HTTP_SHUTDOWN_TIMEOUT
   ▼  requests remain?
 cancel base context (cause: ErrUnavailable) ── handlers answer 503
   │  for the remaining 20%
   ▼  still remain?
 http.Server.Close                 sever connections
   ▼
 worker pool drain                 finish queued jobs; cancel at deadline
   ▼
 join errors, return, process exits (0 if clean, 1 otherwise)
```

See [graceful-shutdown.md](graceful-shutdown.md).

## Observability

- **Logs**: one structured line per request (`request completed`) with method,
  path (never the query string), status, bytes, `duration_ms`, remote address,
  and `request_id`. Probes log at debug when successful.
- **Metrics**: `GET /metrics` returns a JSON snapshot: request counts by status
  class, in-flight requests, active connections, a latency histogram with
  bucket-derived p50/p95/p99 upper bounds, and gauges for the worker queue and
  job counters.
- **Correlation**: the `X-Request-ID` response header, the `request_id` in error
  bodies, and the `request_id` field in every log line written with a context are
  the same value.

This is intentionally small. It shows the primitives; it is not a replacement
for a metrics library with labels, exposition standards, and cardinality
control. See [operational-guide.md](operational-guide.md).

## Failure modes

Summarized in [failure-modes.md](failure-modes.md) with detection, impact,
mitigation, and remaining risk for each.
