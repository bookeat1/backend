package restaurants

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// featuresVenue builds a venue with a known feature set on both the listing and
// the detail payload.
func featuresVenue() (domain.RestaurantListItem, *domain.RestaurantAggregate) {
	set := []domain.VenueFeature{
		{ID: uuid.New(), Code: "wifi", Name: "Wi-Fi", NameI18n: domain.I18n{"en": "Wi-Fi"}},
		{ID: uuid.New(), Code: "prayer_room", Name: "Намазхана", NameI18n: domain.I18n{"en": "Prayer room"}},
	}
	base := domain.Restaurant{
		ID: uuid.New(), Name: "Aiza", City: domain.CityAlmaty, PriceCategory: domain.PriceMid, IsActive: true,
	}
	return domain.RestaurantListItem{Restaurant: base, Features: set},
		&domain.RestaurantAggregate{Restaurant: base, Features: set}
}

// TestFeaturesTravelOnBothPayloads: the `features[]` field keeps the shape it
// had when these rows were free text (id / name / name_i18n), so a client
// mapper written against the old payload still parses — plus the additive
// `code`, which is what the filter and the client icon key off.
//
// It is asserted on the LISTING too, not just the detail read: a card shown
// under a «Wi-Fi» filter has to be able to say why it matched.
func TestFeaturesTravelOnBothPayloads(t *testing.T) {
	item, agg := featuresVenue()
	r := newTestRouter(&fakeFacade{item: item, agg: agg})

	for _, path := range []string{
		"/api/v1/restaurants",
		"/api/v1/restaurants/" + agg.Restaurant.ID.String(),
	} {
		t.Run(path, func(t *testing.T) {
			raw := rawVenue(t, r, path)
			var features []struct {
				ID       string            `json:"id"`
				Code     string            `json:"code"`
				Name     string            `json:"name"`
				NameI18n map[string]string `json:"name_i18n"`
			}
			if err := json.Unmarshal(raw["features"], &features); err != nil {
				t.Fatalf("features is not the expected array: %v (%s)", err, raw["features"])
			}
			if len(features) != 2 {
				t.Fatalf("features = %+v, want 2", features)
			}
			if features[0].Code != "wifi" || features[0].Name != "Wi-Fi" {
				t.Errorf("features[0] = %+v, want the wifi entry", features[0])
			}
			if features[1].Code != "prayer_room" || features[1].Name != "Намазхана" {
				t.Errorf("features[1] = %+v, want the prayer-room entry", features[1])
			}
			if features[0].ID == "" {
				t.Error("feature id is empty")
			}
		})
	}
}

// TestFeatureNameIsLocalized: a client asking for English gets the translation,
// and a locale nobody translated falls back to the Russian base — never a blank.
func TestFeatureNameIsLocalized(t *testing.T) {
	item, agg := featuresVenue()
	r := newTestRouter(&fakeFacade{item: item, agg: agg})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/"+agg.Restaurant.ID.String(), nil)
	req.Header.Set("Accept-Language", "en")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Prayer room") {
		t.Errorf("English payload does not carry the translated feature name: %s", w.Body.String())
	}
}

// TestFeaturesQueryParamReachesTheFilter: the parameter is READ. Before this
// change the app's «Удобства» section never left the phone at all, and the
// server had nowhere to put it if it had — which is the whole bug.
func TestFeaturesQueryParamReachesTheFilter(t *testing.T) {
	item, agg := featuresVenue()

	t.Run("search", func(t *testing.T) {
		f := &fakeFacade{item: item, agg: agg}
		r := newTestRouter(f)
		doGET(t, r, "/api/v1/restaurants/search?features=wifi,parking&features=halal")
		want := []string{"wifi", "parking", "halal"}
		if !reflect.DeepEqual(f.gotSearch.Features, want) {
			t.Errorf("features reaching the usecase = %v, want %v (repeated AND comma-separated both accepted)",
				f.gotSearch.Features, want)
		}
	})

	t.Run("catalog listing", func(t *testing.T) {
		f := &fakeFacade{item: item, agg: agg}
		r := newTestRouter(f)
		doGET(t, r, "/api/v1/restaurants?features=wifi,%20halal")
		want := []string{"wifi", "halal"}
		if !reflect.DeepEqual(f.gotFilter.Features, want) {
			t.Errorf("features reaching the usecase = %v, want %v (padding trimmed)", f.gotFilter.Features, want)
		}
	})

	t.Run("absent parameter stays absent", func(t *testing.T) {
		f := &fakeFacade{item: item, agg: agg}
		r := newTestRouter(f)
		doGET(t, r, "/api/v1/restaurants/search")
		if len(f.gotSearch.Features) != 0 {
			t.Errorf("features = %v for a query with no filter, want none", f.gotSearch.Features)
		}
	})
}

// TestFreeTextFeaturesAreRefused: the old free-text write path is gone, and it
// must fail LOUDLY rather than accept a body it would silently drop.
//
// Nothing in this repo has ever sent this field (the admin panel does not), so
// the 422 breaks no known caller — and a caller that does send it gets told
// where the replacement is instead of believing it saved something.
func TestFreeTextFeaturesAreRefused(t *testing.T) {
	var req saveRestaurantRequest
	body := `{"name":"X","features":[{"name":"Терраса"}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, err := req.toInput()
	if err == nil {
		t.Fatal("free-text features were accepted; want a validation error")
	}
	if !strings.Contains(err.Error(), "/features") {
		t.Errorf("error %q does not point at the replacement endpoint", err)
	}

	// An EMPTY array is not an attempt to write anything, so it must not fail:
	// a client that always sends every field would otherwise be unable to
	// update a venue at all.
	var empty saveRestaurantRequest
	if err := json.Unmarshal([]byte(`{"name":"X","features":[]}`), &empty); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := empty.toInput(); err != nil {
		t.Errorf("an empty features array was rejected (%v); it writes nothing and must pass", err)
	}
}
