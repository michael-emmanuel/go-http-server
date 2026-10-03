# Middleware

## 1. Mental model

Middleware is function composition over handlers. A middleware is a function
from `Handler` to `Handler`:

```go
type Middleware func(http.Handler) http.Handler
```

It receives the "next" handler and returns a new handler that does something
before and/or after calling it. Stacking middleware is nested function
application:

```
RequestID(AccessLog(Metrics(Recovery(router))))
```

The outermost function sees the request first and the response last, like
layers of an onion. `Chain` writes that nesting in reading order:

```go
func Chain(h http.Handler, mws ...Middleware) http.Handler {
    for i := len(mws) - 1; i >= 0; i-- { // build from the inside out
        h = mws[i](h)
    }
    return h
}
// Chain(h, A, B, C) == A(B(C(h)))
```

`TestChainOrderOutermostFirst` records the trace `A:in B:in C:in handler C:out B:out A:out`.

## 2. Code example

The shape of every middleware in `internal/server/middleware.go`:

```go
func RequestTimeout(d time.Duration) Middleware {       // configuration in the outer function
    return func(next http.Handler) http.Handler {        // the Middleware itself
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            ctx, cancel := context.WithTimeout(r.Context(), d)
            defer cancel()
            next.ServeHTTP(w, r.WithContext(ctx))        // hand a modified request down
        })
    }
}
```

The chain in production (`newRootHandler`) and the API-only policy:

```
every request:   RequestID -> AccessLog -> Metrics -> Recovery -> router
/api/* only:                                         Auth -> RequireJSON -> RequestTimeout -> handler
```

| Middleware       | Purpose                                                                                | Notes                                                                                                                      |
| ---------------- | -------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| `RequestID`      | Accept a valid inbound `X-Request-ID` or generate one; echo it; put it in the context. | Invalid inbound IDs (spaces, over 64 chars, non-ASCII) are replaced, not echoed, to prevent log and header injection.      |
| `AccessLog`      | One structured line per request.                                                       | Deferred so it logs even when a panic is re-raised. Logs the path, never the query string or headers. Probes log at debug. |
| `Metrics`        | In-flight gauge, request count, status class, latency histogram.                       | This is the "request timing" layer.                                                                                        |
| `Recovery`       | Turn a handler panic into a JSON 500 and log the stack.                                | Re-raises `http.ErrAbortHandler`; aborts the connection if the response has already started.                               |
| `Auth`           | **Placeholder** bearer-token check.                                                    | Not production authentication. See [operational-guide.md](operational-guide.md).                                           |
| `RequireJSON`    | 415 for POST/PUT/PATCH without `application/json`.                                     | Fails early with a precise error.                                                                                          |
| `RequestTimeout` | Attach a deadline to the request context.                                              | Cooperative; see [context.md](context.md).                                                                                 |

`responseRecorder` wraps the `ResponseWriter` so outer layers can learn the
status and size, which the interface does not expose. `recorderFor` reuses an
existing recorder so wrappers do not stack, and `Unwrap()` keeps
`http.ResponseController` working (flush, deadlines) through the wrapper.

## 3. Production implications

**Order changes what is observable.** Two orderings are pinned by tests:

1. _Request ID before logging._ If `AccessLog` runs outside `RequestID`, its
   `r` does not yet carry the ID, so log lines lose `request_id` and cannot be
   correlated with the response header or with error bodies.
   (`TestProductionChainRecordsPanicAs500`.)
2. _Recovery inside logging and metrics._ Recovery writes the 500 after the
   handler panics. Layers outside it then record 500. If it were outermost, they
   would unwind through the panic first and record the default 200, and a
   crashing endpoint would look healthy on every dashboard.
   (`TestMiddlewareOrderingDeterminesWhatIsObserved` shows both orders with the
   same handler.)

Corollary: Recovery only catches panics in layers _below_ it. A panic in
`AccessLog` or `Metrics` is not caught by it; those are kept trivially small, and
`net/http` still recovers per connection as a last resort.

**Panics in goroutines you start are not recovered by any middleware.**
`net/http` and `Recovery` protect the handler goroutine only. A panic in
`go func() {...}` terminates the process. That is why `domain.runItem` and
`worker.runTask` each install their own `recover`.

**Policy belongs in the route table.** Rather than a global `Auth` that skips
`/health` by string comparison, `httpapi.Routes` takes an `API` wrapper and
applies it only to `/api/*` routes.

## 4. Common mistake

Writing the response, then continuing the chain, or forgetting to call `next` on
the success path. Also: a middleware that captures the request and mutates the
context on the _original_ request. Contexts are immutable; you must pass
`r.WithContext(newCtx)` to `next`. Forgetting this silently drops the value.

A subtler one: logging inside `AccessLog` after `next.ServeHTTP` returns without
`defer`. If anything panics through it, no log line is written for exactly the
requests you most want to see.

## 5. What happens under load

Middleware runs on every request, so its cost is multiplied by traffic.
`BenchmarkMiddlewareOverhead` compares a bare handler with the production stack
so you can measure the cost on your own hardware (`make bench`); the number is
machine-specific and is not quoted here. The design keeps hot paths cheap:
counters are atomics (no lock convoy), one recorder is shared, and the request
ID is one formatted string. The dominant cost in practice is usually logging I/O,
which is why probes log at debug.
