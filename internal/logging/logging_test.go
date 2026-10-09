package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/michael-emmanuel/go-production-http-server/internal/requestid"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, buf.String())
	}
	return m
}

func TestRequestIDAttachedFromContext(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo)

	ctx := requestid.WithContext(context.Background(), "abc123")
	log.InfoContext(ctx, "hello", "k", "v")

	m := decode(t, &buf)
	if m["request_id"] != "abc123" || m["k"] != "v" || m["msg"] != "hello" {
		t.Fatalf("unexpected record: %v", m)
	}
}

func TestNoRequestIDWithoutContextValue(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelInfo).InfoContext(context.Background(), "hello")
	if _, ok := decode(t, &buf)["request_id"]; ok {
		t.Fatal("request_id must be absent when the context has none")
	}
}

func TestRequestIDSurvivesWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo).With("component", "test")

	log.InfoContext(requestid.WithContext(context.Background(), "id-9"), "hello")

	m := decode(t, &buf)
	if m["request_id"] != "id-9" || m["component"] != "test" {
		t.Fatalf("unexpected record: %v", m)
	}
}

func TestLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelWarn).Info("dropped")
	if buf.Len() != 0 {
		t.Fatalf("info record should be filtered at warn level, got %s", buf.String())
	}
}

func TestRequestIDSurvivesWithGroup(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo).WithGroup("g")
	log.InfoContext(requestid.WithContext(context.Background(), "id-7"), "hello", "k", "v")

	m := decode(t, &buf)
	// The ID is added by the handler after the group is opened, so it nests
	// under the group. Documented behavior: prefer logger.With over WithGroup
	// when a top-level request_id matters.
	g, ok := m["g"].(map[string]any)
	if !ok || g["request_id"] != "id-7" || g["k"] != "v" {
		t.Fatalf("unexpected record: %v", m)
	}
}
