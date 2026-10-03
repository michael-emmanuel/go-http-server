package domain

import (
	"errors"
	"sync"
	"testing"
)

func TestStoreEvictsOldestTerminalJobWhenFull(t *testing.T) {
	s := NewStore(2)
	mustCreate := func(id string, st JobStatus) {
		t.Helper()
		if err := s.Create(Job{ID: id, Status: st}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mustCreate("a", StatusSucceeded)
	mustCreate("b", StatusQueued)
	mustCreate("c", StatusQueued) // evicts a, the oldest terminal job

	if _, ok := s.Get("a"); ok {
		t.Error("a should have been evicted")
	}
	if _, ok := s.Get("b"); !ok {
		t.Error("b (active) must be kept")
	}
	if s.Len() != 2 {
		t.Errorf("Len = %d, want 2", s.Len())
	}
}

func TestStoreRefusesRatherThanEvictActiveJobs(t *testing.T) {
	s := NewStore(1)
	if err := s.Create(Job{ID: "a", Status: StatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(Job{ID: "b"}); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("err = %v, want ErrOverloaded", err)
	}
	if _, ok := s.Get("a"); !ok {
		t.Fatal("active job must not be dropped")
	}
}

func TestStoreUpdateAndDelete(t *testing.T) {
	s := NewStore(3)
	if s.Update("missing", func(*Job) {}) {
		t.Error("Update of a missing job must report false")
	}
	if err := s.Create(Job{ID: "a", Status: StatusQueued}); err != nil {
		t.Fatal(err)
	}
	if !s.Update("a", func(j *Job) { j.Status = StatusSucceeded }) {
		t.Fatal("Update should find job a")
	}
	if j, _ := s.Get("a"); j.Status != StatusSucceeded {
		t.Fatalf("status = %s", j.Status)
	}

	s.Delete("a")
	s.Delete("a") // idempotent
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
	// A deleted ID must also leave the eviction order, or Create could later
	// "evict" a job that no longer exists and corrupt the accounting.
	for _, id := range []string{"x", "y", "z", "w"} {
		if err := s.Create(Job{ID: id, Status: StatusSucceeded}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3", s.Len())
	}
}

func TestStoreGetReturnsCopy(t *testing.T) {
	s := NewStore(1)
	_ = s.Create(Job{ID: "a", Status: StatusQueued})
	j, _ := s.Get("a")
	j.Status = StatusFailed
	if again, _ := s.Get("a"); again.Status != StatusQueued {
		t.Fatal("mutating a returned Job must not affect the store")
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	s := NewStore(50)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				id := string(rune('a'+g)) + string(rune('0'+i%10))
				_ = s.Create(Job{ID: id, Status: StatusSucceeded})
				s.Update(id, func(j *Job) { j.Failure = "x" })
				s.Get(id)
				s.Delete(id)
				s.Len()
			}
		}(g)
	}
	wg.Wait()
}

func TestValidationErrorAndStatusHelpers(t *testing.T) {
	e := &ValidationError{Field: "items", Reason: "must be positive"}
	if e.Error() != "items must be positive" {
		t.Errorf("Error() = %q", e.Error())
	}
	for st, want := range map[JobStatus]bool{
		StatusQueued: false, StatusRunning: false,
		StatusSucceeded: true, StatusFailed: true, StatusCanceled: true,
	} {
		if st.Terminal() != want {
			t.Errorf("%s.Terminal() = %v, want %v", st, st.Terminal(), want)
		}
	}
}
