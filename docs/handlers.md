# Handlers

## 1. Mental model

A handler is anything that can answer an HTTP request:

```go
type Handler interface {
    ServeHTTP(ResponseWriter, *Request)
}
```

That single-method interface is the whole abstraction. Routers, middleware, and
your endpoints are all `Handler`s that call, wrap, or dispatch to other
`Handler`s. That uniformity is what makes them composable.

`http.HandlerFunc` exists because most handlers are a single function and
declaring a struct with one method for each would be noise:

```go
type HandlerFunc func(ResponseWriter, *Request)
func (f HandlerFunc) ServeHTTP(w ResponseWriter, r *Request) { f(w, r) }
```

It is an adapter: a function type with a method, so any function with the right
signature satisfies `Handler`. `mux.Handle("GET /x", http.HandlerFunc(fn))` is a
conversion, not a call.

## 2. Code example

`httpapi.Handler` is a struct holding the dependencies its methods share, with
methods matching the `HandlerFunc` signature:

```go
type Handler struct {
    work    WorkService
    log     *slog.Logger
    maxBody int64
    ...
}

func (h *Handler) RunWork(w http.ResponseWriter, r *http.Request) {
    items, err := intParam(r.URL.Query(), "items", defaultItems) // transport: parse
    if err != nil { h.fail(w, r, err); return }
    res, err := h.work.RunBatch(r.Context(), domain.BatchRequest{...}) // application
    if err != nil { h.fail(w, r, err); return }
    h.respond(w, r, http.StatusOK, batchResponse{...})              // transport: encode
}
```

The route table binds them, and is the only place that knows the URL layout:

```go
mux.Handle("GET /api/v1/work", api(http.HandlerFunc(d.Handler.RunWork)))
```

`d.Handler.RunWork` is a method value; `http.HandlerFunc(...)` adapts it; `api`
wraps it with policy (`Auth`, `RequireJSON`, `RequestTimeout`).

A handler has three jobs, in this order: validate input (transport-level parsing
here, business rules in the domain), call the application with the request
context, and map the outcome (including every error) to exactly one response.
It does not contain business rules, does not call `context.Background()`, and
does not write error text derived from internal errors.

## 3. Production implications

- **One error path.** All failures funnel through `fail`, which classifies the
  error (`classify`), logs the full internal error, and writes only a generic,
  client-safe message. The request ID appears in both, so a client report can be
  matched to the internal log. `TestInternalErrorsNeverLeakToClients` plants a
  connection string in an error and asserts it never reaches the body.
- **Encode before writing.** `respond` marshals first. If marshalling fails, no
  header has been sent yet and the client gets a clean 500 instead of a
  truncated 200.
- **Handlers are shared.** One `Handler` value serves every request
  concurrently. Its fields are immutable after construction, so no locking is
  needed. Adding a mutable field would require thinking about that.
- **Consumer-side interface.** `httpapi.WorkService` lists only the three methods
  the handlers call. That is why handler tests use a scripted fake, and why
  `httpapi` does not depend on the concrete `domain.Service`.
- **Router owns 404/405.** `ServeMux` writes plain-text 404 and 405 responses.
  `router.ServeHTTP` runs the mux's handler against a probe writer to learn which
  it would send, then writes the JSON envelope. The `Allow` header is preserved.

## 4. Common mistake

Calling `mux.Handler(r)` and then invoking the returned handler yourself. In Go
1.22, `Handler` only _looks up_ the match; wildcard values for `r.PathValue`
are populated inside `ServeMux.ServeHTTP`. Our router originally did this, and
`GET /api/v1/work/{id}` received an empty id. The curl smoke test did not
exercise that route; the unit test `TestGetWork` did. The fix and its reason are
commented in `routes.go`, and a mutation test now guards it.

Other frequent mistakes: writing the header twice (`superfluous
response.WriteHeader call`), writing to the body after an error response, and
forgetting to `return` after `h.fail(...)`.

## 5. What happens under load

Handler code runs on the connection goroutine. If a handler blocks (waiting on a
slow dependency, or on `wg.Wait`), that connection is unavailable and, on
HTTP/1.1, so is the pipeline behind it. Nothing in `net/http` limits how many
handlers run at once. Under a traffic spike the server accepts every connection
it can and lets latency rise. The controls that bound this here are the request
deadline (work stops), the bounded fan-out inside a request, and the bounded
worker queue (asynchronous work is rejected with 503 rather than accepted
without limit).
