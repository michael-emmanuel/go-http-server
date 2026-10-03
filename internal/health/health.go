// Package health implements liveness and readiness reporting.
//
// The two probes answer different questions and must not be conflated:
//
//	liveness:  "Is this process alive?"  A failure means restart me.
//	readiness: "Should I receive traffic?" A failure means stop routing to me.
//
// Liveness deliberately checks nothing beyond "the HTTP stack can respond".
// If liveness depended on a database, a database outage would make an
// orchestrator restart every healthy instance, turning a partial outage into
// a total one.
package health

import (
	"net/http"
	"sync"
	"sync/atomic"
)

// State is the readiness state of the process.
type State int32

const (
	// StateStarting means the server has not yet begun accepting traffic.
	StateStarting State = iota
	// StateReady means the instance should receive traffic.
	StateReady
	// StateDraining means shutdown has begun. It is terminal: an instance that
	// has started draining never becomes ready again.
	StateDraining
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateDraining:
		return "draining"
	default:
		return "unknown"
	}
}

// Checker holds the readiness state. It is read by every probe request and
// written by the server lifecycle, from different goroutines, hence atomics.
type Checker struct {
	state     atomic.Int32
	ready     chan struct{}
	draining  chan struct{}
	drainOnce sync.Once
}

// New returns a Checker in StateStarting.
func New() *Checker {
	return &Checker{ready: make(chan struct{}), draining: make(chan struct{})}
}

// MarkReady moves starting -> ready. It reports whether the transition
// happened; it never resurrects a draining instance.
func (c *Checker) MarkReady() bool {
	if !c.state.CompareAndSwap(int32(StateStarting), int32(StateReady)) {
		return false
	}
	// Only the goroutine that wins the CAS reaches here, so the channel is
	// closed exactly once without needing a sync.Once.
	close(c.ready)
	return true
}

// MarkDraining moves to the terminal draining state and closes the channel
// returned by Draining. It is safe to call more than once.
func (c *Checker) MarkDraining() {
	c.state.Store(int32(StateDraining))
	c.drainOnce.Do(func() { close(c.draining) })
}

// State returns the current state.
func (c *Checker) State() State { return State(c.state.Load()) }

// Ready returns a channel that is closed when the checker first becomes ready.
func (c *Checker) Ready() <-chan struct{} { return c.ready }

// Draining returns a channel that is closed when draining begins. Long-lived
// handlers can select on it to wind down early; tests use it to coordinate
// without sleeping.
func (c *Checker) Draining() <-chan struct{} { return c.draining }

var (
	bodyLive     = []byte("{\"status\":\"ok\"}\n")
	bodyReady    = []byte("{\"status\":\"ready\"}\n")
	bodyStarting = []byte("{\"status\":\"starting\"}\n")
	bodyDraining = []byte("{\"status\":\"draining\"}\n")
)

// LiveHandler always reports 200 while the process can serve HTTP at all.
func (c *Checker) LiveHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, bodyLive)
	})
}

// ReadyHandler reports 200 only in StateReady and 503 otherwise, so a load
// balancer stops sending new requests while in-flight ones finish.
func (c *Checker) ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch c.State() {
		case StateReady:
			write(w, http.StatusOK, bodyReady)
		case StateDraining:
			write(w, http.StatusServiceUnavailable, bodyDraining)
		default:
			write(w, http.StatusServiceUnavailable, bodyStarting)
		}
	})
}

func write(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// A failed write means the prober disconnected; there is nothing to do.
	_, _ = w.Write(body)
}
