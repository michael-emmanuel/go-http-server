# HTTP request/response lifecycle

## 1. Mental model

An HTTP request is a message carried over a TCP connection. The two are
different things with different lifetimes:

- A **connection** is a long-lived byte stream. With HTTP/1.1 keep-alive one
  connection carries many requests, one at a time.
- A **request** is one message in, one message out.

`net/http` maps them like this: one goroutine per accepted connection, which
loops reading a request, calling your handler, writing the response, and going
back to reading. A slow client therefore occupies a goroutine and a file
descriptor for as long as it holds the connection, whether or not any request is
in progress. That is the reason the server-level timeouts exist.

```
client                                    server
  │  DNS: name -> IP                        │
  │  TCP: SYN / SYN-ACK / ACK  ───────────▶ │  kernel completes handshake, queues in accept backlog
  │  (TLS handshake, if any)                │  Accept() returns; ConnState=New; goroutine starts
  │  "GET /x HTTP/1.1\r\nHost: ...\r\n\r\n" │  ReadHeaderTimeout runs while headers are read
  │ ──────────────────────────────────────▶ │  Handler.ServeHTTP(w, r)   (request context created)
  │                                         │  ... handler writes status + headers + body
  │ ◀────────────────────────────────────── │  response flushed
  │                                         │  keep-alive: goroutine waits for next request
  │  (idle longer than IdleTimeout)         │  connection closed
```

## 2. Code example

The whole lifecycle is configured in one place, `server.New`:

```go
srv := &http.Server{
    Addr:              cfg.Addr,
    Handler:           root,
    ReadHeaderTimeout: cfg.ReadHeaderTimeout, // slow-header attacks
    ReadTimeout:       cfg.ReadTimeout,       // total time to read the request
    WriteTimeout:      cfg.WriteTimeout,      // total time to write the response
    IdleTimeout:       cfg.IdleTimeout,       // keep-alive lifetime
    MaxHeaderBytes:    cfg.MaxHeaderBytes,
    BaseContext:       func(net.Listener) context.Context { return baseCtx },
    ConnState:         m.ConnState,           // observe connection transitions
    ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
}
```

What each timeout does, and what it costs:

| Setting                  | Protects against                                                                                                                                               | Cost of too small                                                        | Cost of too large                                                                                                                                       |
| ------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ReadHeaderTimeout` (5s) | A client that opens a connection and drips headers to hold a goroutine and descriptor (slowloris).                                                             | Clients on very slow links fail before sending headers.                  | Attackers can pin resources longer.                                                                                                                     |
| `ReadTimeout` (10s)      | A client that sends the headers and then stalls on the body. Covers headers plus body.                                                                         | Legitimate large uploads are cut off.                                    | Stalled uploads hold resources.                                                                                                                         |
| `WriteTimeout` (15s)     | A client that stops reading the response, leaving the handler's writes blocked. Its clock starts when the headers are read, so it also bounds handler runtime. | Slow handlers or slow readers get a dropped connection with no response. | Blocked writers pin resources.                                                                                                                          |
| `IdleTimeout` (60s)      | Idle keep-alive connections accumulating.                                                                                                                      | More reconnects and handshakes (an issue with TLS).                      | Many idle descriptors held. Must be longer than any load balancer's idle timeout or the balancer will send onto connections the server has just closed. |
| `MaxHeaderBytes` (1 MiB) | Memory spent on enormous headers.                                                                                                                              | Legitimate large cookies or tokens are rejected with 431.                | More memory per connection.                                                                                                                             |

`MaxHeaderBytes` is approximate, not exact. `net/http` adds about 4 KiB of read
buffer slop, and on a reused keep-alive connection up to about 4 KiB of the
next request may already be buffered before the limit applies. We measured this:
with a limit of 1024, an 8192-byte header was rejected on a fresh connection
and accepted on a reused one. Treat it as a coarse guard, not a precise quota.

`WriteTimeout` interacts with the request deadline. `HTTP_REQUEST_TIMEOUT`
(the cooperative deadline on the request context) is validated to be shorter
than `HTTP_WRITE_TIMEOUT`, so a `504` can be written before the connection's
write deadline fires. If it were longer, the deadline would fire first and the
client would see a dropped connection instead of a response.

## 3. Production implications

- Every setting above is required. `net/http` defaults are "no timeout", which
  is unsafe for an internet-facing server. `config.Validate` rejects zero.
- Errors produced by `net/http` _before_ a handler runs (oversized headers,
  malformed request line) are written by the standard library in plain text.
  Only responses produced by our code use the JSON error envelope. The
  integration test `TestRequestHardening` asserts the 431.
- `ConnState` lets us count connections separately from requests
  (`active_connections` vs `requests_in_flight`). A high connection count with
  few in-flight requests means idle keep-alives; the reverse means fan-in on few
  connections (HTTP/2 or a proxy).
- The response is not "sent" when the handler calls `Write`. It is buffered and
  flushed when the buffer fills or the handler returns. That is why the access
  log's duration measures handler time, not client-perceived time.

## 4. Common mistake

Assuming the request context is scoped to the handler's lifetime only.
`r.Context()` is cancelled when the client's connection closes, when
`ServeHTTP` returns, or (for HTTP/2) when the client cancels the stream. A
goroutine started in the handler and using `r.Context()` after the handler
returns sees a cancelled context. This is why asynchronous jobs in this repo run
under the worker pool's own context, never the request's.

## 5. What happens under load

- Connections beyond what the process can accept wait in the kernel's accept
  backlog; beyond that, clients see connection failures. `net/http` has no
  built-in cap on concurrent connections. Limiting them is a job for the load
  balancer or a wrapped listener (for example `LimitListener` from
  `golang.org/x/net/netutil`, which would be this repo's first dependency); this repo does not add one, which is a
  documented gap (see [operational-guide.md](operational-guide.md)).
- Each connection costs a goroutine (a few KiB of stack at minimum), buffers for
  reading and writing, and a descriptor. The descriptor limit (`ulimit -n`) is
  usually hit before memory.
- A burst of slow clients is bounded by the timeouts: no connection can hold a
  goroutine longer than the applicable timeout plus handler time.
