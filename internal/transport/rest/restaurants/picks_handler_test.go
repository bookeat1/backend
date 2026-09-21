package restaurants

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/usecase/foryou"
	uc "backend-core/internal/usecase/restaurants"
)

// fakePicks is the admin-facing homepicks.Facade double: the editor's
// read/write pair (Editor/Replace) is what admin_read_test.go-style tests
// exercise here. Guest/GuestResolved/ManualPickIDs are never called by this
// package's handler any more (guestPicks goes through recommend/foryou
// instead) — they exist only so fakePicks satisfies the interface.
type fakePicks struct {
	items []domain.RestaurantListItem

	gotCity string
	// savedCity/savedIDs capture the last Replace call; savedCalled separates
	// "saved an empty list" (a real, meaningful operation) from "never called".
	savedCity   string
	savedIDs    []uuid.UUID
	savedCalled bool
	err         error
}

func (f *fakePicks) Guest(_ context.Context, _ string, _ int) ([]domain.RestaurantListItem, error) {
	return f.items, f.err
}

func (f *fakePicks) GuestResolved(_ context.Context, _ string, _ int) ([]domain.RestaurantListItem, domain.HomePicksMode, error) {
	return f.items, domain.HomePicksModePopular, f.err
}

func (f *fakePicks) ManualPickIDs(_ context.Context, _ string) (map[uuid.UUID]bool, error) {
	return map[uuid.UUID]bool{}, f.err
}

func (f *fakePicks) Editor(_ context.Context, city string) ([]domain.RestaurantListItem, error) {
	f.gotCity = city
	return f.items, f.err
}

func (f *fakePicks) Replace(_ context.Context, city string, ids []uuid.UUID) error {
	f.savedCity, f.savedIDs, f.savedCalled = city, ids, true
	return f.err
}

// fakeRecommend is the recommender (usecase/foryou.Facade) double the guest
// read (guestPicks) actually calls.
type fakeRecommend struct {
	result foryou.Result
	err    error

	gotUserID *uuid.UUID
	gotCity   string
	gotLimit  int
}

func (f *fakeRecommend) Guest(_ context.Context, userID *uuid.UUID, city string, limit int) (foryou.Result, error) {
	f.gotUserID, f.gotCity, f.gotLimit = userID, city, limit
	return f.result, f.err
}

func picksRouter(picks *fakePicks, recommend *fakeRecommend) *gin.Engine {
	return picksRouterAs(picks, recommend, nil)
}

// picksRouterAs is picksRouter plus an optional signed-in caller, injected
// exactly the way middleware.OptionalAuth would (WithAuthUser on the request
// context) — see usecase/promocodes' handler_test.go for the same pattern.
func picksRouterAs(picks *fakePicks, recommend *fakeRecommend, user *middleware.AuthUser) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPicksHandler(picks, recommend, nil)
	api := r.Group("/api/v1")
	api.Use(func(c *gin.Context) {
		if user != nil {
			c.Request = c.Request.WithContext(middleware.WithAuthUser(c.Request.Context(), *user))
		}
		c.Next()
	})
	h.RegisterPublic(api)
	h.RegisterAdminGlobal(api)
	return r
}

func pickVenue(name string) domain.RestaurantListItem {
	return domain.RestaurantListItem{
		Restaurant: domain.Restaurant{ID: uuid.New(), Name: name, IsActive: true},
	}
}

func plainResult(mode foryou.Mode, items ...domain.RestaurantListItem) foryou.Result {
	out := make([]foryou.Item, 0, len(items))
	for _, it := range items {
		out = append(out, foryou.Item{RestaurantListItem: it})
	}
	return foryou.Result{Items: out, Mode: mode}
}

type pickPage struct {
	Data struct {
		Items []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Match *struct {
				Score   int `json:"score"`
				Reasons []struct {
					Code   string `json:"code"`
					Points int    `json:"points"`
					Detail string `json:"detail"`
				} `json:"reasons"`
			} `json:"match"`
		} `json:"items"`
		Total int    `json:"total"`
		Mode  string `json:"mode"`
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
	recommend := &fakeRecommend{result: plainResult(foryou.ModePopular, pickVenue("Выбранное"))}
	api := r.Group("/api/v1")
	NewHandler(&fakeFacade{}, nil, nil, uc.BookingRulesDefaults{}).RegisterPublic(api)
	NewPicksHandler(&fakePicks{}, recommend, nil).RegisterPublic(api)

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
	recommend := &fakeRecommend{result: plainResult(foryou.ModePopular, pickVenue("А"))}
	w := httptest.NewRecorder()
	picksRouter(&fakePicks{}, recommend).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/picks?city=%D0%90%D0%BB%D0%BC%D0%B0%D1%82%D1%8B&limit=3", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if recommend.gotCity != "Алматы" {
		t.Fatalf("city = %q, want Алматы", recommend.gotCity)
	}
	if recommend.gotLimit != 3 {
		t.Fatalf("limit = %d, want 3", recommend.gotLimit)
	}
}

// The main screen must always get an answer: a nonsense limit is ignored (0 =
// "use the default"), never a 422.
func TestGuestPicksIgnoresAGarbageLimit(t *testing.T) {
	recommend := &fakeRecommend{}
	w := httptest.NewRecorder()
	picksRouter(&fakePicks{}, recommend).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/picks?limit=nonsense", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if recommend.gotLimit != 0 {
		t.Fatalf("limit = %d, want 0 (fall back to the default)", recommend.gotLimit)
	}
}

// An empty rail is a 200 with an empty list, not a 404 — the app draws its own
// empty state and a 404 would show it an error instead.
func TestGuestPicksAnswersAnEmptyRailWith200(t *testing.T) {
	w := httptest.NewRecorder()
	picksRouter(&fakePicks{}, &fakeRecommend{result: plainResult(foryou.ModePopular)}).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/picks", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	got := decodePickPage(t, w.Body.String())
	if got.Data.Items == nil || len(got.Data.Items) != 0 {
		t.Fatalf("items = %v, want an empty array", got.Data.Items)
	}
}

// criterion 5: an anonymous request (no bearer token) passes a nil userID —
// the handler must not invent one, and the SAME shape (mode, no match) comes
// back regardless of which non-personalized mode the usecase chose.
func TestGuestPicksAnonymousPassesANilUserID(t *testing.T) {
	recommend := &fakeRecommend{result: plainResult(foryou.ModeEditorial, pickVenue("А"))}
	w := httptest.NewRecorder()
	picksRouter(&fakePicks{}, recommend).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/picks", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if recommend.gotUserID != nil {
		t.Fatalf("userID = %v, want nil for an anonymous caller", *recommend.gotUserID)
	}
	got := decodePickPage(t, w.Body.String())
	if got.Data.Mode != "editorial" {
		t.Fatalf("mode = %q, want editorial", got.Data.Mode)
	}
	if got.Data.Items[0].Match != nil {
		t.Fatal("editorial/popular mode must never carry a match block")
	}
	if cc := w.Header().Get("Cache-Control"); cc != "" {
		t.Fatalf("Cache-Control = %q, want unset outside for_you", cc)
	}
}

// criterion 6: a signed-in caller's id (from OptionalAuth's AuthUser) reaches
// the usecase — this is the ONLY way a for_you decision can ever be made.
func TestGuestPicksAuthenticatedPassesTheUserID(t *testing.T) {
	userID := uuid.New()
	recommend := &fakeRecommend{result: plainResult(foryou.ModePopular)}
	w := httptest.NewRecorder()
	picksRouterAs(&fakePicks{}, recommend, &middleware.AuthUser{ID: userID}).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/picks", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if recommend.gotUserID == nil || *recommend.gotUserID != userID {
		t.Fatalf("userID = %v, want %v", recommend.gotUserID, userID)
	}
}

// criterion 6/§5.6: a for_you card serializes match.score/reasons exactly as
// the contract shows — code/points/detail per reason.
func TestGuestPicksSerializesTheMatchBlock(t *testing.T) {
	venue := pickVenue("Итальянское")
	result := foryou.Result{
		Mode: foryou.ModeForYou,
		Items: []foryou.Item{{
			RestaurantListItem: venue,
			Match: &foryou.Match{
				Score: 600,
				Reasons: []domain.TasteMatchReason{
					{Code: domain.TasteSignalCuisineMatch, Points: 400, Params: map[string]any{"cuisine_codes": []string{"italian"}}, Detail: "matches the guest's cuisine preferences"},
					{Code: domain.TasteSignalBudgetMatch, Points: 200, Detail: "matches the guest's budget tier"},
					{Code: domain.TasteSignalDietMatch, Points: 0, Detail: "no diet data for venue"},
				},
			},
		}},
	}
	userID := uuid.New()
	recommend := &fakeRecommend{result: result}
	w := httptest.NewRecorder()
	picksRouterAs(&fakePicks{}, recommend, &middleware.AuthUser{ID: userID}).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/picks", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	got := decodePickPage(t, w.Body.String())
	if got.Data.Mode != "for_you" {
		t.Fatalf("mode = %q, want for_you", got.Data.Mode)
	}
	item := got.Data.Items[0]
	if item.Match == nil || item.Match.Score != 600 {
		t.Fatalf("match = %+v, want score 600", item.Match)
	}
	if len(item.Match.Reasons) != 3 || item.Match.Reasons[0].Code != "cuisine_match" || item.Match.Reasons[0].Points != 400 {
		t.Fatalf("reasons = %+v", item.Match.Reasons)
	}
	// criterion 2: a 0-point reason still travels — diet_match here.
	if item.Match.Reasons[2].Code != "diet_match" || item.Match.Reasons[2].Points != 0 {
		t.Fatalf("zero-point reason missing/wrong: %+v", item.Match.Reasons[2])
	}
}

// criterion 14: only a for_you answer carries the no-cache header — a
// two-guests-on-one-proxy catastrophe is a caching bug, and editorial/popular
// content is genuinely shared, so it must not be penalized with the same
// header.
func TestGuestPicksSetsCacheControlOnlyForForYou(t *testing.T) {
	userID := uuid.New()
	recommend := &fakeRecommend{result: plainResult(foryou.ModeForYou, pickVenue("А"))}
	w := httptest.NewRecorder()
	picksRouterAs(&fakePicks{}, recommend, &middleware.AuthUser{ID: userID}).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/picks", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q, want %q", got, "private, no-store")
	}
}

// The same two guests, one after another through the same handler instance,
// must get their OWN items — nothing here may be memoized per-process.
func TestGuestPicksTwoGuestsInARowGetDifferentItems(t *testing.T) {
	recommend := &fakeRecommend{}
	router := func(userID uuid.UUID, venueName string) *http.Response {
		recommend.result = plainResult(foryou.ModeForYou, pickVenue(venueName))
		w := httptest.NewRecorder()
		picksRouterAs(&fakePicks{}, recommend, &middleware.AuthUser{ID: userID}).ServeHTTP(w,
			httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/picks", nil))
		return w.Result()
	}
	respA := router(uuid.New(), "Заведение гостя А")
	respB := router(uuid.New(), "Заведение гостя Б")

	bodyA, _ := decodeBody(respA)
	bodyB, _ := decodeBody(respB)
	pageA := decodePickPage(t, bodyA)
	pageB := decodePickPage(t, bodyB)
	if pageA.Data.Items[0].Name == pageB.Data.Items[0].Name {
		t.Fatalf("both guests got %q — items must not be shared across requests", pageA.Data.Items[0].Name)
	}
}

func decodeBody(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// The editor's empty city is the all-cities rail, a real key — not a missing
// parameter to refuse.
func TestAdminPicksTreatsAnEmptyCityAsTheAllCitiesRail(t *testing.T) {
	picks := &fakePicks{}
	w := httptest.NewRecorder()
	picksRouter(picks, &fakeRecommend{}).ServeHTTP(w,
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
	picksRouter(picks, &fakeRecommend{}).ServeHTTP(w, req)

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
	picksRouter(picks, &fakeRecommend{}).ServeHTTP(w, req)

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
	picksRouter(picks, &fakeRecommend{}).ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", w.Code, w.Body.String())
	}
	if picks.savedCalled {
		t.Fatal("a refused save must not reach the usecase")
	}
}
