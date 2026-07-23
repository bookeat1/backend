package reqlocale

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestResolve(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name           string
		query          string
		acceptLanguage string
		want           string
	}{
		{"no signal at all leaves format untouched", "", "", ""},
		{"lang query param, supported", "?lang=kk", "", "kk"},
		{"lang query param, uppercase normalized", "?lang=EN", "", "en"},
		{"lang query param, unsupported falls back to ru", "?lang=fr", "", "ru"},
		{"Accept-Language matches a supported tag", "", "kk-KZ,ru;q=0.8", "kk"},
		{"Accept-Language with no supported tag falls back to ru", "", "fr-FR,de;q=0.8", "ru"},
		{"query param wins over Accept-Language", "?lang=en", "kk", "en"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/restaurants"+tc.query, nil)
			if tc.acceptLanguage != "" {
				req.Header.Set("Accept-Language", tc.acceptLanguage)
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req

			if got := Resolve(c); got != tc.want {
				t.Errorf("Resolve() = %q, want %q", got, tc.want)
			}
		})
	}
}
