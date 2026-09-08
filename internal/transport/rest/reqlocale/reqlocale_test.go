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
		// Old store builds (and the imported menu rows) spell Kazakh 'kz'. It
		// must resolve to kk, not fall back to ru: falling back is what made the
		// Kazakh menu look empty.
		{"legacy kz alias resolves to kk", "?lang=kz", "", "kk"},
		{"legacy kz alias with a region subtag", "?lang=kz-KZ", "", "kk"},
		{"legacy kz alias in Accept-Language", "", "kz,ru;q=0.8", "kk"},
		// ko and zh joined SupportedLocales on 2026-09-02. Before that both
		// landed on the "unsupported" branch and were answered in Russian,
		// which is why the Korean and Chinese texts already in the database
		// were unreachable.
		{"korean", "?lang=ko", "", "ko"},
		{"korean with a region subtag", "?lang=ko-KR", "", "ko"},
		{"chinese", "?lang=zh", "", "zh"},
		{"chinese with a region subtag", "?lang=zh-CN", "", "zh"},
		{"chinese with a script subtag", "?lang=zh-Hans", "", "zh"},
		{"chinese with script and region", "?lang=zh-Hans-CN", "", "zh"},
		// Traditional is served the Simplified text, not Russian: it is the
		// closest thing we hold. See domain.NormalizeLocale.
		{"traditional chinese collapses into zh", "?lang=zh-Hant", "", "zh"},
		{"korean in Accept-Language", "", "ko-KR,ko;q=0.9,en;q=0.8", "ko"},
		{"chinese in Accept-Language", "", "zh-Hans-CN,zh;q=0.9", "zh"},
		// The rule that must survive every new language: something we cannot
		// serve is answered in Russian, not with an empty payload.
		{"still-unsupported language falls back to ru", "?lang=ja", "", "ru"},
		{"a country code is not a language", "?lang=cn", "", "ru"},
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
