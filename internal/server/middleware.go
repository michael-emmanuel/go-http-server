package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/example/go-production-http-server/internal/httpapi"
	"github.com/example/go-production-http-server/internal/metrics"
	"github.com/example/go-production-http-server/internal/requestid"
)

// Middleware wraps a handler with additional behavior.
//
// Middleware is function composition: a Middleware is a function from Handler
// to Handler, so stacking them is just nested function application,
//
//	RequestID(AccessLog(Recovery(mux)))
//
// The outermost function sees the request first and the response last. Chain
// writes that nesting in reading order.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first argument is the outermost layer:
// Chain(h, A, B, C) is A(B(C(h))). Iterating in reverse builds the nesting
// from the inside out.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// responseRecorder captures what a handler wrote so outer middleware can log
// the status and size after the fact. http.ResponseWriter offers no way to ask
// "what status did you send?", so wrapping is the only option.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (rr *responseRecorder) WriteHeader(code int) {
	// 1xx informational responses (for example 103 Early Hints) may precede
	// the final status and must not be mistaken for it.
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		rr.ResponseWriter.WriteHeader(code)
		return
	}
	if rr.wroteHeader {
		return
	}
	rr.status = code
	rr.wroteHeader = true
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	if !rr.wroteHeader {
		rr.WriteHeader(http.StatusOK)
	}
	n, err := rr.ResponseWriter.Write(b)
	rr.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer for Flush,
// deadlines, and hijacking, which a plain wrapper would otherwise hide.
func (rr *responseRecorder) Unwrap() http.ResponseWriter { return rr.ResponseWriter }

// recorderFor returns w as a recorder, wrapping it only if an outer middleware
// has not already done so. Sharing one recorder means each layer sees the same
// status without stacking wrappers.
func recorderFor(w http.ResponseWriter) *responseRecorder {
	if rr, ok := w.(*responseRecorder); ok {
		return rr
	}
	return &responseRecorder{ResponseWriter: w, status: http.StatusOK}
}

// RequestID attaches a request ID to the context and the response.
//
// It must be the outermost middleware: every later layer (logging, recovery,
// handlers) reads the ID from the context, so it has to exist before any of
// them run. A valid inbound ID is honored so a caller (or an upstream proxy)
// can correlate across services; anything else is replaced, because echoing
// arbitrary client input into logs and headers invites injection.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(requestid.Header)
			if !requestid.Valid(id) {
				id = requestid.New()
			}
			w.Header().Set(requestid.Header, id)
			next.ServeHTTP(w, r.WithContext(requestid.WithContext(r.Context(), id)))
		})
	}
}

// AccessLog writes one structured line per request after it completes.
//
// The log call is deferred so a request that ends in a panic that Recovery
// deliberately re-raises (http.ErrAbortHandler) is still logged. It records
// the path but never the query string or headers, which routinely carry
// tokens and personal data.
func AccessLog(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rr := recorderFor(w)
			start := time.Now()
			defer func() {
				level := slog.LevelInfo
				switch {
				case rr.status >= 500:
					level = slog.LevelError
				case rr.status >= 400:
					level = slog.LevelWarn
				case strings.HasPrefix(r.URL.Path, "/health/"):
					// Probes fire every few seconds per instance; at Info they
					// would drown out real traffic.
					level = slog.LevelDebug
				}
				log.LogAttrs(r.Context(), level, "request completed",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", rr.status),
					slog.Int64("bytes", rr.bytes),
					slog.Float64("duration_ms", float64(time.Since(start))/float64(time.Millisecond)),
					slog.String("remote_addr", r.RemoteAddr),
				)
			}()
			next.ServeHTTP(rr, r)
		})
	}
}

// Metrics records in-flight count, request count, status class, and latency.
// This is also the "request timing" middleware: the duration observed here
// feeds the latency histogram.
func Metrics(m *metrics.Metrics) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rr := recorderFor(w)
			m.IncInFlight()
			start := time.Now()
			defer func() {
				m.DecInFlight()
				m.Observe(rr.status, time.Since(start))
			}()
			next.ServeHTTP(rr, r)
		})
	}
}

// Recovery converts a handler panic into a 500 response.
//
// net/http already recovers panics per connection, but its behavior is to log
// and drop the connection, which the client sees as a network error with no
// request ID and which our access log and metrics never record. Recovering
// here keeps the connection usable, returns the standard error envelope, and
// makes the failure visible to the layers above.
//
// Position matters. Recovery sits inside AccessLog and Metrics so that when it
// writes the 500, those layers observe it. Placed outside them, they would
// finish with the default 200 before Recovery ran. It can only catch panics in
// layers below it, so it should be as far out as possible without
// hiding its result from the observers.
func Recovery(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rr := recorderFor(w)
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// ErrAbortHandler is the sanctioned way for a handler to abort
				// a response on purpose. It must keep propagating.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				log.ErrorContext(r.Context(), "panic recovered",
					"panic", fmt.Sprint(rec),
					"stack", string(debug.Stack()),
				)
				if rr.wroteHeader {
					// The status line is already on the wire, so a 500 is
					// impossible. Abort the connection so the client sees a
					// truncated response, not a silently corrupt "success".
					panic(http.ErrAbortHandler)
				}
				httpapi.WriteError(rr, r, http.StatusInternalServerError, "internal_error", "internal server error")
			}()
			next.ServeHTTP(rr, r)
		})
	}
}

// Auth is a PLACEHOLDER bearer-token check, not production authentication.
//
// It compares one static shared secret in constant time. It has no user
// identity, no expiry, no rotation, no revocation, and no rate limiting on
// failures. Replace it with a real mechanism (mTLS, OIDC/JWT validation,
// or an authenticating gateway) before exposing anything sensitive. An empty
// token disables the check entirely, which is only appropriate for local use.
func Auth(token string) Middleware {
	if token == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	want := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			// ConstantTimeCompare avoids leaking, through response time, how
			// many leading bytes of a guess were correct. It returns early on
			// a length mismatch, which reveals only the length.
			if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
				httpapi.WriteError(w, r, http.StatusUnauthorized, "unauthorized", "missing or invalid credentials")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireJSON rejects requests with a body-carrying method and a non-JSON
// Content-Type. Failing early with 415 gives a precise error instead of a
// confusing decode failure inside the handler.
func RequireJSON() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch:
				mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if err != nil || mt != "application/json" {
					httpapi.WriteError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequestTimeout attaches a deadline to the request context.
//
// This is a cooperative timeout: it cancels the context, and code that honors
// the context stops. It cannot forcibly stop a goroutine that ignores it. The
// server-level WriteTimeout is the non-cooperative backstop that eventually
// closes the connection regardless.
func RequestTimeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel() // release the timer as soon as the handler returns
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
