package swaggerui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func do(r http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestServesDocsOutsideProduction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	Register(r, "development")

	rec := do(r, "/docs")
	if rec.Code != http.StatusOK {
		t.Fatalf("/docs status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "swagger-ui") {
		t.Error("expected Swagger UI markup at /docs")
	}
	if !strings.Contains(rec.Body.String(), "/docs/openapi.yaml") {
		t.Error("expected the page to point at the spec URL")
	}

	rec = do(r, "/docs/openapi.yaml")
	if rec.Code != http.StatusOK {
		t.Fatalf("/docs/openapi.yaml status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Error("expected non-empty spec body")
	}

	rec = do(r, "/docs/openapi.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("/docs/openapi.json status = %d, want 200", rec.Code)
	}
}

func TestHiddenInProduction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	Register(r, "production")

	for _, p := range []string{"/docs", "/docs/openapi.yaml", "/docs/openapi.json"} {
		if rec := do(r, p); rec.Code != http.StatusNotFound {
			t.Errorf("%s in production = %d, want 404", p, rec.Code)
		}
	}
}
