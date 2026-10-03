package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func probe(h http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec
}

func TestReadinessTransitions(t *testing.T) {
	c := New()

	if got := probe(c.ReadyHandler()); got.Code != http.StatusServiceUnavailable || !strings.Contains(got.Body.String(), "starting") {
		t.Fatalf("before ready: %d %s", got.Code, got.Body)
	}

	if !c.MarkReady() {
		t.Fatal("MarkReady should transition from starting")
	}
	if got := probe(c.ReadyHandler()); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "ready") {
		t.Fatalf("when ready: %d %s", got.Code, got.Body)
	}

	c.MarkDraining()
	if got := probe(c.ReadyHandler()); got.Code != http.StatusServiceUnavailable || !strings.Contains(got.Body.String(), "draining") {
		t.Fatalf("when draining: %d %s", got.Code, got.Body)
	}
}

func TestDrainingIsTerminal(t *testing.T) {
	c := New()
	c.MarkReady()
	c.MarkDraining()
	if c.MarkReady() {
		t.Fatal("a draining instance must never become ready again")
	}
	if c.State() != StateDraining {
		t.Fatalf("state = %v, want draining", c.State())
	}
}

func TestLivenessIgnoresReadiness(t *testing.T) {
	c := New()
	for name, setup := range map[string]func(){
		"starting": func() {},
		"ready":    func() { c.MarkReady() },
		"draining": func() { c.MarkDraining() },
	} {
		setup()
		if got := probe(c.LiveHandler()); got.Code != http.StatusOK {
			t.Errorf("liveness while %s = %d, want 200", name, got.Code)
		}
	}
}

func TestDrainingChannelClosesOnceAndIsIdempotent(t *testing.T) {
	c := New()
	select {
	case <-c.Draining():
		t.Fatal("channel must be open before draining")
	default:
	}
	c.MarkDraining()
	c.MarkDraining() // must not panic on double close
	select {
	case <-c.Draining():
	default:
		t.Fatal("channel must be closed after MarkDraining")
	}
}

func TestStateString(t *testing.T) {
	for s, want := range map[State]string{StateStarting: "starting", StateReady: "ready", StateDraining: "draining", State(9): "unknown"} {
		if s.String() != want {
			t.Errorf("State(%d) = %q, want %q", s, s.String(), want)
		}
	}
}

func TestReadyChannelClosesOnFirstTransitionOnly(t *testing.T) {
	c := New()
	select {
	case <-c.Ready():
		t.Fatal("Ready channel must be open while starting")
	default:
	}
	if !c.MarkReady() {
		t.Fatal("first MarkReady must succeed")
	}
	if c.MarkReady() { // a second call must neither succeed nor double-close (panic)
		t.Fatal("second MarkReady must report false")
	}
	select {
	case <-c.Ready():
	default:
		t.Fatal("Ready channel must be closed after MarkReady")
	}
}

func TestConcurrentMarkReadyClosesChannelOnce(t *testing.T) {
	c := New()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.MarkReady() }()
	}
	wg.Wait()
	<-c.Ready()
}
