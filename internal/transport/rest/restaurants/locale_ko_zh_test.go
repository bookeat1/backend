package restaurants

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Korean and Chinese venue descriptions had been sitting in description_i18n
// since the legacy import, and every read answered Russian: reqlocale sent any
// tag outside SupportedLocales to LocaleRU, so the texts were unreachable.
// These tests pin the reachable/unreachable line where it now is.
//
// The venue below carries all five languages plus one the code does not know
// ("ja"), which is what an unsupported tag must fall back FROM.
const (
	descRU = "Уютное место у реки"
	descKK = "Өзен жағасындағы жайлы орын"
	descEN = "A cosy place by the river"
	descKO = "강가의 아늑한 공간"
	descZH = "河畔的舒适空间"
	descJA = "川沿いの居心地よい空間"
)

func localizedVenue() (domain.RestaurantListItem, *domain.RestaurantAggregate) {
	id := uuid.New()
	base := domain.Restaurant{
		ID: id, Name: "У реки", City: domain.CityAlmaty, PriceCategory: domain.PriceMid,
		IsActive:    true,
		Description: descRU,
		DescriptionI18n: domain.I18n{
			domain.LocaleRU: descRU,
			domain.LocaleKK: descKK,
			domain.LocaleEN: descEN,
			domain.LocaleKO: descKO,
			domain.LocaleZH: descZH,
			"ja":            descJA,
		},
	}
	return domain.RestaurantListItem{Restaurant: base}, &domain.RestaurantAggregate{Restaurant: base}
}

// description reads the resolved (not raw-map) description out of a venue
// payload, which is what a guest's client actually renders.
func description(t *testing.T, r *gin.Engine, path string) string {
	t.Helper()
	raw, ok := rawVenue(t, r, path)["description"]
	if !ok {
		t.Fatalf("GET %s: no description in the payload", path)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("GET %s: description is not a string: %v", path, err)
	}
	return s
}

// TestLangResolvesEverySupportedLocale is the whole point of adding ko and zh:
// ?lang=ko must answer Korean and ?lang=zh Chinese, on BOTH the listing and the
// detail route, because the two build their payload through different
// functions and only the listing is cached by the app.
func TestLangResolvesEverySupportedLocale(t *testing.T) {
	item, agg := localizedVenue()
	r := newTestRouter(&fakeFacade{item: item, agg: agg})
	paths := map[string]string{
		"listing": "/api/v1/restaurants?lang=",
		"detail":  "/api/v1/restaurants/" + agg.ID.String() + "?lang=",
	}

	for _, tc := range []struct{ lang, want string }{
		{"ru", descRU},
		{"kk", descKK},
		{"en", descEN},
		{"ko", descKO},
		{"zh", descZH},
	} {
		for route, prefix := range paths {
			if got := description(t, r, prefix+tc.lang); got != tc.want {
				t.Errorf("%s ?lang=%s: description = %q, want %q", route, tc.lang, got, tc.want)
			}
		}
	}
}

// Region and script subtags are dropped, so the tags a real device sends —
// ko-KR from a Korean phone, zh-Hans-CN / zh-Hant-TW from a Chinese one —
// reach the same translation. zh-Hant getting Simplified is deliberate: it is
// the closest text we hold, and the alternative is Russian.
func TestLangAcceptsRegionAndScriptSubtags(t *testing.T) {
	item, agg := localizedVenue()
	r := newTestRouter(&fakeFacade{item: item, agg: agg})

	for _, tc := range []struct{ lang, want string }{
		{"ko-KR", descKO},
		{"KO", descKO},
		{"kor", descKO},
		{"zh-CN", descZH},
		{"zh-Hans", descZH},
		{"zh-Hans-CN", descZH},
		{"zh-Hant", descZH},
		{"zh_TW", descZH},
		{"zho", descZH},
	} {
		if got := description(t, r, "/api/v1/restaurants?lang="+tc.lang); got != tc.want {
			t.Errorf("?lang=%s: description = %q, want %q", tc.lang, got, tc.want)
		}
	}
}

// The rule that must NOT change while new languages are added: a tag nothing
// can serve answers Russian, never an empty string and never a half-translated
// payload. "ja" is in the stored map on purpose — an unsupported tag must be
// refused by the LOCALE set, not quietly served because the data happens to
// have that key.
func TestUnsupportedLangStillAnswersRussian(t *testing.T) {
	item, agg := localizedVenue()
	r := newTestRouter(&fakeFacade{item: item, agg: agg})

	for _, lang := range []string{"fr", "de", "xx", "ja", "cn", "zzzz", "-"} {
		if got := description(t, r, "/api/v1/restaurants?lang="+lang); got != descRU {
			t.Errorf("?lang=%s: description = %q, want the Russian base %q", lang, got, descRU)
		}
	}
}

// Accept-Language is the other half of the resolution and the one a browser
// sets by itself; it has to agree with ?lang= on both the new languages and on
// the fallback.
func TestAcceptLanguageResolvesTheNewLocales(t *testing.T) {
	item, agg := localizedVenue()
	r := newTestRouter(&fakeFacade{item: item, agg: agg})

	for _, tc := range []struct{ header, want string }{
		{"ko-KR,ko;q=0.9,en;q=0.8", descKO},
		{"zh-Hans-CN,zh;q=0.9", descZH},
		{"kk-KZ,ru;q=0.8", descKK},
		{"fr-FR,de;q=0.8", descRU},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/restaurants", nil)
		req.Header.Set("Accept-Language", tc.header)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("Accept-Language %q: status %d (%s)", tc.header, w.Code, w.Body)
		}
		var env struct {
			Data struct {
				Items []struct {
					Description string `json:"description"`
				} `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(env.Data.Items) == 0 {
			t.Fatal("empty listing")
		}
		if got := env.Data.Items[0].Description; got != tc.want {
			t.Errorf("Accept-Language %q: description = %q, want %q", tc.header, got, tc.want)
		}
	}
}

// A client that asks for nothing must keep getting the base column, byte for
// byte — that is what keeps the payload identical for every build already in
// the stores.
func TestNoLanguageSignalStillServesTheBaseColumn(t *testing.T) {
	item, agg := localizedVenue()
	r := newTestRouter(&fakeFacade{item: item, agg: agg})

	if got := description(t, r, "/api/v1/restaurants"); got != descRU {
		t.Errorf("no lang signal: description = %q, want the base column %q", got, descRU)
	}
}

// A venue that has no Korean falls back to Russian rather than to some other
// translation: Resolve must not go hunting for the nearest language.
func TestMissingTranslationFallsBackToTheBaseColumnNotToAnotherLanguage(t *testing.T) {
	item, agg := localizedVenue()
	item.DescriptionI18n = domain.I18n{domain.LocaleEN: descEN}
	agg.DescriptionI18n = domain.I18n{domain.LocaleEN: descEN}
	r := newTestRouter(&fakeFacade{item: item, agg: agg})

	if got := description(t, r, "/api/v1/restaurants?lang=ko"); got != descRU {
		t.Errorf("?lang=ko without a Korean text: description = %q, want %q", got, descRU)
	}
}
