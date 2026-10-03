// Package requestid generates request identifiers and carries them in a
// context.Context.
//
// A request ID exists so that one log line from the access log, one from a
// downstream error, and one from a client bug report can be tied together.
package requestid

import (
	"context"
	"fmt"
	"math/rand/v2"
)

// Header is the HTTP header used to accept and echo request IDs.
const Header = "X-Request-ID"

const maxLen = 64

// ctxKey is unexported so no other package can collide with or overwrite our
// context value.
type ctxKey struct{}

// New returns a 128-bit random identifier in hex.
//
// math/rand/v2's top-level functions are safe for concurrent use and seeded
// from the OS, and unlike crypto/rand they cannot fail. Request IDs need
// uniqueness, not unpredictability, so this is sufficient.
func New() string {
	return fmt.Sprintf("%016x%016x", rand.Uint64(), rand.Uint64())
}

// Valid reports whether id is safe to accept from a client and echo into logs
// and response headers. Accepting arbitrary client input here would allow log
// injection and header abuse.
func Valid(id string) bool {
	if id == "" || len(id) > maxLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// WithContext returns a copy of ctx carrying id.
func WithContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the request ID stored in ctx, or "" if there is none.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}
