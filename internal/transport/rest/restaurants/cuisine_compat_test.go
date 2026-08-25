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

// cuisineVenue builds a venue whose cuisine set is known, so both halves of
// the compatibility contract can be asserted on one payload.
func cuisineVenue() (domain.RestaurantListItem, *domain.RestaurantAggregate) {
	id := uuid.New()
	set := []domain.Cuisine{
		{ID: uuid.New(), Code: "italian", Name: "Итальянская", NameI18n: domain.I18n{"en": "Italian"}},
		{ID: uuid.New(), Code: "european", Name: "Европейская", NameI18n: domain.I18n{"en": "European"}},
	}
	base := domain.Restaurant{
		ID: id, Name: "Trattoria", City: domain.CityAlmaty, PriceCategory: domain.PriceMid,
		// Deliberately STALE: the stored string says one cuisine while the set
		// says two. The response must render the set, so an old build never
		// sees a cuisine the venue no longer claims.
		CuisineType: "Итальянская",
		IsActive:    true,
	}
	item := domain.RestaurantListItem{Restaurant: base, Cuisines: set}
	agg := &domain.RestaurantAggregate{Restaurant: base, Cuisines: set}
	return item, agg
}

// TestCuisineTypeStaysAStringForOldBuilds is the contract that lets this
// migration ship at all: 1.4 is live in the App Store and reads ONE string.
// `cuisine_type` must therefore still be present, still be a plain string, and
// now contain the venue's cuisines joined with ", ".
func TestCuisineTypeStaysAStringForOldBuilds(t *testing.T) {
	item, agg := cuisineVenue()
	f := &fakeFacade{item: item, agg: agg}
	r := newTestRouter(f)

	for _, path := range []string{
		"/api/v1/restaurants",
		"/api/v1/restaurants/" + agg.Restaurant.ID.String(),
	} {
		t.Run(path, func(t *testing.T) {
			raw := rawVenue(t, r, path)

			var cuisineType string
			if err := json.Unmarshal(raw["cuisine_type"], &cuisineType); err != nil {
				t.Fatalf("cuisine_type is not a plain string any more: %v (%s)", err, raw["cuisine_type"])
			}
			if cuisineType != "Итальянская, Европейская" {
				t.Errorf("cuisine_type = %q, want the set joined with \", \"", cuisineType)
			}

			var cuisines []struct {
				Code     string `json:"code"`
				Name     string `json:"name"`
				ImageURL string `json:"image_url"`
			}
			if err := json.Unmarshal(raw["cuisines"], &cuisines); err != nil {
				t.Fatalf("decode cuisines: %v (%s)", err, raw["cuisines"])
			}
			if len(cuisines) != 2 || cuisines[0].Code != "italian" {
				t.Errorf("cuisines = %+v, want the venue's own order", cuisines)
			}
		})
	}
}

// TestCuisineTypeFollowsTheRequestedLanguage: the stored column is Russian, but
// a client that asks for English gets the joined string translated — the same
// rule every other localizable field already follows.
func TestCuisineTypeFollowsTheRequestedLanguage(t *testing.T) {
	item, agg := cuisineVenue()
	r := newTestRouter(&fakeFacade{item: item, agg: agg})

	raw := rawVenue(t, r, "/api/v1/restaurants?lang=en")
	var cuisineType string
	if err := json.Unmarshal(raw["cuisine_type"], &cuisineType); err != nil {
		t.Fatalf("decode cuisine_type: %v", err)
	}
	if cuisineType != "Italian, European" {
		t.Errorf("cuisine_type(en) = %q, want %q", cuisineType, "Italian, European")
	}
}

// TestVenueWithoutDictionaryLinksKeepsItsStoredString: a venue whose composite
// spelling is still awaiting a human decision must keep answering exactly as it
// does today — no empty cuisine, and no invented array.
func TestVenueWithoutDictionaryLinksKeepsItsStoredString(t *testing.T) {
	base := domain.Restaurant{
		ID: uuid.New(), Name: "Corner Cafe", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, CuisineType: "Кафе, европейская", IsActive: true,
	}
	item := domain.RestaurantListItem{Restaurant: base}
	agg := &domain.RestaurantAggregate{Restaurant: base}
	r := newTestRouter(&fakeFacade{item: item, agg: agg})

	raw := rawVenue(t, r, "/api/v1/restaurants")
	var cuisineType string
	if err := json.Unmarshal(raw["cuisine_type"], &cuisineType); err != nil {
		t.Fatalf("decode cuisine_type: %v", err)
	}
	if cuisineType != "Кафе, европейская" {
		t.Errorf("cuisine_type = %q, want the original string untouched", cuisineType)
	}
	if _, present := raw["cuisines"]; present {
		t.Error("cuisines must be OMITTED, not an empty array: absent means 'not mapped yet', [] would mean 'declared none'")
	}
}

// TestSearchSplitsACommaSeparatedCuisineFilter pins the query-string half of
// the compatibility story: an already-installed app scrapes its chips out of
// the catalog, so it sends the whole composite string back as a filter.
func TestSearchSplitsACommaSeparatedCuisineFilter(t *testing.T) {
	item, agg := cuisineVenue()
	f := &fakeFacade{item: item, agg: agg}
	r := newTestRouter(f)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/restaurants/search?cuisine=%D0%9A%D0%B0%D1%84%D0%B5,%20%D0%B5%D0%B2%D1%80%D0%BE%D0%BF%D0%B5%D0%B9%D1%81%D0%BA%D0%B0%D1%8F&cuisine=%D0%98%D1%82%D0%B0%D0%BB%D1%8C%D1%8F%D0%BD%D1%81%D0%BA%D0%B0%D1%8F", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("search = %d, body %s", w.Code, w.Body.String())
	}
	want := []string{"Кафе", "европейская", "Итальянская"}
	if len(f.gotSearch.Cuisines) != len(want) {
		t.Fatalf("cuisines reaching the usecase = %v, want %v", f.gotSearch.Cuisines, want)
	}
	for i, v := range want {
		if f.gotSearch.Cuisines[i] != v {
			t.Errorf("cuisine[%d] = %q, want %q", i, f.gotSearch.Cuisines[i], v)
		}
	}
}

// rawVenue returns the venue object of either the listing or the detail route
// as RAW JSON fields, so the assertions read the wire format rather than a
// struct that would happily paper over a changed type.
func rawVenue(t *testing.T, r *gin.Engine, path string) map[string]json.RawMessage {
	t.Helper()
	env := doGET(t, r, path)
	raw, ok := env["data"]
	if !ok {
		t.Fatalf("no data in envelope: %v", env)
	}
	// The listing wraps items in a page envelope; the detail route does not.
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err == nil && page.Items != nil {
		if len(page.Items) == 0 {
			t.Fatal("empty listing")
		}
		raw = page.Items[0]
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode venue: %v (%s)", err, raw)
	}
	return fields
}
