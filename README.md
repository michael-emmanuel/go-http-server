# Go Production HTTP Server

A production-oriented HTTP server implemented with Go's standard library, designed to demonstrate HTTP lifecycle management, handler composition, middleware, concurrency, context propagation, observability, and graceful shutdown.

The service itself is small on purpose (simulated batch work, in-memory jobs). The point is the machinery around it: how requests are accepted, bounded, cancelled, observed, and drained, and how each of those behaviors is tested.

## Verification status

What has and has not been checked, stated plainly.

| Claim                                                                          | Status                                                                                                                                                                               |
| ------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Builds, `go vet` clean, `gofmt` clean                                          | Verified with Go 1.22.2                                                                                                                                                              |
| `go test -race ./...` passes                                                   | Verified; repeated 35 times across `GOMAXPROCS` 1 and 8 with no failures                                                                                                             |
| 138 test functions, 6 benchmarks, about 95% statement coverage across packages | Verified (`make cover`; the figure needs `-coverpkg`, see Testing)                                                                                                                   |
| Tests can fail (mutation-checked)                                              | Verified: 33 hand-written mutants, all detected (`make mutation`)                                                                                                                    |
| Graceful shutdown with real `SIGTERM`                                          | Verified against the built binary: drain, cooperative cancel, force close, second-signal exit                                                                                        |
| Multi-core race behavior                                                       | **Not exercised locally.** The authoring sandbox had one CPU. CI provides the multi-core run.                                                                                        |
| Docker image builds and runs                                                   | **Not verified.** No Docker daemon was available. The Dockerfile follows the design described below; the binary it packages and its `healthcheck` subcommand were verified natively. |
| Load testing (`hey`, `wrk`, `vegeta`)                                          | **Not run.** Commands are documented; no results are published.                                                                                                                      |
| Deployed to any environment, including Kubernetes or a cloud                   | **No.** No manifests are included and nothing here claims a deployment.                                                                                                              |
| OpenAPI file                                                                   | Parsed and cross-checked against the source (all `$ref`s resolve, all error codes exist in code). Not run through an OpenAPI linter.                                                 |

There are no benchmark numbers in this repository. A number without its hardware and load model is not a claim anyone can check; run `make bench` on your own machine.

## Architecture

```
Client -> net/http.Server -> root middleware -> router -> [API middleware] -> handlers -> domain -> worker pool
            timeouts,        RequestID,          Go 1.22   Auth (placeholder),  parse,     bounded   bounded queue,
            ConnState,       AccessLog,          ServeMux  RequireJSON,         validate,   fan-out,  backpressure,
            BaseContext      Metrics, Recovery   JSON 404  RequestTimeout        map errors  jobs      panic-safe
                                                 /405
```

Full diagram, dependency rules, and concurrency table: [docs/architecture.md](docs/architecture.md).

Concerns are separated on purpose:

- **Transport** (methods, paths, headers, JSON, status codes): `internal/httpapi`, `internal/server/middleware.go`
- **Application** (what a batch or job is, limits, execution rules): `internal/domain`, which does not import `net/http`
- **Infrastructure** (sockets, timeouts, pools, signals): `internal/server`, `internal/worker`, `cmd/server`

## Features

- `http.Server` with all five protective settings configured, validated for consistency, and explained ([docs/http-lifecycle.md](docs/http-lifecycle.md))
- Go 1.22 pattern routing, with 404 and 405 answered in the same JSON envelope as every other error
- Middleware: request ID, structured access log, metrics and timing, panic recovery, placeholder authentication, content-type enforcement, request deadline
- Synchronous batch endpoint with bounded concurrent fan-out tied to the request context
- Asynchronous jobs on a bounded worker pool, with explicit backpressure (`503` plus `Retry-After`)
- Two-phase graceful shutdown: drain, cancel with a cause, force close, then drain background work, all inside one deadline
- Readiness as a three-state machine (`starting`, `ready`, `draining`) separate from liveness
- Structured JSON logs with request-ID correlation; in-process metrics with a latency histogram; `/metrics` endpoint
- Consistent error model that distinguishes client errors, cancellation, timeouts, overload, and shutdown, and never leaks internals
- Standard library only: no third-party dependencies, no `go.sum`

## Why this project exists

Frameworks make the first hour of an HTTP service easy and hide the behavior that matters in production: what happens when a client stalls, disconnects, or floods you; what a deploy does to in-flight requests; why a counter is wrong under load. This repository builds those pieces directly on `net/http` so each behavior is visible, testable, and explained. It is a study of fundamentals, not a template for a product.

## Technical concepts demonstrated

| Concept                                               | Where                                                                                      |
| ----------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| Request/response lifecycle, TCP connection vs request | [docs/http-lifecycle.md](docs/http-lifecycle.md), `server.New` (`ConnState`, timeouts)     |
| `Handler`, `HandlerFunc`, composition                 | [docs/handlers.md](docs/handlers.md), `httpapi/handlers.go`, `httpapi/routes.go`           |
| Middleware as function composition; ordering effects  | [docs/middleware.md](docs/middleware.md), `TestMiddlewareOrderingDeterminesWhatIsObserved` |
| Concurrency, atomics, mutexes, channels, races        | [docs/concurrency.md](docs/concurrency.md), `domain/service.go`, `metrics/metrics.go`      |
| Context: cancellation, deadlines, causes, propagation | [docs/context.md](docs/context.md), `TestClientDisconnectCancelsServerSideWork`            |
| Graceful shutdown, draining, readiness                | [docs/graceful-shutdown.md](docs/graceful-shutdown.md), `server/shutdown.go`               |
| Backpressure and bounded resources                    | `worker/worker.go`, `domain/job.go`, `TestQueueFullReturns503WithRetryAfter`               |
| Error taxonomy and wrapping (`errors.Is`/`As`, `%w`)  | `httpapi/response.go` (`classify`), `domain/job.go`                                        |
| Failure analysis                                      | [docs/failure-modes.md](docs/failure-modes.md)                                             |
| Decisions and alternatives                            | [docs/decisions.md](docs/decisions.md)                                                     |
| Engineering Review                                    | [docs/engineering-review.md](docs/engineering-review.md)                                   |

## Repository structure

```
cmd/server/main.go              entry point: env, signals, exit code; healthcheck subcommand
internal/
  config/                       env loading, cross-field validation, fail fast
  server/                       composition root, http.Server setup, middleware, shutdown
  httpapi/                      handlers, routes, JSON envelope, error classification
  domain/                       batch execution, jobs, store, domain errors (no net/http)
  worker/                       bounded worker pool with backpressure
  health/                       liveness and readiness state machine
  metrics/                      atomic counters, latency histogram, gauges
  requestid/                    request ID generation and context carrying
  logging/                      structured logger that attaches the request ID
test/integration/               end-to-end tests over real TCP connections
docs/                           architecture, teaching documents, API, ADRs, failure modes
scripts/mutation-check.sh       optional mutation testing (see Testing)
.github/workflows/ci.yml        format, vet, build, test, race, coverage, benchmarks
Dockerfile  Makefile  LICENSE
```

The module path is `github.com/michael-emmanuel/go-production-http-server`

```
go mod edit -module github.com/michael-emmanuel/go-http-server
grep -rl 'github.com/michael-emmanuel/go-http-server' --include='*.go' . | \
  xargs sed -i 's#github.com/michael-emmanuel/go-http-server#github.com/michael-emmanuel/go-http-server#g'
```

## Getting started

Requires Go 1.22 or newer (for `ServeMux` method and wildcard patterns).

```
git clone https://github.com/michael-emmanuel/go-http-server.git
cd go-http-server
go run ./cmd/server
```

## Configuration

Environment variables, validated at startup; an invalid value lists every problem and exits 1.

| Variable                                                          | Default           | Notes                                            |
| ----------------------------------------------------------------- | ----------------- | ------------------------------------------------ |
| `HTTP_ADDR`                                                       | `:8080`           |                                                  |
| `HTTP_READ_HEADER_TIMEOUT`                                        | `5s`              | must be positive and not exceed the read timeout |
| `HTTP_READ_TIMEOUT`                                               | `10s`             |                                                  |
| `HTTP_WRITE_TIMEOUT`                                              | `15s`             |                                                  |
| `HTTP_IDLE_TIMEOUT`                                               | `60s`             |                                                  |
| `HTTP_MAX_HEADER_BYTES`                                           | `1048576`         | approximate limit                                |
| `HTTP_REQUEST_TIMEOUT`                                            | `10s`             | must be shorter than the write timeout           |
| `HTTP_MAX_BODY_BYTES`                                             | `65536`           |                                                  |
| `HTTP_SHUTDOWN_TIMEOUT`                                           | `30s`             | whole shutdown budget                            |
| `HTTP_SHUTDOWN_DRAIN_DELAY`                                       | `0s`              | at most half the shutdown timeout                |
| `WORK_MAX_ITEMS`, `WORK_MAX_ITEM_DELAY`, `WORK_BATCH_CONCURRENCY` | `1000`, `1s`, `8` | work limits                                      |
| `WORKER_COUNT`, `WORKER_QUEUE_SIZE`, `WORK_JOB_RETENTION`         | `4`, `64`, `1000` | async job capacity                               |
| `AUTH_TOKEN`                                                      | empty             | enables the **placeholder** bearer check         |
| `LOG_LEVEL`                                                       | `info`            |                                                  |

Rationale for each value, and the relationships between them: [docs/operational-guide.md](docs/operational-guide.md).

## Running locally

```
make run                                   # or: go run ./cmd/server
curl -i http://localhost:8080/health/live
curl -i http://localhost:8080/health/ready
curl 'http://localhost:8080/api/v1/work?items=10&delay_ms=20'
curl -X POST -H 'Content-Type: application/json' -d '{"items":5}' http://localhost:8080/api/v1/work
```

Send `SIGTERM` or press Ctrl-C to watch the shutdown sequence in the logs.

## Running tests

```
make test          # go test ./...
make race          # go test -race ./...
make cover         # coverage attributing integration tests to the packages they exercise
make check         # format check, vet, build, race tests
```

## Testing strategy

- **Unit tests** for each package, with fakes only where a real dependency would make a test nondeterministic (a fake executor that runs tasks when told to; a scripted service for handlers).
- **Handler and middleware tests** with `httptest`, including panic recovery, request IDs, auth, content-type enforcement, error mapping, and the exact production middleware order.
- **Concurrency tests** that assert exact results, not just "no crash": checksums that expose lost or duplicated work, peak-concurrency bounds, first-error cancellation with a join, and `-race`.
- **Integration tests** over real TCP: concurrent clients, client disconnect cancelling server-side work, request deadlines, backpressure, slow-header disconnection, header and body limits, and the graceful-shutdown paths (in-flight drain, readiness window, deadline cancellation, forced close, async job drain).
- **Determinism.** Tests wait on channels and on server-provided signals (`Ready()`, `Draining()`, a work function reporting it started), not on sleeps. Where a bound is unavoidable it is one-sided: a guard that a correct implementation can never trip. One test polls (job completion observed over HTTP) and says so.
- **Mutation testing.** `make mutation` copies the repo, breaks one behavior at a time (removing a join, dropping a lock, swapping middleware order, skipping the cancel phase, and so on), and confirms the suite fails. This found real problems: a race test that passed because an unrelated atomic accidentally synchronized the goroutines, a shutdown test that could not tell a missing worker drain from a present one, and a base-context cancellation that turned out to be redundant with `Close()`. All 33 mutants are now detected. The script is slow and its search strings track the source text, so it is opt-in rather than part of CI.

Coverage note: `go test -cover ./...` under-reports `internal/server` (under 50%) because its lifecycle code is exercised by `test/integration`, a different package. `make cover` uses `-coverpkg=./...` and reports about 95% overall. What is uncovered is mainly `main()` (unit-testing `os.Exit` is not useful) and defensive branches such as a `crypto/rand` failure.

## Race detection

```
go test -race ./...
```

CI runs this on every push and pull request. Shared state uses atomics or a mutex as tabulated in [docs/architecture.md](docs/architecture.md); the docs also cover the limits of what the detector can see.

## Benchmarks

```
make bench         # go test -run '^$' -bench . -benchmem ./...
```

Benchmarks cover request handling (routing, handler, JSON), middleware overhead (bare handler versus the production stack, serial and parallel), batch fan-out, and metrics counter contention. Benchmarks measure relative cost on one machine and allocations per operation; they do not measure real latency or throughput (no network, no real I/O) and are not portable across machines. CI runs them for visibility and never fails on their results. See [docs/operational-guide.md](docs/operational-guide.md#benchmarks) for what they can and cannot tell you, and for optional load-testing commands (`hey`, `wrk`, `vegeta`) with guidance on reading throughput, p50/p95/p99, error rate, and why averages mislead.

## Docker

```
make docker-build
make docker-run
```

Multi-stage build: a `golang:1.22-alpine` stage compiles a static binary (`CGO_ENABLED=0`, `-trimpath`, stripped); the runtime stage is `gcr.io/distroless/static-debian12:nonroot`, containing only the binary, CA certificates, and tzdata, with no shell or package manager, running as a non-root user. Because the image has no `curl`, `HEALTHCHECK` runs `/server healthcheck`, a subcommand that probes `/health/live` on the configured address. As noted above, the image build itself has not been run in the authoring environment.

## API documentation

[docs/api.md](docs/api.md) documents every endpoint (method, path, purpose, headers, body, query parameters, responses, status codes, errors, curl examples), using responses captured from the running binary. [docs/openapi.yaml](docs/openapi.yaml) is the machine-readable form.

| Endpoint                              | Purpose                                    |
| ------------------------------------- | ------------------------------------------ |
| `GET /health/live`                    | liveness                                   |
| `GET /health/ready`                   | readiness (503 while starting or draining) |
| `GET /metrics`                        | JSON metrics snapshot                      |
| `GET /api/v1/info`                    | build and runtime information              |
| `GET /api/v1/work?items=N&delay_ms=M` | run a batch concurrently, synchronously    |
| `POST /api/v1/work`                   | submit an asynchronous job (`202`)         |
| `GET /api/v1/work/{id}`               | job status and result                      |

## Architecture documentation

| Document                                            | Contents                                                                      |
| --------------------------------------------------- | ----------------------------------------------------------------------------- |
| [architecture.md](docs/architecture.md)             | diagram, layers, dependency rules, concurrency table, shutdown, observability |
| [http-lifecycle.md](docs/http-lifecycle.md)         | DNS to connection reuse; what each server timeout protects                    |
| [handlers.md](docs/handlers.md)                     | `Handler`, `HandlerFunc`, composition, a real routing bug and its fix         |
| [middleware.md](docs/middleware.md)                 | composition, ordering, panics in goroutines                                   |
| [concurrency.md](docs/concurrency.md)               | primitives and why each, the incorrect version, backpressure                  |
| [context.md](docs/context.md)                       | cancellation, deadlines, causes, where `Background()` is justified            |
| [graceful-shutdown.md](docs/graceful-shutdown.md)   | the sequence, orchestrator behavior, observed results                         |
| [failure-modes.md](docs/failure-modes.md)           | 14 failures with detection, impact, mitigation, remaining risk                |
| [decisions.md](docs/decisions.md)                   | ADR-001 to ADR-009                                                            |
| [operational-guide.md](docs/operational-guide.md)   | config, logs, metrics, security, load testing, scaling                        |
| [engineering-review.md](docs/engineering-review.md) | 20 questions with answers grounded in this code                               |

Each teaching document follows the same shape: mental model, code, production implications, a common mistake, behavior under load, discussion points.

## Graceful shutdown

```
SIGTERM -> readiness = draining -> (optional drain delay) -> Shutdown(): close listeners, drain in-flight
        -> [80% of budget elapsed] cancel base context (cause: unavailable) -> handlers answer 503
        -> [budget elapsed] close remaining connections -> drain worker pool -> exit
```

One deadline (`HTTP_SHUTDOWN_TIMEOUT`) covers the whole sequence. Exit status is 0 for a clean drain and 1 with a description otherwise. A second signal terminates immediately. Observed behavior with the real binary is tabulated in [docs/graceful-shutdown.md](docs/graceful-shutdown.md). Behavior inside an orchestrator such as Kubernetes is described there too (readiness before listener close, drain delay for routing convergence, timeout below the grace period); no manifests are shipped and it has not been run on Kubernetes.

## Operational considerations

- **Authentication is a placeholder.** `AUTH_TOKEN` enables one static shared secret. It is not production authentication. Without it the API is open and the server warns at startup.
- **TLS is assumed to terminate upstream.** `/health/*` and `/metrics` are unauthenticated and must be network-restricted.
- **No rate limiting and no connection cap.** Timeouts bound how long a connection is held, not how many are held.
- **Jobs are in memory.** A `202` is not durable across restarts, and a poll routed to a different instance returns 404.
- **`MaxHeaderBytes` is approximate.** Measured: with a limit of 1024, an 8192-byte header was rejected on a fresh connection and accepted on a reused one.
- **`Shutdown` polls** for idle connections with a backoff up to 500 ms, so shutdown can take slightly longer than the last request.

Remaining attack surface, remaining risks per failure mode, and scaling limits are in [docs/operational-guide.md](docs/operational-guide.md) and [docs/failure-modes.md](docs/failure-modes.md).

## Engineering tradeoffs

None of these is claimed as universally right; each states when it would be revisited ([docs/decisions.md](docs/decisions.md)).

- **`net/http` over a framework**: visibility of mechanics, zero dependencies; costs hand-written decoding, error envelope, and 404/405 handling.
- **Recovery inside logging and metrics**, not outermost: a panic is recorded as a 500 instead of a 200. Costs: a panic in the logging or metrics layers is not caught by recovery.
- **Bounded fan-out instead of a goroutine per item**: protects capacity from one large request; too low a limit serializes independent waits.
- **Explicit rejection over queueing**: a full queue returns `503` immediately. Clients must handle retries; the service stays bounded and observable.
- **Atomics for counters, a mutex for the job store**: atomics for independent words on the hot path, a mutex where several fields must change together.
- **Readiness does not reflect saturation**: taking a saturated instance out of rotation shifts its load onto its peers. Overload is shed per request instead.
- **Two-phase shutdown with an 80/20 split**: gives handlers a chance to answer `503` instead of dropping the connection. The split is a heuristic and a constant, not a knob.
- **JSON metrics, not Prometheus format**: keeps the focus on primitives; needs an adapter for real scraping.

## Future improvements

In rough order of value:

1. Durable job storage and a real queue, so `202` is a promise the process can keep and the API scales horizontally.
2. Per-client rate limiting and a connection limit; a global concurrency limiter.
3. Real authentication (OIDC/JWT or mTLS) replacing the placeholder.
4. Prometheus exposition and OpenTelemetry tracing; labeled metrics with cardinality control.
5. Build and run the container image in CI with a smoke test that exercises `SIGTERM`; publish a signed image.
6. Route-pattern labels in logs and metrics (`Request.Pattern`, available from Go 1.23).
7. Fuzz tests for the JSON decoding path.
8. Optional Kubernetes manifests, only once the service has actually been deployed to a cluster.

## License

MIT. See [LICENSE](LICENSE).
