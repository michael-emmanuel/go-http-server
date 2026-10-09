package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/michael-emmanuel/go-production-http-server/internal/domain"
	"github.com/michael-emmanuel/go-production-http-server/internal/requestid"
)

// StatusClientClosedRequest is the de-facto status (popularized by nginx) for
// "the client went away before we answered". It is not an IANA-registered
// code. It is recorded in logs and metrics so client disconnects are
// distinguishable from server faults; the client, having gone, never sees it.
const StatusClientClosedRequest = 499

// errorBody is the single error envelope used by every endpoint, including
// errors produced by middleware and the router.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// apiError is a client-facing failure with an explicit status and code. It is
// how transport-level problems (bad JSON, oversized body) travel up to the
// single place that writes error responses.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.code + ": " + e.message }

// WriteError writes the standard error envelope. It is exported because
// middleware in other packages must produce byte-identical error responses.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	body, err := json.Marshal(errorBody{Error: errorDetail{
		Code:      code,
		Message:   message,
		RequestID: requestid.FromContext(r.Context()),
	}})
	if err != nil {
		// Unreachable for this fixed struct, but never fall through silently.
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	// A failed write means the client is gone; the access log still records
	// the status, and there is nothing further to do.
	_ = writeRaw(w, status, body)
}

func writeRaw(w http.ResponseWriter, status int, body []byte) error {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	// nosniff stops browsers from second-guessing the declared type.
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, err := w.Write(append(body, '\n'))
	return err
}

// respond marshals v before touching the ResponseWriter so that an encoding
// failure can still produce a clean 500 instead of a half-written 200.
func (h *Handler) respond(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := writeRaw(w, status, body); err != nil {
		h.log.DebugContext(r.Context(), "write response", "error", err)
	}
}

// failure is the classified, client-safe view of an error.
type failure struct {
	status     int
	code       string
	message    string
	level      slog.Level
	retryAfter string
}

// classify maps an internal error to a client-safe failure. It is the single
// place where the taxonomy of client errors, server errors, cancellation,
// timeouts, and dependency failures is decided.
//
// Order matters: context.DeadlineExceeded also satisfies net.Error with
// Timeout() == true, so it must be tested before the generic timeout case.
func classify(err error) failure {
	var (
		apiErr *apiError
		valErr *domain.ValidationError
		netErr net.Error
	)
	switch {
	case errors.As(err, &apiErr):
		return failure{status: apiErr.status, code: apiErr.code, message: apiErr.message, level: slog.LevelDebug}
	case errors.As(err, &valErr):
		return failure{status: http.StatusBadRequest, code: "invalid_request", message: "invalid parameter: " + valErr.Error(), level: slog.LevelDebug}
	case errors.Is(err, domain.ErrNotFound):
		return failure{status: http.StatusNotFound, code: "not_found", message: "resource not found", level: slog.LevelDebug}
	case errors.Is(err, domain.ErrOverloaded):
		return failure{status: http.StatusServiceUnavailable, code: "overloaded", message: "server is at capacity, retry later", level: slog.LevelWarn, retryAfter: "1"}
	case errors.Is(err, domain.ErrUnavailable):
		return failure{status: http.StatusServiceUnavailable, code: "unavailable", message: "server is shutting down", level: slog.LevelInfo, retryAfter: "1"}
	case errors.Is(err, context.Canceled):
		return failure{status: StatusClientClosedRequest, code: "client_closed_request", message: "request canceled", level: slog.LevelInfo}
	case errors.Is(err, context.DeadlineExceeded):
		return failure{status: http.StatusGatewayTimeout, code: "timeout", message: "request deadline exceeded", level: slog.LevelWarn}
	case errors.As(err, &netErr) && netErr.Timeout():
		return failure{status: http.StatusRequestTimeout, code: "request_timeout", message: "timed out reading request", level: slog.LevelInfo}
	default:
		return failure{status: http.StatusInternalServerError, code: "internal_error", message: "internal server error", level: slog.LevelError}
	}
}

// fail logs err with full internal detail and writes only the classified,
// generic version to the client. The request ID in both places is what lets an
// operator connect a client-visible failure to the internal cause.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	// A cancelled context means one of two very different things: the client
	// went away (nobody is listening; 499 for our logs) or the server is
	// shutting down and cancelled us on purpose (the client is waiting and
	// should retry elsewhere; 503). The cancellation cause tells them apart.
	if errors.Is(err, context.Canceled) && errors.Is(context.Cause(r.Context()), domain.ErrUnavailable) {
		err = fmt.Errorf("%w: %w", domain.ErrUnavailable, err)
	}
	f := classify(err)
	h.log.Log(r.Context(), f.level, "request failed", "error", err, "status", f.status, "code", f.code)
	if f.retryAfter != "" {
		w.Header().Set("Retry-After", f.retryAfter)
	}
	WriteError(w, r, f.status, f.code, f.message)
}

// decodeJSON reads exactly one JSON value from the request body into dst.
//
// Hardening applied here:
//   - MaxBytesReader caps memory a client can make us buffer and returns
//     *http.MaxBytesError once the limit is exceeded. (Its extra behavior of
//     asking the server to close the connection depends on the ResponseWriter
//     implementing an unexported net/http interface, which our recording
//     wrapper does not; net/http then discards up to 256 KiB of the unread
//     body before reusing the connection, or closes it.)
//   - DisallowUnknownFields turns client typos into errors instead of
//     silently ignored parameters.
//   - The trailing-token check rejects "{}{}" and "{} garbage".
func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				return decodeError(err)
			}
		}
		return &apiError{http.StatusBadRequest, "invalid_request", "request body must contain a single JSON object"}
	}
	return nil
}

func decodeError(err error) error {
	var (
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
		maxErr    *http.MaxBytesError
	)
	switch {
	case errors.As(err, &maxErr):
		return &apiError{http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large"}
	case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF):
		return &apiError{http.StatusBadRequest, "malformed_json", "request body is not valid JSON"}
	case errors.As(err, &typeErr):
		return &apiError{http.StatusBadRequest, "invalid_request", "field \"" + typeErr.Field + "\" has the wrong type"}
	case errors.Is(err, io.EOF):
		return &apiError{http.StatusBadRequest, "invalid_request", "request body must not be empty"}
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		// encoding/json reports unknown fields with a plain error, so the
		// message prefix is the only signal available.
		return &apiError{http.StatusBadRequest, "invalid_request", "request body contains an unknown field"}
	default:
		// Not a client-format problem (for example a read timeout or a
		// dropped connection); let classify decide from the underlying error.
		return err
	}
}
