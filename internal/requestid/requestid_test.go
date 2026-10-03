package requestid

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestNewIsValidAndFixedLength(t *testing.T) {
	id := New()
	if len(id) != 32 || !Valid(id) {
		t.Fatalf("New() = %q; want 32 valid chars", id)
	}
}

func TestNewIsUniqueUnderConcurrency(t *testing.T) {
	const goroutines, per = 16, 500
	ids := make(chan string, goroutines*per)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				ids <- New()
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, goroutines*per)
	for id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"abc-123_DEF.x", true},
		{"", false},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"has space", false},
		{"new\nline", false},
		{"quote\"", false},
		{"ünï", false},
	}
	for _, tc := range tests {
		if got := Valid(tc.id); got != tc.want {
			t.Errorf("Valid(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestContextRoundTrip(t *testing.T) {
	if got := FromContext(context.Background()); got != "" {
		t.Fatalf("empty context should yield empty id, got %q", got)
	}
	ctx := WithContext(context.Background(), "req-1")
	if got := FromContext(ctx); got != "req-1" {
		t.Fatalf("FromContext = %q, want req-1", got)
	}
}
