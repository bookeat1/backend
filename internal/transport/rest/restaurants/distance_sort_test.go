package restaurants

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// TestGuestCoordsReachTheUsecase covers the ?lat=&lng= plumbing on both list
// and search: a valid pair reaches domain.RestaurantFilter/
// RestaurantSearchFilter unchanged, and anything else (half a pair, garbage,
// out-of-range) degrades to "no distance sort" rather than a 400 — the same
// convention TestCatalogFiltersReachTheUsecase documents for open_now.
func TestGuestCoordsReachTheUsecase(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		wantLat *float64
		wantLng *float64
	}{
		{name: "no coords", query: ""},
		{name: "valid pair", query: "?lat=43.238293&lng=76.945465", wantLat: f64(43.238293), wantLng: f64(76.945465)},
		{name: "negative valid pair", query: "?lat=-33.86&lng=151.20", wantLat: f64(-33.86), wantLng: f64(151.20)},
		{name: "lat only (half a pair) is ignored", query: "?lat=43.23"},
		{name: "lng only (half a pair) is ignored", query: "?lng=76.94"},
		{name: "lat out of range is ignored", query: "?lat=95&lng=76.94"},
		{name: "lng out of range is ignored", query: "?lat=43.23&lng=190"},
		{name: "garbage is ignored, not a 400", query: "?lat=north&lng=east"},
		{name: "empty values are ignored", query: "?lat=&lng="},
	}

	for _, tc := range tests {
		for _, route := range []string{"/api/v1/restaurants", "/api/v1/restaurants/search"} {
			t.Run(route+" "+tc.name, func(t *testing.T) {
				id := uuid.New()
				rest := activeVenue(id)
				f := &fakeFacade{item: domain.RestaurantListItem{Restaurant: rest}}
				r := newTestRouter(f)

				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, route+tc.query, nil))
				if w.Code != http.StatusOK {
					t.Fatalf("GET %s%s = %d, body %s", route, tc.query, w.Code, w.Body.String())
				}

				var gotLat, gotLng *float64
				if route == "/api/v1/restaurants" {
					gotLat, gotLng = f.gotFilter.GuestLat, f.gotFilter.GuestLng
				} else {
					gotLat, gotLng = f.gotSearch.GuestLat, f.gotSearch.GuestLng
				}
				assertFloatFilter(t, "lat", gotLat, tc.wantLat)
				assertFloatFilter(t, "lng", gotLng, tc.wantLng)
			})
		}
	}
}

func f64(v float64) *float64 { return &v }

func assertFloatFilter(t *testing.T, name string, got, want *float64) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s = %v, want no filter", name, *got)
	case want != nil && got == nil:
		t.Errorf("%s = no filter, want %v", name, *want)
	case want != nil && got != nil && *got != *want:
		t.Errorf("%s = %v, want %v", name, *got, *want)
	}
}
