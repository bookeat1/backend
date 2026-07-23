package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"log/slog"

	"github.com/gin-gonic/gin"
)

// withCapturedDefaultLogger points slog's default logger at a JSON handler
// writing to buf for the duration of the test, and restores the previous
// default on cleanup. AccessLog reads whatever logging.FromContext resolves
// to, which falls back to slog.Default() when RequestID's own attached logger
// is (as here) built from the default at request time — same as the real app,
// which calls slog.SetDefault(log) once in bootstrap.NewApp.
func withCapturedDefaultLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func newAccessLogEngine(status int) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.Use(AccessLog())
	r.GET("/x/:id", func(c *gin.Context) { c.Status(status) })
	return r
}

func lastLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	var out map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &out); err != nil {
		t.Fatalf("unmarshal log line: %v (raw: %s)", err, buf.String())
	}
	return out
}

func TestAccessLogFieldsAndRoutePattern(t *testing.T) {
	buf := withCapturedDefaultLogger(t)
	r := newAccessLogEngine(http.StatusOK)

	req := httptest.NewRequest(http.MethodGet, "/x/42", nil)
	req.Header.Set("User-Agent", "test-agent")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	line := lastLogLine(t, buf)
	if line["msg"] != "http.request" {
		t.Errorf("msg = %v, want http.request", line["msg"])
	}
	if line["path"] != "/x/:id" {
		t.Errorf("path = %v, want the route template /x/:id (not /x/42)", line["path"])
	}
	if line["method"] != http.MethodGet {
		t.Errorf("method = %v, want GET", line["method"])
	}
	if line["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200", line["status"])
	}
	if line["user_agent"] != "test-agent" {
		t.Errorf("user_agent = %v, want test-agent", line["user_agent"])
	}
	if _, ok := line["request_id"]; !ok {
		t.Error("expected request_id to be present via the context logger")
	}
	if _, ok := line["duration_ms"]; !ok {
		t.Error("expected duration_ms field")
	}
	if line["level"] != "INFO" {
		t.Errorf("level = %v, want INFO for a 200", line["level"])
	}
}

func TestAccessLogLevelFollowsStatus(t *testing.T) {
	cases := []struct {
		status    int
		wantLevel string
	}{
		{http.StatusOK, "INFO"},
		{http.StatusNotFound, "WARN"},
		{http.StatusInternalServerError, "ERROR"},
	}
	for _, c := range cases {
		buf := withCapturedDefaultLogger(t)
		r := newAccessLogEngine(c.status)

		req := httptest.NewRequest(http.MethodGet, "/x/1", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		line := lastLogLine(t, buf)
		if line["level"] != c.wantLevel {
			t.Errorf("status %d: level = %v, want %s", c.status, line["level"], c.wantLevel)
		}
	}
}
