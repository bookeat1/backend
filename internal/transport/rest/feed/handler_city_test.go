package feed

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
)

// GET /feed without a city is still a 422 — a rail of another city's offers is
// worse than no rail — but the refusal now carries a machine-readable code, so
// the app can ask the guest to pick a city instead of showing a generic error
// on a first launch. The facade must not be called at all in that case.
func TestMain_MissingOrUnknownCityIsCoded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// A nil facade is the assertion: reaching it would panic.
	NewHandler(nil).RegisterPublic(r.Group("/api/v1"))

	for _, url := range []string{"/api/v1/feed", "/api/v1/feed?city=Париж"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status = %d, want 422", url, w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode %q: %v", url, w.Body.String(), err)
		}
		if body["code"] != string(domain.CodeCityRequired) {
			t.Fatalf("%s: code = %v, want %s", url, body["code"], domain.CodeCityRequired)
		}
		if body["error"] == "" {
			t.Fatalf("%s: the human-readable message must stay", url)
		}
	}
}
