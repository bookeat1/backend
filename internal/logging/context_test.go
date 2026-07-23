package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestRequestIDRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := RequestID(ctx); ok {
		t.Fatal("expected no request id on a bare context")
	}
	ctx = WithRequestID(ctx, "req-123")
	id, ok := RequestID(ctx)
	if !ok || id != "req-123" {
		t.Fatalf("RequestID = %q, %v; want req-123, true", id, ok)
	}
}

func TestFromContextFallsBackToDefault(t *testing.T) {
	// A context that never went through the request-id middleware must not
	// panic or return nil — every layer can call logging.FromContext(ctx)
	// unconditionally.
	log := FromContext(context.Background())
	if log == nil {
		t.Fatal("FromContext returned nil")
	}
}

func TestFromContextCarriesRequestID(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))

	ctx := WithRequestID(context.Background(), "req-abc")
	log := base.With(slog.String("request_id", "req-abc"))
	ctx = WithLogger(ctx, log)

	lg := FromContext(ctx)
	lg.Info("something happened")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("unmarshal log line: %v (raw: %s)", err, buf.String())
	}
	if line["request_id"] != "req-abc" {
		t.Errorf("log line request_id = %v, want req-abc", line["request_id"])
	}
}

func TestWithAttachesExtraFieldsWithoutMutatingContext(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))
	ctx := WithLogger(context.Background(), base)

	With(ctx, slog.String("booking_id", "b-1")).Info("booking.created")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("unmarshal log line: %v (raw: %s)", err, buf.String())
	}
	if line["booking_id"] != "b-1" {
		t.Errorf("log line booking_id = %v, want b-1", line["booking_id"])
	}
	// The context's own logger must remain unchanged by With.
	buf.Reset()
	FromContext(ctx).Info("next line")
	var line2 map[string]any
	_ = json.Unmarshal(buf.Bytes(), &line2)
	if _, ok := line2["booking_id"]; ok {
		t.Error("With must not mutate the logger stored on ctx")
	}
}
