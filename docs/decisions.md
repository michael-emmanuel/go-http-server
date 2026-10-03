# Engineering decisions

Each record gives the context, the decision, the alternatives considered, the
tradeoffs, and the consequences. None of these choices is universally correct;
each states the circumstances under which it would be revisited.

---

## ADR-001: Standard library HTTP server instead of a framework

**Context.** The project exists to show what happens beneath HTTP abstractions,
and the service needs only routing, JSON, middleware, and lifecycle control.

**Decision.** Use `net/http` with Go 1.22's pattern-based `ServeMux`. No
third-party dependencies, which is why the repository has no `go.sum`.

**Alternatives considered.**
- *Gin / Echo / Fiber.* Provide binding, validation, and route groups, but each
  introduces its own context type and handler signature, so `context.Context`
  propagation, `http.Handler` composition, and shutdown behavior become the
  framework's rather than the reader's. Fiber additionally replaces `net/http`
  with `fasthttp`, which changes semantics (for example around request-body
  reuse) that would have to be learned separately.
- *chi.* Thin and `http.Handler`-compatible, so a defensible choice. Before Go
  1.22 it was the pragmatic answer for path parameters. With method patterns and
  `PathValue` in the standard library its main advantage disappears for a table
  this size.

**Tradeoffs.** We hand-write JSON decoding hardening, the error envelope, and
404/405 formatting. A framework would give some of that for free, with less
visibility into its behavior.

**Consequences.** Zero supply-chain surface for the server itself. Everything a
reader wants to inspect (timeouts, `ConnState`, `BaseContext`) is directly
visible. Revisit if the route table grows to the point that hand-rolled
validation and versioning become the dominant cost.

---

## ADR-002: Explicit dependency injection through constructors

**Context.** Handlers need a service, a logger, and limits. Tests need to
substitute the service.

**Decision.** `httpapi.NewHandler(Deps)` takes its collaborators; `server.New` is
the single composition root. No package-level mutable state and no `init()`
wiring. Interfaces (`httpapi.WorkService`, `domain.Executor`) are declared by the
consumer and kept to the methods it uses.

**Alternatives considered.**
- *Package-level singletons* (a global logger, a global store). Shorter, but
  tests share state and cannot run in parallel, and construction order becomes
  implicit.
- *A DI framework (wire, fx).* Solves a problem this graph (about eight objects)
  does not have.
- *Interfaces everywhere.* Rejected: an interface with one implementation and no
  test double is indirection without a purpose. The two interfaces that exist
  each have a fake in the tests.

**Tradeoffs.** `server.New` is a long function that spells the wiring out.

**Consequences.** Any component can be constructed in isolation with fakes, which
is what allows the deterministic tests in `internal/domain` (a fake executor that
runs tasks when told to) and `internal/httpapi` (a scripted service).

---

## ADR-003: Middleware ordering

**Context.** Order changes what each layer can observe.

**Decision.** For every route: `RequestID → AccessLog → Metrics → Recovery`, then
the router. For `/api/*` routes only: `Auth → RequireJSON → RequestTimeout`, then
the handler.

- `RequestID` is outermost because everything after it reads the ID from the
  context. Swapping it with `AccessLog` makes log lines lose their `request_id`
  (asserted by `TestProductionChainRecordsPanicAs500`).
- `Recovery` is *inside* `AccessLog` and `Metrics`, not outside as in a common
  textbook diagram. When Recovery writes the 500, the layers that wrap it observe
  a 500. If Recovery were outermost, a panic would unwind through them before
  Recovery ran; they would log and count the default 200 and the incident would
  be invisible in the dashboards. `TestMiddlewareOrderingDeterminesWhatIsObserved`
  demonstrates both orders.
- `Auth` is first among the API layers so unauthenticated callers cost as little
  as possible, and `RequestTimeout` is last so the deadline clock starts only
  when real work is about to begin.
- Probes and `/metrics` bypass the API layers: orchestrators must be able to
  reach them without credentials.

**Alternatives considered.** Recovery outermost (rejected above); applying
`Auth` globally and exempting paths by string comparison (rejected: an
allow-list of paths inside the middleware is easy to get wrong and hides policy
from the route table).

**Tradeoffs.** A panic inside `AccessLog` or `Metrics` themselves is not caught
by `Recovery`. Those layers are small and deliberately contain no logic that can
panic. `net/http` still recovers per connection as the last line of defense.

**Consequences.** The route table (`httpapi.Routes`) is the single place that
says which policy applies to which URL.

---

## ADR-004: Context propagation strategy

**Context.** Request cancellation, deadlines, and shutdown signals must reach the
goroutines doing work.

**Decision.**
- Pass `context.Context` explicitly as the first parameter. Never store it in a
  struct.
- Handlers pass `r.Context()` to the service. `context.Background()` appears
  only in three places, each deliberate: the shutdown context (the signal context
  is already cancelled), the server's base context, and the worker pool's
  context (an accepted job must outlive the request that created it).
- Request IDs travel in the context; everything else (limits, dependencies)
  travels in constructor arguments.
- Cancellation is cooperative. Work stops promptly only if it waits on
  `ctx.Done()`. `SimulatedWork` uses a `time.Timer` in a `select`, not
  `time.Sleep`, for exactly this reason.

**Alternatives considered.** Storing a logger or a user in the context (rejected:
hides dependencies and makes them impossible to check at compile time);
`http.TimeoutHandler` for deadlines (rejected: it buffers the entire response
and returns a fixed plain-text body, and it still cannot stop work that ignores
the context).

**Consequences.** Cancellation is testable end to end
(`TestClientDisconnectCancelsServerSideWork`). The cost is discipline: every
blocking call in future code must accept and honor a context.

---

## ADR-005: Graceful shutdown strategy

**Context.** Deployments send `SIGTERM`, wait a grace period, then `SIGKILL`.
In-flight requests should complete, but the process must exit within the budget.

**Decision.** A single overall deadline (`HTTP_SHUTDOWN_TIMEOUT`, default 30s)
split into two phases:

1. *Drain* for 80%: `http.Server.Shutdown` waits for in-flight requests.
2. *Cancel* for the last 20%: cancel the base context with cause
   `domain.ErrUnavailable`; handlers that honor their context return promptly and
   answer `503` with `Retry-After`. Only after that window are connections
   closed.

Then drain the worker pool with whatever remains, and return every failure
joined.

**Alternatives considered.**
- *`Shutdown` then `Close` immediately at the deadline.* This was the first
  implementation. Mutation testing showed the base-context cancel was redundant
  because `Close` already cancels request contexts as a side effect of closing
  connections; and clients saw a dropped connection instead of a response.
  The cooperative phase gives them a definite, retryable answer.
- *Cancel request contexts at signal time.* Rejected: it defeats draining. A
  request that would have finished in 200ms is aborted.
- *`os.Exit` in the signal handler.* Rejected: skips deferred cleanup and
  abandons in-flight work.

**Tradeoffs.** The 80/20 split is a heuristic. With a very short timeout the
cancel window is tiny; with a very long one it is generous. It is a constant
(`cancelWindowFraction`), not configuration, because a second knob would let
operators build configurations that do not add up.

**Consequences.** Shutdown outcomes are distinguishable: exit 0 (clean drain),
exit 1 with an error describing which phase failed. Verified against the real
binary with real signals for each of the paths.

---

## ADR-006: Readiness and liveness are separate, and readiness is a state machine

**Context.** Orchestrators use two probes with different consequences: a failed
liveness probe restarts the process; a failed readiness probe removes it from
load balancing.

**Decision.** `/health/live` reports 200 whenever the process can answer HTTP and
checks nothing else. `/health/ready` reports 200 only in `StateReady`; it is 503
in `starting` and `draining`. `draining` is terminal: `MarkReady` uses a
compare-and-swap from `starting`, so a shutting-down instance can never report
ready again.

**Alternatives considered.** One endpoint (rejected: a dependency outage would
either restart healthy processes or keep sending traffic to draining ones);
readiness that reflects worker-queue saturation (rejected: taking a saturated
instance out of rotation moves its load to its peers, which then saturate, a
cascading failure. Overload is answered with `503` on the specific request
instead.)

**Consequences.** There are no downstream dependencies today, so readiness is
purely lifecycle. If a required dependency is added, readiness should check it
with a short timeout; liveness should still not.

---

## ADR-007: Configuration from environment, validated at startup

**Context.** Containers and orchestrators configure processes through the
environment. Bad configuration should fail at deploy time, not under load.

**Decision.** `config.Load(getenv)` reads environment variables, applies
defaults, reports *all* parse errors together, then runs `Validate`, which checks
each field and cross-field relationships:

- `HTTP_READ_HEADER_TIMEOUT <= HTTP_READ_TIMEOUT`
- `HTTP_REQUEST_TIMEOUT < HTTP_WRITE_TIMEOUT` (so a timeout response can still be
  written before the connection's write deadline)
- `HTTP_SHUTDOWN_DRAIN_DELAY <= HTTP_SHUTDOWN_TIMEOUT / 2` (the delay is spent from the
  80% drain phase; a larger delay would leave in-flight requests no time to finish
  before being cancelled)
- zero is rejected for every timeout: in `net/http` zero means "no timeout".

The auth token is excluded from `Config.LogValue`, so logging the config cannot
leak it.

**Alternatives considered.** Config files (adds file-mounting and reload
semantics nobody asked for); a config library such as viper (heavy for about
twenty variables); flags (fine locally, awkward in containers).

**Consequences.** The startup error names the offending variable
(`TestRunFailsFastOnInvalidConfig`). Secrets in environment variables are visible
to anyone who can read the process environment; a secret manager is the upgrade
path.

---

## ADR-008: Asynchronous jobs with a bounded queue and explicit rejection

**Context.** `POST /api/v1/work` must return before the work finishes, which
raises "what if the work arrives faster than it completes".

**Decision.** A fixed pool of workers behind a fixed-size queue. `Submit` never
blocks: a full queue returns `ErrQueueFull`, surfaced as `503 overloaded` with
`Retry-After: 1`. The job store retains a bounded number of jobs and evicts the
oldest *finished* one; it refuses new work rather than dropping an active job.

**Alternatives considered.** Unbounded queue (memory grows without limit under
sustained overload, and latency grows silently); blocking `Submit` (moves the
queue into parked HTTP handler goroutines, which is the same unbounded queue with
worse visibility); a goroutine per job (no upper bound on concurrent work).

**Tradeoffs.** Jobs live in process memory. A crash loses them, and so does a
restart beyond the drain window. A durable queue is the honest upgrade path and
is deliberately not simulated here.

**Consequences.** Backpressure is testable
(`TestQueueFullReturns503WithRetryAfter`) and observable
(`worker_queue_depth` gauge).

---

## ADR-009: Status code choices for cancellation

**Context.** A cancelled context has two very different meanings.

**Decision.** If the request context was cancelled with cause
`domain.ErrUnavailable` (the server shutting down), respond `503` with
`Retry-After`: the client is waiting and should retry elsewhere. Otherwise a
cancellation means the client went away: record `499` (a de-facto nginx code, not
IANA-registered) for logs and metrics; the client will never read it. A deadline
exceeded is `504`.

**Alternatives considered.** Treat every cancellation as 503 (hides client
disconnects that indicate a slow endpoint); use 408 for deadline (408 means the
client was slow to send the request, which is not this case).

**Consequences.** Dashboards can tell "clients are giving up" (499) from "we are
shedding load" (503) from "we are too slow" (504).
