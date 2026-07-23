package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRecoveryConvertsPanicTo500(t *testing.T) {
	buf := withCapturedDefaultLogger(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.Use(Recovery())
	r.GET("/x", func(c *gin.Context) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()

	// Must not panic out of ServeHTTP — that is the whole point.
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response body: %v (raw: %s)", err, rec.Body.String())
	}
	if body["error"] == "" || body["error"] == nil {
		t.Error("expected the standard error envelope, got none")
	}

	line := lastLogLine(t, buf)
	if line["msg"] != "http.panic" {
		t.Errorf("msg = %v, want http.panic", line["msg"])
	}
	if line["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", line["level"])
	}
	if line["panic"] != "boom" {
		t.Errorf("panic field = %v, want boom", line["panic"])
	}
	if _, ok := line["stack"]; !ok {
		t.Error("expected a stack field on the panic log line")
	}
	if _, ok := line["request_id"]; !ok {
		t.Error("expected request_id to still be present on the panic log line")
	}
}

func TestRecoveryDoesNotInterfereWithNormalRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.Use(Recovery())
	r.GET("/ok", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
