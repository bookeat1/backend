package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"backend-core/internal/logging"
)

func newRequestIDEngine(t *testing.T) (*gin.Engine, *string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	var seenID string
	r.GET("/x", func(c *gin.Context) {
		id, _ := logging.RequestID(c.Request.Context())
		seenID = id
		c.Status(http.StatusOK)
	})
	return r, &seenID
}

func TestRequestIDGeneratedWhenAbsent(t *testing.T) {
	r, seenID := newRequestIDEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := rec.Header().Get(RequestIDHeader)
	if got == "" {
		t.Fatal("expected a generated X-Request-Id on the response")
	}
	if *seenID != got {
		t.Errorf("context request id = %q, response header = %q; want equal", *seenID, got)
	}
}

func TestRequestIDReusesIncomingHeader(t *testing.T) {
	r, seenID := newRequestIDEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(RequestIDHeader, "client-supplied-id")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if got := rec.Header().Get(RequestIDHeader); got != "client-supplied-id" {
		t.Errorf("X-Request-Id = %q, want the incoming value reused", got)
	}
	if *seenID != "client-supplied-id" {
		t.Errorf("context request id = %q, want the incoming value reused", *seenID)
	}
}

func TestRequestIDAttachesLoggerToContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	var gotLoggerNonNil bool
	r.GET("/x", func(c *gin.Context) {
		log := logging.FromContext(c.Request.Context())
		gotLoggerNonNil = log != nil
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if !gotLoggerNonNil {
		t.Error("expected logging.FromContext to return a non-nil logger downstream of RequestID")
	}
}
