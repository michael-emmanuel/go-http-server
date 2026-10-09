// Package logging builds the process-wide structured logger.
package logging

import (
	"context"
	"io"
	"log/slog"

	"github.com/michael-emmanuel/go-production-http-server/internal/requestid"
)

// New returns a JSON logger that automatically attaches the request ID found
// in the context passed to the *Context logging methods.
//
// Call sites must use log.InfoContext(ctx, ...) rather than log.Info(...) for
// the request ID to appear: slog only sees the context if it is handed one.
func New(w io.Writer, level slog.Level) *slog.Logger {
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(contextHandler{Handler: base})
}

// contextHandler decorates a slog.Handler with request-scoped attributes.
// Keeping this in the handler (instead of at every call site) means a
// developer cannot forget to include the request ID.
type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := requestid.FromContext(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{Handler: h.Handler.WithGroup(name)}
}
