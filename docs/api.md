# API reference

Base URL in the examples: `http://localhost:8080`. Every example response below
was captured from the running binary; timestamps and IDs are abbreviated.
A machine-readable description is in [openapi.yaml](openapi.yaml).

## Conventions

**Content type.** All bodies are JSON (`application/json; charset=utf-8`).
Request bodies must be sent with `Content-Type: application/json`.

**Authentication.** Endpoints under `/api/` require
`Authorization: Bearer <token>` **only if** the server was started with
`AUTH_TOKEN`. Without it, the API is open and the server logs a warning at
startup. This is a placeholder mechanism, not production authentication (see
[operational-guide.md](operational-guide.md#security)). `/health/*` and `/metrics`
never require credentials.

**Request IDs.** Every response carries `X-Request-ID`. A valid inbound
`X-Request-ID` (1 to 64 characters from `A-Za-z0-9._-`) is echoed; anything else
is replaced with a generated 32-character hex ID.

**Error format.** Every error produced by this service, including 404 and 405
from the router and 401 and 415 from middleware, uses one envelope:

```json
{"error": {"code": "invalid_request", "message": "human-readable text", "request_id": "..."}}
```

`code` is stable and meant for programs; `message` is for humans and may change.
Internal details are never included.

**Errors produced before our code runs.** Errors detected by `net/http` before a
request is dispatched (header block too large, malformed request line) are
written by the standard library in plain text, not in this envelope. Example:
`431 Request Header Fields Too Large`.

**Idempotency and retries.** `503` responses carry `Retry-After: 1`. `GET`
requests are safe to retry. `POST /api/v1/work` is *not* idempotent: a retry after
an ambiguous failure creates another job.

### Error codes

| HTTP | `code` | When |
|---|---|---|
| 400 | `invalid_request` | Bad query parameter, unknown JSON field, wrong JSON type, empty body, more than one JSON value, value out of range. |
| 400 | `malformed_json` | Body is not valid JSON or is truncated. |
| 401 | `unauthorized` | Missing or wrong bearer token (only when `AUTH_TOKEN` is set). Includes `WWW-Authenticate: Bearer realm="api"`. |
| 404 | `not_found` | Unknown path, or unknown job ID. |
| 405 | `method_not_allowed` | Path exists but not for this method. Includes `Allow`. |
| 408 | `request_timeout` | A network timeout occurred while reading the request body. |
| 413 | `payload_too_large` | Body exceeds `HTTP_MAX_BODY_BYTES` (default 65536). |
| 415 | `unsupported_media_type` | POST/PUT/PATCH without `Content-Type: application/json`. |
| 499 | `client_closed_request` | The client disconnected. Recorded in logs and metrics; the client never sees it. Not an IANA-registered status. |
| 500 | `internal_error` | Unexpected failure. Generic message; the cause is in the server log under the same request ID. |
| 503 | `overloaded` | The job queue or store is full. `Retry-After: 1`. |
| 503 | `unavailable` | The server is shutting down. `Retry-After: 1`. |
| 504 | `timeout` | The request deadline (`HTTP_REQUEST_TIMEOUT`, default 10 s) was exceeded. |

---

## Operational endpoints

### `GET /health/live`

**Purpose.** Liveness: is the process able to answer HTTP? Checks nothing else.
An orchestrator should restart the process if this fails.

**Headers / query / body.** None required.

**Responses.** `200` always, while the process is serving.

```
$ curl -i http://localhost:8080/health/live
HTTP/1.1 200 OK
Cache-Control: no-store
Content-Type: application/json; charset=utf-8
X-Request-Id: <32 hex chars>
Content-Length: 16

{"status":"ok"}
```

### `GET /health/ready`

**Purpose.** Readiness: should this instance receive traffic? A load balancer
should stop routing to the instance when this returns 503.

**Responses.**

| Status | Body | Meaning |
|---|---|---|
| 200 | `{"status":"ready"}` | Accepting traffic. |
| 503 | `{"status":"starting"}` | Not yet serving. |
| 503 | `{"status":"draining"}` | Shutdown has begun. Terminal for this process. |

```
$ curl -i http://localhost:8080/health/ready
HTTP/1.1 200 OK
Content-Type: application/json; charset=utf-8

{"status":"ready"}
```

### `GET /metrics`

**Purpose.** In-process metrics snapshot as JSON (not Prometheus format).
Unauthenticated; restrict at the network layer.

**Response `200`.**

```
$ curl http://localhost:8080/metrics
{"requests_total":8,"requests_in_flight":1,"active_connections":1,
 "responses_by_class":{"1xx":0,"2xx":2,"3xx":0,"4xx":6,"5xx":0},
 "client_errors_total":6,"server_errors_total":0,
 "latency":{"count":8,"sum_seconds":0.001054,"buckets":[{"le_seconds":0.001,"count":8},...],
            "p50_seconds":0.001,"p95_seconds":0.001,"p99_seconds":0.001},
 "gauges":{"jobs_canceled_total":0,"jobs_failed_total":0,"jobs_stored":1,
           "jobs_succeeded_total":1,"work_items_processed_total":5,"worker_queue_depth":0}}
```

`requests_in_flight` includes the request reading it. Latency buckets are
cumulative; the quantiles are upper-bound estimates (the upper edge of the bucket
containing the quantile), saturating at 10 s. See the operational guide.

---

## API endpoints

### `GET /api/v1/info`

**Purpose.** Build and runtime information.

**Headers.** `Authorization` if auth is enabled.

**Response `200`.**

```
$ curl -H 'Authorization: Bearer dev-secret' http://localhost:8080/api/v1/info
{"service":"go-production-http-server","version":"dev","go_version":"go1.22.2",
 "started_at":"<ts>","uptime_seconds":12.3,"goroutines":11,"gomaxprocs":1}
```

`version` comes from `-ldflags "-X main.version=..."` (default `dev`).
`goroutines` is a coarse leak indicator.

**Status codes.** 200, 401.

### `GET /api/v1/work`

**Purpose.** Run a batch of independent simulated work items **concurrently and
synchronously**. Items run under a bounded fan-out (default 8 at a time) tied to
the request context: a client disconnect or the request deadline stops the work.
Each item waits `delay_ms` (cancellably) and returns `index²`; `checksum` is the
sum, which makes lost or duplicated work detectable.

**Query parameters.**

| Name | Type | Default | Constraint |
|---|---|---|---|
| `items` | integer | `10` | 1 to `WORK_MAX_ITEMS` (default 1000) |
| `delay_ms` | integer | `10` | 0 to `WORK_MAX_ITEM_DELAY` in ms (default 1000) |

**Response `200`.**

```
$ curl -H 'Authorization: Bearer dev-secret' \
       'http://localhost:8080/api/v1/work?items=10&delay_ms=20'
{"items":10,"checksum":285,"concurrency":8,"elapsed_ms":40.728396}
```

`concurrency` is `min(WORK_BATCH_CONCURRENCY, items)`. With 10 items at 20 ms and
concurrency 8, two waves run, so `elapsed_ms` is about 40.

**Errors.**

```
$ curl -i -H 'Authorization: Bearer dev-secret' 'http://localhost:8080/api/v1/work?items=0'
HTTP/1.1 400 Bad Request
{"error":{"code":"invalid_request","message":"invalid parameter: items must be between 1 and 1000","request_id":"<id>"}}
```

**Status codes.** 200, 400, 401, 499 (client left), 500, 503 (`unavailable` during
shutdown), 504 (deadline).

### `POST /api/v1/work`

**Purpose.** Submit the same kind of batch as an **asynchronous job**. Returns
immediately with `202 Accepted`; a fixed pool of background workers runs it. The
job outlives the request: it runs under the worker pool's context, not the
request's.

**Headers.** `Content-Type: application/json` (required), `Authorization` if
enabled.

**Request body.** A single JSON object. Unknown fields are rejected. All fields
optional.

| Field | Type | Default | Constraint |
|---|---|---|---|
| `items` | integer | `10` | 1 to `WORK_MAX_ITEMS` |
| `delay_ms` | integer | `10` | 0 to `WORK_MAX_ITEM_DELAY` in ms |

The body is limited to `HTTP_MAX_BODY_BYTES` (default 64 KiB).

**Response `202`.** `Location` header points at the job.

```
$ curl -i -X POST -H 'Authorization: Bearer dev-secret' \
       -H 'Content-Type: application/json' \
       -d '{"items":5,"delay_ms":10}' http://localhost:8080/api/v1/work
HTTP/1.1 202 Accepted
Location: /api/v1/work/<job-id>
Content-Type: application/json; charset=utf-8

{"id":"<job-id>","status":"queued","items":5,"delay_ms":10,"created_at":"<ts>"}
```

**Errors.**

```
$ curl -s -X POST -H 'Authorization: Bearer dev-secret' -d '{}' http://localhost:8080/api/v1/work
{"error":{"code":"unsupported_media_type","message":"Content-Type must be application/json","request_id":"<id>"}}

$ curl -s -X POST ... -H 'Content-Type: application/json' -d '{"items":' ...
{"error":{"code":"malformed_json","message":"request body is not valid JSON","request_id":"<id>"}}

$ curl -s -X POST ... -H 'Content-Type: application/json' -d '{"bogus":1}' ...
{"error":{"code":"invalid_request","message":"request body contains an unknown field","request_id":"<id>"}}
```

**Status codes.** 202, 400, 401, 413, 415, 503 (`overloaded` when the queue or
store is full, `unavailable` during shutdown; both with `Retry-After`), 500.

Jobs are held in the memory of the instance that accepted them and are lost on
restart.

### `GET /api/v1/work/{id}`

**Purpose.** Fetch a job's status and result.

**Path parameter.** `id`: the job ID from the `POST` response.

**Response `200`.**

```
$ curl -H 'Authorization: Bearer dev-secret' http://localhost:8080/api/v1/work/<job-id>
{"id":"<job-id>","status":"succeeded","items":5,"delay_ms":10,
 "created_at":"<ts>","started_at":"<ts>","finished_at":"<ts>",
 "result":{"items":5,"checksum":30,"concurrency":5,"elapsed_ms":10.330445}}
```

`status` is one of `queued`, `running`, `succeeded`, `failed`, `canceled`.
`started_at`, `finished_at`, and `result` appear when applicable. For `failed`
and `canceled`, `failure` is `internal_error` or `canceled`. The underlying
error is logged, not returned.

**Errors.**

```
$ curl -H 'Authorization: Bearer dev-secret' http://localhost:8080/api/v1/work/nope
{"error":{"code":"not_found","message":"resource not found","request_id":"<id>"}}
```

An unknown ID and an evicted ID are indistinguishable. At most
`WORK_JOB_RETENTION` (default 1000) jobs are retained; the oldest finished job is
evicted first.

**Status codes.** 200, 401, 404, 499, 503.

---

## Router behavior

Unknown paths and wrong methods use the standard envelope:

```
$ curl -i -X DELETE -H 'Authorization: Bearer dev-secret' http://localhost:8080/api/v1/work
HTTP/1.1 405 Method Not Allowed
Allow: GET, HEAD, POST
Content-Type: application/json; charset=utf-8

{"error":{"code":"method_not_allowed","message":"method not allowed for this endpoint","request_id":"<id>"}}

$ curl http://localhost:8080/nope
{"error":{"code":"not_found","message":"no such endpoint","request_id":"<id>"}}
```

`HEAD` is accepted wherever `GET` is (a property of Go's `ServeMux` patterns).

Note that the 405 for `DELETE` above is returned without a credential check
because the router answers method mismatches before the API policy runs; it
reveals only which methods exist.
