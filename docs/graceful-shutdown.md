# Graceful shutdown

## 1. Mental model

A deployment replaces old instances with new ones. The orchestrator sends
`SIGTERM`, waits a grace period, then sends `SIGKILL`. Graceful shutdown is what
the process does with that grace period so users do not see errors:

1. Stop receiving _new_ work.
2. Finish work already accepted.
3. Give up in a controlled way if that takes too long.

Killing the process instead drops every in-flight request as a connection reset
and abandons any accepted background work.

```
Running
  │  SIGTERM / SIGINT
  ▼
readiness = draining        load balancer stops routing here
  ▼
(drain delay, optional)     wait for routing to actually converge
  ▼
Shutdown(): listeners close, idle conns close, in-flight requests finish
  │  first 80% of HTTP_SHUTDOWN_TIMEOUT
  ▼  anything left?
cancel base context         handlers unwind and answer 503 + Retry-After
  │  last 20%
  ▼  anything left?
Close(): sever connections
  ▼
worker pool drain           queued jobs finish; cancelled at the deadline
  ▼
exit 0 (clean) or exit 1 with a description of what did not drain
```

## 2. Code example

Signal handling lives in `main` and only converts a signal into context
cancellation:

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
go func() { <-ctx.Done(); stop() }()   // restore default: a second signal kills us
return srv.ListenAndServe(ctx)
```

`Serve` blocks until that context is cancelled, then calls `shutdown`:

```go
s.health.MarkDraining()                                   // 1. readiness first
ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
drainCtx, _ := context.WithTimeout(ctx, timeout-timeout/5)

s.drainWait(drainCtx, d)                                  // 2. optional delay
if err := s.http.Shutdown(drainCtx); err != nil {         // 3. drain
    s.baseCancel(domain.ErrUnavailable)                   // 4. cancel with cause
    if err := s.http.Shutdown(ctx); err != nil {
        s.http.Close()                                    // 5. force
    }
}
s.pool.Shutdown(ctx)                                      // 6. background work
<-serveErr                                                // 7. reap the Serve goroutine
```

Design points:

- **Readiness flips first**, before anything else, so the earliest externally
  visible event is "stop sending me traffic". `TestReadinessFlipsBeforeListenerCloses`
  observes `/health/ready == 503` while `/health/live == 200` and the listener is
  still open.
- **The shutdown context is rooted in `Background`**, not the signal context,
  which is already cancelled.
- **One deadline for the whole sequence**, because an orchestrator's grace period
  is one wall-clock budget. Drain gets 80%, the cancel window 20%.
- **Cancellation has a cause.** Handlers see `context.Cause == ErrUnavailable`
  and answer `503 unavailable` with `Retry-After`, which a client or proxy can
  safely retry on another instance. Without the cause they would report `499`
  (client gone), which is wrong: the client is still waiting.
- **`os.Exit` appears once**, in `main`, after `run` has returned and every
  `defer` has run. Exit codes: 0 clean, 1 on any failure (including a drain that
  did not complete).
- **Second signal**: after the first signal, `stop()` restores the default
  action so a second Ctrl-C or `SIGTERM` terminates immediately. Verified against
  the built binary: exit 143 in 155 ms with a 30 s budget. (Caveat we hit: a
  background job in a non-interactive shell starts with `SIGINT` ignored, and Go
  preserves that after `signal.Stop`; use `SIGTERM` when testing that path from
  a script.)

### Behavior we observed with the real binary

| Scenario                                              | Result                                                                                                                                                      |
| ----------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `SIGTERM` while a request has ~0.6 s left, budget 5 s | Request completes with 200; exit 0; "shutdown complete".                                                                                                    |
| Request needs 1 s, budget 400 ms                      | Drain incomplete after 320 ms; handler cancelled; client receives `503 {"code":"unavailable"}`; exit 1; error names the drain failure; finished in ~322 ms. |
| Second `SIGTERM` during shutdown                      | Process terminates immediately (143).                                                                                                                       |

Also observed: `http.Server.Shutdown` polls for idle connections with a backoff
that reaches 500 ms, so a shutdown can take up to about half a second longer
than the last request. Budget for it.

## 3. Production implications

**Kubernetes and similar orchestrators.** On pod termination, the endpoint is
removed from Service routing and `SIGTERM` is sent concurrently, not in sequence.
Routing updates propagate asynchronously (kube-proxy, ingress controllers, cloud
load balancers), so for a short window traffic can still arrive after the signal.
If the process closes its listener immediately, those requests get connection
refused. The mitigation is `HTTP_SHUTDOWN_DRAIN_DELAY`: keep serving for a few
seconds after readiness fails. In our run with the default of `0`, the listener
closed immediately and readiness was not observable over HTTP, which is why the
delay exists. Rules of thumb:

- `HTTP_SHUTDOWN_TIMEOUT` must be shorter than the orchestrator's termination
  grace period (`terminationGracePeriodSeconds` in Kubernetes, default 30 s), or
  `SIGKILL` arrives mid-shutdown.
- The default `HTTP_SHUTDOWN_TIMEOUT` of 30 s equals that Kubernetes default, so
  in Kubernetes deployments lower it (or raise the grace period) rather than
  running with the two equal.
- `HTTP_SHUTDOWN_DRAIN_DELAY` counts against the same budget.

This repository ships no Kubernetes manifests and has not been deployed to
Kubernetes; the above describes how the process behaves and what an operator
should configure.

**Hijacked connections and long-lived streams** (WebSocket, SSE) are not
tracked by `http.Server.Shutdown`. This service has none; adding any means
registering them with `RegisterOnShutdown` and closing them explicitly.

**Background work.** Accepted jobs are drained after HTTP. Jobs still running at
the deadline are cancelled and recorded as `canceled`. Jobs are in memory:
anything cancelled or queued at exit is lost. A durable queue is the real fix.

## 4. Common mistake

- Calling `Shutdown` but never waiting for `Serve`'s return, or treating
  `http.ErrServerClosed` from `Serve` as a failure. `ListenAndServe` returns it
  _immediately_ when `Shutdown` starts; the drain is still in progress. Waiting
  on `Serve` instead of `Shutdown` exits early and drops requests.
- Deriving the shutdown context from the already-cancelled signal context.
- Failing readiness only _after_ closing the listener: the load balancer keeps
  sending to a server that refuses connections.
- Using `os.Exit` in the signal handler, which skips defers and in-flight work.
- Assuming `Shutdown` cancels request contexts. It does not; that is why the
  cancel phase exists.

## 5. What happens under load

Draining time is roughly the longest in-flight request. Under load there are
more of them, and slow requests dominate: a p99 of 8 s means some shutdowns need
8 s. The cancel phase caps this by turning "wait for the slowest" into "wait up
to the budget, then ask them to stop", so shutdown time is bounded even when
some handlers are slow. Handlers that ignore their context cannot be stopped
cooperatively, so the deadline falls through to `Close()` and those clients see
a dropped connection (`TestShutdownForcesCloseWhenHandlersIgnoreCancellation`).
Load also raises the number of requests that arrive during the delay window, so
if drain delay is too short, the tail of the routing convergence shows up as
errors.
