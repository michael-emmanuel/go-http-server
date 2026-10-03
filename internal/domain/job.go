// Package domain contains the application logic: what a "work batch" and a
// "work job" are, the rules for accepting them, and how they are executed.
//
// It knows nothing about HTTP. Status codes, JSON, and headers are transport
// concerns and live in package httpapi. Goroutines, queues, and the process
// lifecycle are infrastructure concerns; the domain consumes them through the
// small Executor interface.
package domain

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Sentinel errors. Callers match them with errors.Is, which works through any
// number of fmt.Errorf("...: %w", err) wraps.
var (
	// ErrNotFound means the requested job does not exist (or was evicted).
	ErrNotFound = errors.New("not found")
	// ErrOverloaded means the system is at capacity and the caller should
	// retry later.
	ErrOverloaded = errors.New("overloaded")
	// ErrUnavailable means the system is shutting down and cannot accept work.
	ErrUnavailable = errors.New("unavailable")
)

// ValidationError describes invalid caller input. It is a struct type so
// callers extract it with errors.As and can read the offending field.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return e.Field + " " + e.Reason }

// JobStatus is the lifecycle state of an asynchronous job.
type JobStatus string

const (
	StatusQueued    JobStatus = "queued"
	StatusRunning   JobStatus = "running"
	StatusSucceeded JobStatus = "succeeded"
	StatusFailed    JobStatus = "failed"
	StatusCanceled  JobStatus = "canceled"
)

// Terminal reports whether the job will never change state again.
func (s JobStatus) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCanceled
}

// Coarse failure reasons stored on a job. Internal error text is logged, never
// stored where an API client could read it.
const (
	FailureCanceled = "canceled"
	FailureInternal = "internal_error"
)

// BatchRequest describes a batch of independent simulated work items.
type BatchRequest struct {
	Items     int
	ItemDelay time.Duration
}

// BatchResult is the outcome of a completed batch.
type BatchResult struct {
	Items int
	// Checksum is the sum of the per-item results. Because every item has a
	// known result, tests can verify that concurrent execution neither lost
	// nor duplicated work.
	Checksum    int64
	Concurrency int
	Elapsed     time.Duration
}

// Job is an asynchronous batch and its state. Job values are copied in and out
// of the Store; Result is replaced wholesale and never mutated in place, so a
// copy never aliases mutable state.
type Job struct {
	ID         string
	Request    BatchRequest
	Status     JobStatus
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	Result     *BatchResult
	Failure    string
}

// Store is an in-memory, bounded job store safe for concurrent use.
//
// It is unexported-state plus a mutex rather than a sync.Map because updates
// are read-modify-write on a struct, which needs one critical section; the
// mutex also protects the eviction order slice.
type Store struct {
	mu    sync.RWMutex
	jobs  map[string]Job
	order []string // insertion order, oldest first
	max   int
}

// NewStore returns a Store that retains at most max jobs.
func NewStore(max int) *Store {
	return &Store{jobs: make(map[string]Job), max: max}
}

// Create inserts j. When the store is full it evicts the oldest terminal job;
// if every retained job is still active it refuses with ErrOverloaded rather
// than dropping work that is in progress.
func (s *Store) Create(j Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.jobs) >= s.max && !s.evictOldestTerminalLocked() {
		return fmt.Errorf("job store full (%d active jobs): %w", len(s.jobs), ErrOverloaded)
	}
	s.jobs[j.ID] = j
	s.order = append(s.order, j.ID)
	return nil
}

// Get returns a copy of the job.
func (s *Store) Get(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	return j, ok
}

// Update applies fn to a copy of the job and stores the result atomically with
// respect to other store operations. It reports whether the job existed.
func (s *Store) Update(id string, fn func(*Job)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return false
	}
	fn(&j)
	s.jobs[id] = j
	return true
}

// Delete removes a job. It is used to roll back a job that could not be queued.
func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; !ok {
		return
	}
	delete(s.jobs, id)
	for i, oid := range s.order {
		if oid == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Len returns the number of retained jobs.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.jobs)
}

// evictOldestTerminalLocked removes the oldest finished job. The caller must
// hold s.mu for writing.
func (s *Store) evictOldestTerminalLocked() bool {
	for i, id := range s.order {
		if s.jobs[id].Status.Terminal() {
			delete(s.jobs, id)
			s.order = append(s.order[:i], s.order[i+1:]...)
			return true
		}
	}
	return false
}
