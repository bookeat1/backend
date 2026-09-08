package restaurants

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakePicks records what the transport asked the usecase for, because most of
// what this handler does is turn a query string into arguments.
type fakePicks struct {
	items []domain.RestaurantListItem

	gotCity  string
	gotLimit int
	// savedCity/savedIDs capture the last Replace call; savedCalled separates
	// "saved an empty list" (a real, meaningful operation) from "never called".
	savedCity   string
	savedIDs    []uuid.UUID
	savedCalled bool
	err         error
}

func (f *fakePicks) Guest(_ context.Context, city string, limit int) ([]domain.RestaurantListItem, error) {
	f.gotCity, f.gotLimit = city, limit
	return f.items, f.err
}

func (f *fakePicks) Editor(_ context.Context, city string) ([]domain.RestaurantListItem, error) {
	f.gotCity = city
	return f.items, f.err
}

func (f *fakePicks) Replace(_ context.Context, city string, ids []uuid.UUID) error {
	f.savedCity, f.savedIDs, f.savedCalled = city, ids, true
	return f.err
}

func picksRouter(f *fakePicks) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPicksHandler(f, nil)
	api := r.Group("/api/v1")
	h.RegisterPublic(api)
	h.RegisterAdminGlobal(api)
	return r
}

func pickVenue(name string) domain.RestaurantListItem {
	return domain.RestaurantListItem{
		Restaurant: domain.Restaurant{ID: uuid.New(), Name: name, IsActive: true},
	}
}

type pickPage struct {
	Data struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
		Total int `json:"total"`
	} `json:"data"`
}

func decodePickPage(t *testing.T, body string) pickPage {
	t.Helper()
	var p pickPage
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("decode: %v (body %s)", err, body)
	}
	return p
}

// The route has to coexist with GET /restaurants/:id — a literal segment next
// to a wildcard is exactly the shape that makes a router answer the wrong
// handler (or refuse to start). /restaurants/search already proves gin copes;
// this proves it for the rail too, WITH the catalog handler mounted alongside.
func TestPicksRouteIsNotSwallowedByTheVenueDetailRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	picks := &fakePicks{items: []domain.RestaurantListItem{pickVenue("Выбранное")}}
	api := r.Group("/api/v1")
	NewHandler(&fakeFacade{}, nil, nil).RegisterPublic(api)
	NewPicksHandler(picks, nil).RegisterPublic(api)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/picks", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	got := decodePickPage(t, w.Body.String())
	if len(got.Data.Items) != 1 || got.Data.Items[0].Name != "Выбранное" {
		t.Fatalf("the detail route answered instead of the rail: %s", w.Body.String())
	}
}

func TestGuestPicksPassesTheCityAndLimitDown(t *testing.T) {
	picks := &fakePicks{items: []domain.RestaurantListItem{pickVenue("А")}}
	w := httptest.NewRecorder()
	picksRouter(picks).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/picks?city=%D0%90%D0%BB%D0%BC%D0%B0%D1%82%D1%8B&limit=3", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if picks.gotCity != "Алматы" {
		t.Fatalf("city = %q, want Алматы", picks.gotCity)
	}
	if picks.gotLimit != 3 {
		t.Fatalf("limit = %d, want 3", picks.gotLimit)
	}
}

// The main screen must always get an answer: a nonsense limit is ignored (0 =
// "use the default"), never a 422.
func TestGuestPicksIgnoresAGarbageLimit(t *testing.T) {
	picks := &fakePicks{}
	w := httptest.NewRecorder()
	picksRouter(picks).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/picks?limit=nonsense", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if picks.gotLimit != 0 {
		t.Fatalf("limit = %d, want 0 (fall back to the default)", picks.gotLimit)
	}
}

// An empty rail is a 200 with an empty list, not a 404 — the app draws its own
// empty state and a 404 would show it an error instead.
func TestGuestPicksAnswersAnEmptyRailWith200(t *testing.T) {
	w := httptest.NewRecorder()
	picksRouter(&fakePicks{}).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/picks", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	got := decodePickPage(t, w.Body.String())
	if got.Data.Items == nil || len(got.Data.Items) != 0 {
		t.Fatalf("items = %v, want an empty array", got.Data.Items)
	}
}

// The editor's empty city is the all-cities rail, a real key — not a missing
// parameter to refuse.
func TestAdminPicksTreatsAnEmptyCityAsTheAllCitiesRail(t *testing.T) {
	picks := &fakePicks{}
	w := httptest.NewRecorder()
	picksRouter(picks).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/restaurants/picks", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if picks.gotCity != domain.HomePicksAllCities {
		t.Fatalf("city = %q, want the all-cities key", picks.gotCity)
	}
}

func TestReplacePicksPassesTheOrderThrough(t *testing.T) {
	picks := &fakePicks{}
	a, b := uuid.New(), uuid.New()
	body := `{"city":"Алматы","restaurant_ids":["` + a.String() + `","` + b.String() + `"]}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/restaurants/picks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	picksRouter(picks).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if picks.savedCity != "Алматы" {
		t.Fatalf("city = %q, want Алматы", picks.savedCity)
	}
	if len(picks.savedIDs) != 2 || picks.savedIDs[0] != a || picks.savedIDs[1] != b {
		t.Fatalf("ids = %v, want [%s %s] in that order", picks.savedIDs, a, b)
	}
}

// Saving an empty list is the "back to automatic" switch and must reach the
// usecase, not be short-circuited as "nothing to do".
func TestReplacePicksAcceptsAnEmptyListAsClearing(t *testing.T) {
	picks := &fakePicks{}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/restaurants/picks",
		strings.NewReader(`{"city":"Алматы","restaurant_ids":[]}`))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	picksRouter(picks).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if !picks.savedCalled {
		t.Fatal("clearing must reach the usecase")
	}
	if len(picks.savedIDs) != 0 {
		t.Fatalf("ids = %v, want none", picks.savedIDs)
	}
}

func TestReplacePicksRefusesAnIDThatIsNotAUUID(t *testing.T) {
	picks := &fakePicks{}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/restaurants/picks",
		strings.NewReader(`{"city":"Алматы","restaurant_ids":["не-uuid"]}`))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	picksRouter(picks).ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", w.Code, w.Body.String())
	}
	if picks.savedCalled {
		t.Fatal("a refused save must not reach the usecase")
	}
}
