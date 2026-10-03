package httpapi

import (
	"net/http"

	"github.com/example/go-production-http-server/internal/health"
)

// RouteDeps are the collaborators needed to build the route table.
type RouteDeps struct {
	Handler *Handler
	Health  *health.Checker
	Metrics http.Handler
	// API wraps every /api/ route. Cross-cutting policy that applies to the
	// API but not to probes (authentication, content-type enforcement, request
	// deadlines) is injected here so this package does not import middleware.
	// nil means no wrapping.
	API func(http.Handler) http.Handler
}

// Routes builds the route table.
//
// Routing lives apart from the handlers for two reasons. First, the table is
// the one place that answers "which URL runs what, under which policy";
// scattering that across handler methods makes it unreviewable. Second, it
// lets tests exercise handlers directly without any routing, and exercise
// routing without caring what handlers do.
//
// It relies on Go 1.22 ServeMux patterns: an optional method prefix and {name}
// wildcards read with Request.PathValue.
func Routes(d RouteDeps) http.Handler {
	api := d.API
	if api == nil {
		api = func(h http.Handler) http.Handler { return h }
	}

	mux := http.NewServeMux()

	// Operational endpoints: unauthenticated by design, because orchestrators
	// and load balancers must be able to probe them. Restrict them at the
	// network layer, not here.
	mux.Handle("GET /health/live", d.Health.LiveHandler())
	mux.Handle("GET /health/ready", d.Health.ReadyHandler())
	mux.Handle("GET /metrics", d.Metrics)

	mux.Handle("GET /api/v1/info", api(http.HandlerFunc(d.Handler.Info)))
	mux.Handle("GET /api/v1/work", api(http.HandlerFunc(d.Handler.RunWork)))
	mux.Handle("POST /api/v1/work", api(http.HandlerFunc(d.Handler.SubmitWork)))
	mux.Handle("GET /api/v1/work/{id}", api(http.HandlerFunc(d.Handler.GetWork)))

	return &router{mux: mux}
}

// router wraps ServeMux so that its built-in 404 and 405 responses, which are
// plain text, use the same JSON error envelope as everything else.
type router struct {
	mux *http.ServeMux
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h, pattern := rt.mux.Handler(r)
	if pattern != "" {
		// Serve through the mux, not through h: only ServeMux.ServeHTTP
		// populates wildcard values for Request.PathValue. Handler() merely
		// looks the match up.
		rt.mux.ServeHTTP(w, r)
		return
	}

	// No pattern matched. ServeMux returns a handler that writes 404 or 405
	// (with an Allow header). Run it against a probe writer to learn which,
	// then answer in our own format.
	probe := &probeWriter{header: make(http.Header), status: http.StatusOK}
	h.ServeHTTP(probe, r)

	switch probe.status {
	case http.StatusNotFound:
		WriteError(w, r, http.StatusNotFound, "not_found", "no such endpoint")
	case http.StatusMethodNotAllowed:
		if allow := probe.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this endpoint")
	default:
		// Something else, such as a redirect to a cleaned path. Let the mux
		// answer for real.
		rt.mux.ServeHTTP(w, r)
	}
}

// probeWriter records the status and headers a handler would send and
// discards the body.
type probeWriter struct {
	header http.Header
	status int
	wrote  bool
}

func (p *probeWriter) Header() http.Header { return p.header }

func (p *probeWriter) WriteHeader(status int) {
	if !p.wrote {
		p.status = status
		p.wrote = true
	}
}

func (p *probeWriter) Write(b []byte) (int, error) {
	p.WriteHeader(http.StatusOK)
	return len(b), nil
}
