package gastroguide

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	uc "backend-core/internal/usecase/gastroguide"
)

// --- guest side ---

type fakeRouteFacade struct {
	detail *domain.GastroRouteDetail
	items  []domain.GastroRoute
	err    error

	gotCity *domain.City
	gotSlug string
}

func (f *fakeRouteFacade) ListRoutes(_ context.Context, in uc.RouteListInput) ([]domain.GastroRoute, int, error) {
	f.gotCity = in.City
	return f.items, len(f.items), f.err
}

func (f *fakeRouteFacade) GetRoute(_ context.Context, slug string) (*domain.GastroRouteDetail, error) {
	f.gotSlug = slug
	if f.err != nil {
		return nil, f.err
	}
	return f.detail, nil
}

var _ uc.RouteFacade = (*fakeRouteFacade)(nil)

func guestRouter(f uc.RouteFacade) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewRouteHandler(f).RegisterPublic(r.Group("/api/v1"))
	return r
}

func get(t *testing.T, r *gin.Engine, url string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	return w
}

// The exact guest shape, pinned: the client is written against it. A place stop
// carries its own content and NO venue object; a venue stop carries the card
// resolved server-side, so the app never fans out one request per stop.
func TestRouteHandler_GuestShape(t *testing.T) {
	venueID := uuid.New()
	lat, lng := 43.238949, 76.889709
	photo := "https://cdn.example/park.jpg"
	f := &fakeRouteFacade{detail: &domain.GastroRouteDetail{
		GastroRoute: domain.GastroRoute{
			ID: uuid.New(), Slug: "classic-almaty", Title: "Классический тур по Алматы",
			DurationLabel: "1 день · 4 точки", PointCount: 2, Position: 1,
		},
		Points: []domain.GuideRoutePoint{
			{
				ID: uuid.New(), Position: 1, Kind: domain.GuideRoutePointRestaurant,
				Title: "Утро: Daily Coffee",
				Venue: &domain.GuideRoutePointVenue{
					ID: venueID, Name: "Daily Coffee", Address: "Достык, 1",
					City: domain.CityAlmaty, PriceCategory: domain.PriceMid, IsActive: true,
				},
			},
			{
				ID: uuid.New(), Position: 2, Kind: domain.GuideRoutePointPlace,
				Title: "Парк 28 панфиловцев", PhotoURL: &photo,
				Address: "Гоголя, 40", Latitude: &lat, Longitude: &lng,
			},
		},
	}}

	w := get(t, guestRouter(f), "/api/v1/gastroguide/routes/classic-almaty")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var body struct {
		Data struct {
			Slug          string `json:"slug"`
			DurationLabel string `json:"duration_label"`
			PointCount    int    `json:"point_count"`
			Points        []struct {
				Position int      `json:"position"`
				Kind     string   `json:"kind"`
				Title    string   `json:"title"`
				PhotoURL *string  `json:"photo_url"`
				Latitude *float64 `json:"latitude"`
				Venue    *struct {
					ID   string `json:"id"`
					Name string `json:"name"`
					City string `json:"city"`
				} `json:"venue"`
			} `json:"points"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if body.Data.Slug != "classic-almaty" || body.Data.DurationLabel != "1 день · 4 точки" || body.Data.PointCount != 2 {
		t.Fatalf("card fields = %+v", body.Data)
	}
	if len(body.Data.Points) != 2 {
		t.Fatalf("points = %d, want 2", len(body.Data.Points))
	}
	first := body.Data.Points[0]
	if first.Kind != "restaurant" || first.Venue == nil {
		t.Fatalf("venue stop = %+v, want a resolved venue card", first)
	}
	if first.Venue.ID != venueID.String() || first.Venue.Name != "Daily Coffee" {
		t.Errorf("venue card = %+v, want the catalog fields", first.Venue)
	}
	if first.Venue.City != string(domain.CityAlmaty) {
		t.Errorf("venue city = %q, want %q", first.Venue.City, domain.CityAlmaty)
	}
	second := body.Data.Points[1]
	if second.Kind != "place" || second.Venue != nil {
		t.Fatalf("place stop = %+v, want no venue object", second)
	}
	if second.Title != "Парк 28 панфиловцев" || second.PhotoURL == nil || second.Latitude == nil {
		t.Errorf("place stop lost its own content: %+v", second)
	}
}

// A typo in ?city= is a 422 with a machine-readable code, not a silently empty
// list: "nothing found because of a typo" and "we have nothing for your city"
// look identical on a home screen and mean very different things.
func TestRouteHandler_UnknownCityIsRefused(t *testing.T) {
	f := &fakeRouteFacade{}
	w := get(t, guestRouter(f), "/api/v1/gastroguide/routes?city=Almata")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body %s, want 422", w.Code, w.Body.String())
	}
	var body struct{ Code string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != string(domain.CodeCityRequired) {
		t.Errorf("code = %q, want %q", body.Code, domain.CodeCityRequired)
	}
	if f.gotCity != nil {
		t.Errorf("the facade was called with %v despite the refusal", f.gotCity)
	}
}

// A known city reaches the facade as a city, and a missing one as "no filter".
func TestRouteHandler_CityFilterReachesTheFacade(t *testing.T) {
	f := &fakeRouteFacade{}
	r := guestRouter(f)

	if w := get(t, r, "/api/v1/gastroguide/routes?city="+string(domain.CityAlmaty)); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if f.gotCity == nil || *f.gotCity != domain.CityAlmaty {
		t.Fatalf("city = %v, want %q", f.gotCity, domain.CityAlmaty)
	}
	f.gotCity = nil
	if w := get(t, r, "/api/v1/gastroguide/routes"); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if f.gotCity != nil {
		t.Errorf("city = %v, want nil (no filter)", f.gotCity)
	}
}

// --- editor side ---

type fakeRouteEditor struct {
	err error

	gotActor EditorActorRecorder
	gotOrder []uuid.UUID
	gotRoute uuid.UUID
	gotPoint uc.PointInput
	calls    int
}

// EditorActorRecorder keeps the actor the handler built, so the tests can check
// the ROLE is carried through unchanged.
type EditorActorRecorder = uc.EditorActor

func (f *fakeRouteEditor) ListRoutes(_ context.Context, a uc.EditorActor, _ uc.RouteAdminListInput) ([]domain.GastroRoute, int, error) {
	f.calls, f.gotActor = f.calls+1, a
	return nil, 0, f.err
}

func (f *fakeRouteEditor) GetRoute(_ context.Context, a uc.EditorActor, _ uuid.UUID) (*domain.GastroRouteAdminDetail, error) {
	f.calls, f.gotActor = f.calls+1, a
	if f.err != nil {
		return nil, f.err
	}
	return &domain.GastroRouteAdminDetail{}, nil
}

func (f *fakeRouteEditor) CreateRoute(_ context.Context, a uc.EditorActor, _ uc.RouteInput) (*domain.GastroRoute, error) {
	f.calls, f.gotActor = f.calls+1, a
	return &domain.GastroRoute{ID: uuid.New()}, f.err
}

func (f *fakeRouteEditor) UpdateRoute(_ context.Context, a uc.EditorActor, _ uuid.UUID, _ uc.RouteInput) (*domain.GastroRoute, error) {
	f.calls, f.gotActor = f.calls+1, a
	return &domain.GastroRoute{ID: uuid.New()}, f.err
}

func (f *fakeRouteEditor) Publish(_ context.Context, a uc.EditorActor, _ uuid.UUID, at *time.Time) (*domain.GastroRoute, error) {
	f.calls, f.gotActor = f.calls+1, a
	return &domain.GastroRoute{PublishedAt: at}, f.err
}

func (f *fakeRouteEditor) Unpublish(_ context.Context, a uc.EditorActor, _ uuid.UUID) (*domain.GastroRoute, error) {
	f.calls, f.gotActor = f.calls+1, a
	return &domain.GastroRoute{}, f.err
}

func (f *fakeRouteEditor) Archive(_ context.Context, a uc.EditorActor, _ uuid.UUID) (*domain.GastroRoute, error) {
	f.calls, f.gotActor = f.calls+1, a
	return &domain.GastroRoute{}, f.err
}

func (f *fakeRouteEditor) AddPoint(_ context.Context, a uc.EditorActor, route uuid.UUID, in uc.PointInput) (*domain.GuideRoutePoint, error) {
	f.calls, f.gotActor, f.gotRoute, f.gotPoint = f.calls+1, a, route, in
	return &domain.GuideRoutePoint{ID: uuid.New(), Kind: in.Kind, Title: in.Title}, f.err
}

func (f *fakeRouteEditor) UpdatePoint(_ context.Context, a uc.EditorActor, route, _ uuid.UUID, in uc.PointInput) (*domain.GuideRoutePoint, error) {
	f.calls, f.gotActor, f.gotRoute, f.gotPoint = f.calls+1, a, route, in
	return &domain.GuideRoutePoint{ID: uuid.New(), Kind: in.Kind, Title: in.Title}, f.err
}

func (f *fakeRouteEditor) DeletePoint(_ context.Context, a uc.EditorActor, _, _ uuid.UUID) error {
	f.calls, f.gotActor = f.calls+1, a
	return f.err
}

func (f *fakeRouteEditor) ReorderPoints(_ context.Context, a uc.EditorActor, route uuid.UUID, ids []uuid.UUID) error {
	f.calls, f.gotActor, f.gotRoute, f.gotOrder = f.calls+1, a, route, ids
	return f.err
}

var _ uc.RouteEditor = (*fakeRouteEditor)(nil)

// routeAdminRouter builds the router EXACTLY the way bootstrap/app.go mounts
// the route editor — the real middleware.Auth followed by the real
// RequireRole(RoleAdmin) — so these tests cover the router gate itself and not
// only the handler.
func routeAdminRouter(e uc.RouteEditor, role domain.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: role}))
	adminGlobal := authed.Group("")
	adminGlobal.Use(middleware.RequireRole(domain.RoleAdmin))
	NewRouteEditorHandler(e).RegisterAdminRoutes(adminGlobal)
	return r
}

// Nobody but a superadmin gets past the router, on ANY of the route endpoints —
// a venue owner who could write an itinerary would put themselves on every walk
// in the city. The usecase is never even reached.
func TestRouteEditorHandler_OnlySuperadminPasses(t *testing.T) {
	id := uuid.New()
	endpoints := []struct {
		method string
		url    string
		body   any
	}{
		{http.MethodGet, "/api/v1/admin/gastroguide/routes", nil},
		{http.MethodPost, "/api/v1/admin/gastroguide/routes", map[string]any{"slug": "walk", "title": "Прогулка"}},
		{http.MethodGet, "/api/v1/admin/gastroguide/routes/" + id.String(), nil},
		{http.MethodPut, "/api/v1/admin/gastroguide/routes/" + id.String(), map[string]any{"slug": "walk", "title": "Прогулка"}},
		{http.MethodPost, "/api/v1/admin/gastroguide/routes/" + id.String() + "/publish", nil},
		{http.MethodPost, "/api/v1/admin/gastroguide/routes/" + id.String() + "/unpublish", nil},
		{http.MethodPost, "/api/v1/admin/gastroguide/routes/" + id.String() + "/archive", nil},
		{http.MethodPost, "/api/v1/admin/gastroguide/routes/" + id.String() + "/points", map[string]any{"kind": "place", "title": "Точка"}},
		{http.MethodPut, "/api/v1/admin/gastroguide/routes/" + id.String() + "/points/order", map[string]any{"point_ids": []string{uuid.NewString()}}},
		{http.MethodPut, "/api/v1/admin/gastroguide/routes/" + id.String() + "/points/" + uuid.NewString(), map[string]any{"kind": "place", "title": "Точка"}},
		{http.MethodDelete, "/api/v1/admin/gastroguide/routes/" + id.String() + "/points/" + uuid.NewString(), nil},
	}

	for _, role := range []domain.Role{domain.RoleUser, domain.RoleRestaurant} {
		f := &fakeRouteEditor{}
		r := routeAdminRouter(f, role)
		for _, ep := range endpoints {
			w := send(t, r, ep.method, ep.url, ep.body)
			if w.Code != http.StatusForbidden {
				t.Errorf("role %s, %s %s: status = %d, body %s, want 403",
					role, ep.method, ep.url, w.Code, w.Body.String())
			}
		}
		if f.calls != 0 {
			t.Errorf("role %s: the usecase was called %d times behind the router gate", role, f.calls)
		}
	}

	// The same calls as a superadmin are let through (the fake accepts them),
	// so the test above is about the ROLE and not about a broken route table.
	f := &fakeRouteEditor{}
	r := routeAdminRouter(f, domain.RoleAdmin)
	for _, ep := range endpoints {
		if w := send(t, r, ep.method, ep.url, ep.body); w.Code == http.StatusForbidden {
			t.Errorf("superadmin was refused on %s %s: %s", ep.method, ep.url, w.Body.String())
		}
	}
	if f.calls != len(endpoints) {
		t.Errorf("usecase calls = %d, want %d", f.calls, len(endpoints))
	}
	if f.gotActor.Role != domain.RoleAdmin {
		t.Errorf("actor role = %q, want admin", f.gotActor.Role)
	}
}

// The reorder endpoint hands down the intended FINAL sequence untouched — a
// handler that sorted or de-duplicated would hide the stale-screen case the
// usecase is built to refuse.
func TestRouteEditorHandler_ReorderPassesTheFinalOrderThrough(t *testing.T) {
	f := &fakeRouteEditor{}
	r := routeAdminRouter(f, domain.RoleAdmin)
	route := uuid.New()
	a, b, c := uuid.New(), uuid.New(), uuid.New()

	w := send(t, r, http.MethodPut,
		"/api/v1/admin/gastroguide/routes/"+route.String()+"/points/order",
		map[string]any{"point_ids": []string{c.String(), a.String(), b.String()}})
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %s, want 204", w.Code, w.Body.String())
	}
	if f.gotRoute != route {
		t.Errorf("route = %s, want %s", f.gotRoute, route)
	}
	want := []uuid.UUID{c, a, b}
	if len(f.gotOrder) != len(want) {
		t.Fatalf("order = %v, want %v", f.gotOrder, want)
	}
	for i := range want {
		if f.gotOrder[i] != want[i] {
			t.Fatalf("order = %v, want %v", f.gotOrder, want)
		}
	}
}

// A refusal from the usecase reaches the panel as the right status WITH its
// machine-readable code, so it can say «в маршруте нет ни одной точки» instead
// of «что-то пошло не так».
func TestRouteEditorHandler_EmptyRouteRefusalCarriesItsCode(t *testing.T) {
	f := &fakeRouteEditor{err: domain.WithCode(domain.CodeGuideRouteEmpty, domain.ErrValidation)}
	r := routeAdminRouter(f, domain.RoleAdmin)

	w := send(t, r, http.MethodPost,
		"/api/v1/admin/gastroguide/routes/"+uuid.NewString()+"/publish", nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body %s, want 422", w.Code, w.Body.String())
	}
	var body struct{ Code string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != string(domain.CodeGuideRouteEmpty) {
		t.Errorf("code = %q, want %q", body.Code, domain.CodeGuideRouteEmpty)
	}
}

// A stop posted by the panel arrives at the usecase whole: the kind, the venue
// id and the coordinates all survive the DTO.
func TestRouteEditorHandler_PointBodyReachesTheUsecase(t *testing.T) {
	f := &fakeRouteEditor{}
	r := routeAdminRouter(f, domain.RoleAdmin)
	venue := uuid.New()

	w := send(t, r, http.MethodPost,
		"/api/v1/admin/gastroguide/routes/"+uuid.NewString()+"/points",
		map[string]any{
			"kind": "restaurant", "restaurant_id": venue.String(),
			"title": "Утро: Daily Coffee", "latitude": 43.238949, "longitude": 76.889709,
		})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s, want 201", w.Code, w.Body.String())
	}
	if f.gotPoint.Kind != domain.GuideRoutePointRestaurant {
		t.Errorf("kind = %q, want restaurant", f.gotPoint.Kind)
	}
	if f.gotPoint.RestaurantID == nil || *f.gotPoint.RestaurantID != venue {
		t.Errorf("restaurant_id = %v, want %s", f.gotPoint.RestaurantID, venue)
	}
	if f.gotPoint.Latitude == nil || f.gotPoint.Longitude == nil {
		t.Errorf("coordinates lost: %+v", f.gotPoint)
	}
}
