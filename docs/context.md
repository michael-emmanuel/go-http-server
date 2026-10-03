# Context

## 1. Mental model

`context.Context` is not a bag for values. Its job is to carry three things
across API and goroutine boundaries:

1. **Cancellation**: "stop, nobody needs this result any more."
2. **Deadlines**: "stop by this time regardless."
3. **Request-scoped values**: a small amount of data that belongs to one request
   (here, only the request ID).

Cancellation is a _signal_, not a kill. A cancelled context stops work only if
the work is written to notice: by selecting on `ctx.Done()`, or by calling
something that does (`http.NewRequestWithContext`, `database/sql` `*Context`
methods). Code that never looks at the context runs to completion regardless.

A timeout and a cancellation are different causes of the same signal.
`ctx.Err()` distinguishes them: `context.DeadlineExceeded` (time ran out) versus
`context.Canceled` (someone called `cancel`). `context.Cause(ctx)` can carry a
more specific reason, which we use for shutdown.

Contexts form a tree. Cancelling a parent cancels every descendant; cancelling a
child does not affect its parent or siblings.

```
BaseContext (cancelled on shutdown deadline, cause = ErrUnavailable)
  └── request context (also cancelled when the client disconnects or ServeHTTP returns)
        └── WithTimeout(HTTP_REQUEST_TIMEOUT)         [RequestTimeout middleware]
              └── WithValue(request id)               [RequestID middleware]
                    └── WithCancel                    [Service.process, cancels siblings on first error]
```

(Values are added above the timeout in code order; the tree shape is what
matters.)

## 2. Code example

Making a blocking operation cancellable (`domain.SimulatedWork`):

```go
t := time.NewTimer(delay)
defer t.Stop()
select {
case <-t.C:                    // finished normally
case <-ctx.Done():
    return 0, ctx.Err()        // stop early, tell the caller why
}
```

`time.Sleep(delay)` cannot be interrupted, which is why it is not used. A
CPU-bound loop needs the equivalent periodic check:

```go
for i := range items {
    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
    }
    process(i)
}
```

Propagation: the handler passes the request context down, and nothing in between
substitutes `context.Background()`:

```go
res, err := h.work.RunBatch(r.Context(), req)     // httpapi
values, err := s.process(ctx, req)                 // domain
v, err := s.work(ctx, i, delay)                    // per-item, in its own goroutine
```

Telling a client disconnect from a shutdown (`Handler.fail`):

```go
if errors.Is(err, context.Canceled) &&
   errors.Is(context.Cause(r.Context()), domain.ErrUnavailable) {
    err = fmt.Errorf("%w: %w", domain.ErrUnavailable, err)   // -> 503 + Retry-After
}
// otherwise a plain cancellation means the client left -> 499 in logs and metrics
```

Where `context.Background()` _is_ used, on purpose:

| Place                            | Why not the request context                                                                                                                        |
| -------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| Shutdown context (`shutdown.go`) | The signal context is already cancelled; deriving from it would mean "zero time to shut down".                                                     |
| `BaseContext` root               | It is the root; requests derive from it.                                                                                                           |
| Worker pool context              | An accepted asynchronous job must outlive the request that created it. A job under `r.Context()` would be cancelled the moment the 202 is written. |

Why not store a context in a struct: a context is scoped to an operation, and a
struct usually outlives operations. Stashing one couples the struct's lifetime
to a single request's (a later call would silently run under an old, possibly
cancelled context) and hides the cancellation dependency from callers. Pass it
explicitly as the first parameter. The exception in the standard library
(`http.Request` holds one) is a request object, which is itself operation-scoped.

## 3. Production implications

- **Client disconnects.** `net/http` cancels the request context when the
  connection closes. `TestClientDisconnectCancelsServerSideWork` shows the
  server-side work observing `context.Canceled` after the client hangs up.
  Without propagation, the work keeps burning CPU for a response nobody will
  read.
- **Deadlines.** `RequestTimeout` attaches a deadline to API requests. The result
  is a `504` with the standard envelope
  (`TestRequestDeadlineProduces504`). `defer cancel()` releases the timer as
  soon as the handler returns; omitting it leaks the timer until the deadline
  and `go vet`'s `lostcancel` check flags it.
- **Values are for request-scoped metadata only.** The one value here is the
  request ID, under an unexported key type so no other package can collide with
  it. Dependencies (loggers, stores, config) go in constructors, where the
  compiler can see them.
- **`context.Cause`.** Cancelling with a cause costs nothing and turns "cancelled"
  into "cancelled because the server is shutting down".

## 4. Common mistake

Starting background work with the request's context, then returning:

```go
go doAsync(r.Context())   // BAD: cancelled the moment the handler returns
```

or, the mirror image, using `context.Background()` inside request handling and
thereby making the work uncancellable, so it survives client disconnects,
deadlines, and shutdown. A third: ignoring the `cancel` function returned by
`WithTimeout` / `WithCancel`.

## 5. What happens under load

A slow dependency plus no deadline is the classic outage: requests pile up
behind it, each holding a goroutine and connection, until resources run out.
Deadlines cap how long any one request can hold on. Cancellation on disconnect
matters most under load, because overloaded servers are slow, slow servers make
clients give up and retry, and without cancellation the abandoned work
(now duplicated by the retries) keeps consuming capacity. That feedback loop is a
common cause of metastable overload.
