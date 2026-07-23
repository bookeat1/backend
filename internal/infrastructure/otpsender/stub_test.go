package otpsender

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// The OTP is a bearer credential. These tests are the guard rail: outside a
// development environment the code must never reach a log line, whatever else
// changes in this package.
func TestStubWithholdsCodeOutsideDevelopment(t *testing.T) {
	for _, env := range []string{"production", "staging", "test", "", "prod"} {
		t.Run(env, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			channel, err := NewStub(log, env).Send(context.Background(), "+77075552233", "482913")
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if channel != "stub" {
				t.Fatalf("channel = %q, want stub", channel)
			}

			out := buf.String()
			if strings.Contains(out, "482913") {
				t.Fatalf("OTP code leaked into the log for env %q: %s", env, out)
			}
			if strings.Contains(out, "+77075552233") {
				t.Fatalf("unmasked phone leaked into the log for env %q: %s", env, out)
			}
			if !strings.Contains(out, "+7707***2233") {
				t.Fatalf("masked phone missing for env %q: %s", env, out)
			}
			if !strings.Contains(out, `"level":"WARN"`) {
				t.Fatalf("a stub answering outside development must warn, got: %s", out)
			}
		})
	}
}

func TestStubLogsCodeInDevelopment(t *testing.T) {
	for _, env := range []string{"development", "Development", " development "} {
		t.Run(env, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, nil))

			if _, err := NewStub(log, env).Send(context.Background(), "+77075552233", "482913"); err != nil {
				t.Fatalf("Send: %v", err)
			}

			out := buf.String()
			if !strings.Contains(out, "482913") {
				t.Fatalf("development stub must print the code, got: %s", out)
			}
		})
	}
}
