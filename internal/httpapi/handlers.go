// Package httpapi is the transport layer: it translates HTTP requests into
// calls on the application layer and translates results and errors back into
// HTTP responses.
//
// Three kinds of concern are kept apart on purpose:
//
//	transport:      methods, paths, headers, JSON, status codes  (this package)
//	application:    what a job is, limits, execution rules       (package domain)
//	infrastructure: goroutine pools, timeouts, sockets, signals  (worker, server)
//
// A handler here parses and validates the wire format, calls the application
// layer with the request context, and maps the outcome to a response. It
// contains no business rules and never reaches for globals.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"time"

	"github.com/example/go-production-http-server/internal/domain"
)

// WorkService is the slice of application behavior the handlers need. It is
// declared here, by the consumer, so handlers can be tested with a fake and so
// this package depends on an interface it owns rather than on domain.Service.
type WorkService interface {
	RunBatch(ctx context.Context, req domain.BatchRequest) (domain.BatchResult, error)
	SubmitJob(ctx context.Context, req domain.BatchRequest) (domain.Job, error)
	GetJob(ctx context.Context, id string) (domain.Job, error)
}

// Deps are the collaborators of a Handler.
type Deps struct {
	Work         WorkService
	Logger       *slog.Logger
	MaxBodyBytes int64
	Version      string
	Now          func() time.Time // default: time.Now
}

// Handler holds the dependencies shared by all endpoint methods. Its fields are
// immutable after construction, so one Handler is safely used by every
// concurrent request without locking.
type Handler struct {
	work    WorkService
	log     *slog.Logger
	maxBody int64
	version string
	started time.Time
	now     func() time.Time
}

// NewHandler validates deps and returns a Handler.
func NewHandler(d Deps) (*Handler, error) {
	if d.Work == nil {
		return nil, errors.New("httpapi: Work is required")
	}
	if d.Logger == nil {
		return nil, errors.New("httpapi: Logger is required")
	}
	if d.MaxBodyBytes < 1 {
		return nil, errors.New("httpapi: MaxBodyBytes must be positive")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	version := d.Version
	if version == "" {
		version = "dev"
	}
	return &Handler{
		work:    d.Work,
		log:     d.Logger,
		maxBody: d.MaxBodyBytes,
		version: version,
		started: now(),
		now:     now,
	}, nil
}

const (
	defaultItems   = 10
	defaultDelayMS = 10
)

type infoResponse struct {
	Service       string    `json:"service"`
	Version       string    `json:"version"`
	GoVersion     string    `json:"go_version"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds float64   `json:"uptime_seconds"`
	Goroutines    int       `json:"goroutines"`
	GOMAXPROCS    int       `json:"gomaxprocs"`
}

// Info handles GET /api/v1/info.
func (h *Handler) Info(w http.ResponseWriter, r *http.Request) {
	h.respond(w, r, http.StatusOK, infoResponse{
		Service:       "go-production-http-server",
		Version:       h.version,
		GoVersion:     runtime.Version(),
		StartedAt:     h.started.UTC(),
		UptimeSeconds: h.now().Sub(h.started).Seconds(),
		Goroutines:    runtime.NumGoroutine(),
		GOMAXPROCS:    runtime.GOMAXPROCS(0),
	})
}

type batchResponse struct {
	Items       int     `json:"items"`
	Checksum    int64   `json:"checksum"`
	Concurrency int     `json:"concurrency"`
	ElapsedMS   float64 `json:"elapsed_ms"`
}

// RunWork handles GET /api/v1/work?items=N&delay_ms=M by running the batch
// synchronously under the request context.
func (h *Handler) RunWork(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, err := intParam(q, "items", defaultItems)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	delayMS, err := intParam(q, "delay_ms", defaultDelayMS)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	// r.Context() is cancelled when the client disconnects, the request
	// deadline (set by middleware) expires, or the server force-closes. Passing
	// it down, rather than context.Background(), is what makes that
	// cancellation reach the goroutines doing the work.
	delay, err := millisToDuration("delay_ms", delayMS)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.work.RunBatch(r.Context(), domain.BatchRequest{Items: items, ItemDelay: delay})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.respond(w, r, http.StatusOK, batchResponse{
		Items:       res.Items,
		Checksum:    res.Checksum,
		Concurrency: res.Concurrency,
		ElapsedMS:   float64(res.Elapsed) / float64(time.Millisecond),
	})
}

type submitRequest struct {
	Items   int `json:"items"`
	DelayMS int `json:"delay_ms"`
}

type resultResponse struct {
	Items       int     `json:"items"`
	Checksum    int64   `json:"checksum"`
	Concurrency int     `json:"concurrency"`
	ElapsedMS   float64 `json:"elapsed_ms"`
}

type jobResponse struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	Items      int             `json:"items"`
	DelayMS    int64           `json:"delay_ms"`
	CreatedAt  time.Time       `json:"created_at"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Result     *resultResponse `json:"result,omitempty"`
	Failure    string          `json:"failure,omitempty"`
}

func toJobResponse(j domain.Job) jobResponse {
	resp := jobResponse{
		ID:        j.ID,
		Status:    string(j.Status),
		Items:     j.Request.Items,
		DelayMS:   j.Request.ItemDelay.Milliseconds(),
		CreatedAt: j.CreatedAt.UTC(),
		Failure:   j.Failure,
	}
	if !j.StartedAt.IsZero() {
		t := j.StartedAt.UTC()
		resp.StartedAt = &t
	}
	if !j.FinishedAt.IsZero() {
		t := j.FinishedAt.UTC()
		resp.FinishedAt = &t
	}
	if j.Result != nil {
		resp.Result = &resultResponse{
			Items:       j.Result.Items,
			Checksum:    j.Result.Checksum,
			Concurrency: j.Result.Concurrency,
			ElapsedMS:   float64(j.Result.Elapsed) / float64(time.Millisecond),
		}
	}
	return resp
}

// SubmitWork handles POST /api/v1/work. It accepts the job and answers 202;
// the work happens later on the worker pool.
func (h *Handler) SubmitWork(w http.ResponseWriter, r *http.Request) {
	req := submitRequest{Items: defaultItems, DelayMS: defaultDelayMS}
	if err := decodeJSON(w, r, h.maxBody, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	delay, err := millisToDuration("delay_ms", req.DelayMS)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	job, err := h.work.SubmitJob(r.Context(), domain.BatchRequest{Items: req.Items, ItemDelay: delay})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/work/"+job.ID)
	h.respond(w, r, http.StatusAccepted, toJobResponse(job))
}

// GetWork handles GET /api/v1/work/{id}.
func (h *Handler) GetWork(w http.ResponseWriter, r *http.Request) {
	job, err := h.work.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.respond(w, r, http.StatusOK, toJobResponse(job))
}

// intParam parses an optional integer query parameter. Parsing is a transport
// concern (the wire format is text); whether the value is acceptable is an
// application concern decided by the domain.
func intParam(q url.Values, key string, def int) (int, error) {
	raw := q.Get(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, &domain.ValidationError{Field: key, Reason: "must be an integer"}
	}
	return n, nil
}

// millisToDuration converts a wire-format millisecond count to a Duration.
//
// The conversion must happen with an overflow check, before the domain's range
// validation runs: time.Duration is int64 nanoseconds, so a huge value
// multiplied by time.Millisecond silently wraps around and can land inside the
// "valid" range. Validating after a wrapped conversion would validate the wrong
// number.
func millisToDuration(field string, ms int) (time.Duration, error) {
	const maxMS = math.MaxInt64 / int64(time.Millisecond)
	if ms < 0 || int64(ms) > maxMS {
		return 0, &domain.ValidationError{Field: field, Reason: "is out of range"}
	}
	return time.Duration(ms) * time.Millisecond, nil
}
