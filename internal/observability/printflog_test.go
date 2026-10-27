package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestPrintfLogger_JSONLineWithLevelAndComponent(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, nil))

	NewPrintfLogger(l, "go-redis").Printf(context.Background(),
		"redis: connection pool: failed to dial after %d attempts: %s\n", 5, "connection refused")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not one JSON line: %v: %q", err, buf.String())
	}
	if line["level"] != "WARN" {
		t.Errorf("level: got %v, want WARN", line["level"])
	}
	if line["component"] != "go-redis" {
		t.Errorf("component: got %v, want go-redis", line["component"])
	}
	want := "redis: connection pool: failed to dial after 5 attempts: connection refused"
	if line["msg"] != want {
		t.Errorf("msg: got %q, want %q", line["msg"], want)
	}
}

func TestPrintfLogger_CarriesContextFields(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil)))

	ctx := WithRequestID(context.Background(), "req-1")
	NewPrintfLogger(l, "go-redis").Printf(ctx, "redis: connection pool: failed to dial")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not one JSON line: %v: %q", err, buf.String())
	}
	if line["request_id"] != "req-1" {
		t.Errorf("request_id: got %v, want req-1", line["request_id"])
	}
}
