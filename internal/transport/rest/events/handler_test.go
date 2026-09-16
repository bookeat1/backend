package events

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	uc "backend-core/internal/usecase/events"
)

// --- fake usecase: records what the transport asked for, answers with canned
// items. One settable err drives the error→HTTP mapping (same shape as the
// reviews/bookings handler tests).

type fakeFacade struct {
	err   error
	items []domain.EventListItem
	total int
	// event, when set, is what GetPublic answers with — drives the detail-shape
	// assertions without a database.
	event *domain.Event
	// detail, when set, is what GetPublicDetail (the event's own page) answers
	// with. It carries the optional venue block, so a platform event is simply
	// one with a nil Restaurant.
	detail *domain.EventListItem
	// ranked/rankedTotal are what ListPublicUpcomingForYou answers with —
	// separate from items/total so a test can tell "the plain listing was
	// called" from "the ranked one was" by which field got populated.
	ranked      []uc.RankedEventListItem
	rankedTotal int

	gotFilter   domain.PublicEventFilter
	calls       int
	forYouCalls int
	gotUserID   uuid.UUID
}

func (f *fakeFacade) ListPublicUpcoming(_ context.Context, flt domain.PublicEventFilter) ([]domain.EventListItem, int, error) {
	f.calls++
	f.gotFilter = flt
	if f.err != nil {
		return nil, 0, f.err
	}
	total := f.total
	if total == 0 {
		total = len(f.items)
	}
	return f.items, total, nil
}

func (f *fakeFacade) ListPublicUpcomingForYou(_ context.Context, flt domain.PublicEventFilter, userID uuid.UUID) ([]uc.RankedEventListItem, int, error) {
	f.forYouCalls++
	f.gotFilter = flt
	f.gotUserID = userID
	if f.err != nil {
		return nil, 0, f.err
	}
	total := f.rankedTotal
	if total == 0 {
		total = len(f.ranked)
	}
	return f.ranked, total, nil
}

// The rest of uc.Facade is not exercised by these tests.
func (f *fakeFacade) Create(context.Context, uc.Actor, uc.CreateInput) (*domain.Event, error) {
	return nil, f.err
}

func (f *fakeFacade) Update(context.Context, uc.Actor, uuid.UUID, uc.UpdateInput) (*domain.Event, error) {
	return nil, f.err
}
func (f *fakeFacade) Delete(context.Context, uc.Actor, uuid.UUID) error { return f.err }
func (f *fakeFacade) GetAdmin(context.Context, uc.Actor, uuid.UUID) (*domain.Event, error) {
	return nil, f.err
}

func (f *fakeFacade) ListAdmin(context.Context, uc.Actor, uuid.UUID, []domain.EventStatus, int, int) ([]domain.Event, int, error) {
	return nil, 0, f.err
}

func (f *fakeFacade) SetRefundPolicy(context.Context, uc.Actor, uuid.UUID, domain.TicketRefundPolicy) (*domain.Event, error) {
	return nil, f.err
}

func (f *fakeFacade) ResetSeriesContent(context.Context, uc.Actor, uuid.UUID, []domain.EventContentField) (*domain.Event, error) {
	return nil, f.err
}

func (f *fakeFacade) ListPublic(context.Context, uuid.UUID, int, int) ([]domain.Event, int, error) {
	return nil, 0, f.err
}

func (f *fakeFacade) GetPublic(context.Context, uuid.UUID, uuid.UUID) (*domain.Event, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.event, nil
}

func (f *fakeFacade) GetPublicDetail(context.Context, uuid.UUID) (*domain.EventListItem, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.detail, nil
}

func (f *fakeFacade) ListPlatformAdmin(context.Context, uc.Actor, []domain.EventStatus, int, int) ([]domain.Event, int, error) {
	return nil, 0, f.err
}

var _ uc.Facade = (*fakeFacade)(nil)

// newPublicRouter registers every guest route, including GET /events
// (RegisterExplore) — anonymous, exactly as if it sat on OptionalAuth with no
// token: OptionalAuth's own contract (middleware/auth.go) is "a missing/
// invalid token behaves exactly like no token", so a plain group with no
// middleware at all is a faithful stand-in for these tests.
func newPublicRouter(f uc.Facade) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1")
	h := NewHandler(f)
	h.RegisterPublic(grp)
	h.RegisterExplore(grp)
	return r
}

// newExploreRouterWithAuth is newPublicRouter's OptionalAuth-signed-in
// counterpart: every request carries au as the authenticated user, exactly
// what middleware.OptionalAuth would attach for a valid bearer token.
func newExploreRouterWithAuth(f uc.Facade, au middleware.AuthUser) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1")
	grp.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(middleware.WithAuthUser(c.Request.Context(), au))
		c.Next()
	})
	h := NewHandler(f)
	h.RegisterPublic(grp)
	h.RegisterExplore(grp)
	return r
}

func do(t *testing.T, r *gin.Engine, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// listEnvelope mirrors response.Page inside the standard envelope.
type listEnvelope struct {
	Data struct {
		Items   []eventListItemResponse `json:"items"`
		Total   int                     `json:"total"`
		Pages   int                     `json:"pages"`
		Page    int                     `json:"page"`
		PerPage int                     `json:"per_page"`
	} `json:"data"`
	Error string `json:"error"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) listEnvelope {
	t.Helper()
	var env listEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return env
}

func sampleItem(startsIn time.Duration, title, venueName string, city domain.City) domain.EventListItem {
	start := time.Now().Add(startsIn).UTC().Truncate(time.Second)
	rid := uuid.New()
	cover := "https://cdn.example.com/cover.jpg"
	return domain.EventListItem{
		Event: domain.Event{
			ID: uuid.New(), RestaurantID: &rid,
			Title: title, TitleI18n: domain.I18n{"en": title + " (en)"},
			Description: "описание", DescriptionI18n: domain.I18n{"en": "description"},
			StartsAt: start, EndsAt: start.Add(2 * time.Hour),
			Venue: "rooftop", CoverImageURL: &cover, Status: domain.EventPublished,
			Tags: []string{"Бранч", "Живая музыка"},
		},
		Restaurant: &domain.EventRestaurant{
			ID: rid, Name: venueName, NameI18n: domain.I18n{"en": venueName + " (en)"}, City: city,
		},
	}
}

// The Explore card must arrive complete in ONE response: the event, its window,
// its cover, and the venue that hosts it — no per-item follow-up call.
func TestListUpcoming_ItemShape(t *testing.T) {
	it := sampleItem(24*time.Hour, "Винный ужин", "Bistro", domain.CityAlmaty)
	f := &fakeFacade{items: []domain.EventListItem{it}}

	rec := do(t, newPublicRouter(f), "/api/v1/events")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	env := decode(t, rec)
	if len(env.Data.Items) != 1 || env.Data.Total != 1 {
		t.Fatalf("items=%d total=%d, want 1/1", len(env.Data.Items), env.Data.Total)
	}
	got := env.Data.Items[0]
	switch {
	case got.ID != it.ID.String():
		t.Fatalf("id = %s, want %s", got.ID, it.ID)
	case got.Title != it.Title:
		t.Fatalf("title = %q, want %q", got.Title, it.Title)
	case got.Description != it.Description:
		t.Fatalf("description = %q, want %q", got.Description, it.Description)
	case got.CoverImageURL == nil || *got.CoverImageURL != *it.CoverImageURL:
		t.Fatalf("cover = %v, want %q", got.CoverImageURL, *it.CoverImageURL)
	case got.StartsAt != it.StartsAt.Format(time.RFC3339):
		t.Fatalf("starts_at = %s, want %s", got.StartsAt, it.StartsAt.Format(time.RFC3339))
	case got.EndsAt != it.EndsAt.Format(time.RFC3339):
		t.Fatalf("ends_at = %s, want %s", got.EndsAt, it.EndsAt.Format(time.RFC3339))
	case got.Restaurant.ID != it.Restaurant.ID.String():
		t.Fatalf("restaurant.id = %s, want %s", got.Restaurant.ID, it.Restaurant.ID)
	case got.Restaurant.Name != "Bistro":
		t.Fatalf("restaurant.name = %q, want Bistro", got.Restaurant.Name)
	case got.Restaurant.City != string(domain.CityAlmaty):
		t.Fatalf("restaurant.city = %q, want %q", got.Restaurant.City, domain.CityAlmaty)
	}
	// A guest-facing item never carries the raw translation maps.
	if got.TitleI18n != nil || got.DescriptionI18n != nil {
		t.Fatalf("raw i18n maps leaked into the guest response: %+v / %+v", got.TitleI18n, got.DescriptionI18n)
	}
}

// The handler must hand the order it was given straight to the client: sorting
// is the database's job (starts_at ASC), and re-ordering here would silently
// break pagination across pages.
func TestListUpcoming_OrderIsPreserved(t *testing.T) {
	soon := sampleItem(2*time.Hour, "Скоро", "A", domain.CityAlmaty)
	later := sampleItem(48*time.Hour, "Позже", "B", domain.CityAstana)
	f := &fakeFacade{items: []domain.EventListItem{soon, later}}

	env := decode(t, do(t, newPublicRouter(f), "/api/v1/events"))
	if len(env.Data.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(env.Data.Items))
	}
	if env.Data.Items[0].ID != soon.ID.String() || env.Data.Items[1].ID != later.ID.String() {
		t.Fatalf("order changed: %s, %s", env.Data.Items[0].ID, env.Data.Items[1].ID)
	}
}

func TestListUpcoming_EmptyListSerializesAsArray(t *testing.T) {
	rec := do(t, newPublicRouter(&fakeFacade{}), "/api/v1/events")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	env := decode(t, rec)
	if env.Data.Items == nil || len(env.Data.Items) != 0 {
		t.Fatalf("items = %v, want an empty array", env.Data.Items)
	}
}

// Each query parameter must arrive at the usecase as the filter it names —
// table-driven, one row per filter plus the combination.
func TestListUpcoming_QueryParametersBecomeFilters(t *testing.T) {
	rid := uuid.New()
	from := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 31, 21, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
		check func(t *testing.T, f domain.PublicEventFilter)
	}{
		{
			name:  "no filters",
			query: "/api/v1/events",
			check: func(t *testing.T, f domain.PublicEventFilter) {
				if f.City != nil || f.RestaurantID != nil || f.From != nil || f.To != nil {
					t.Fatalf("expected no filters, got %+v", f)
				}
				if f.Page != 1 || f.PerPage != defaultPerPage {
					t.Fatalf("page/per_page = %d/%d, want 1/%d", f.Page, f.PerPage, defaultPerPage)
				}
			},
		},
		{
			name:  "city",
			query: "/api/v1/events?city=" + string(domain.CityAstana),
			check: func(t *testing.T, f domain.PublicEventFilter) {
				if f.City == nil || *f.City != domain.CityAstana {
					t.Fatalf("city = %v, want %s", f.City, domain.CityAstana)
				}
			},
		},
		{
			name:  "restaurant_id",
			query: "/api/v1/events?restaurant_id=" + rid.String(),
			check: func(t *testing.T, f domain.PublicEventFilter) {
				if f.RestaurantID == nil || *f.RestaurantID != rid {
					t.Fatalf("restaurant_id = %v, want %s", f.RestaurantID, rid)
				}
			},
		},
		{
			name:  "date range",
			query: "/api/v1/events?from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339),
			check: func(t *testing.T, f domain.PublicEventFilter) {
				if f.From == nil || !f.From.Equal(from) {
					t.Fatalf("from = %v, want %v", f.From, from)
				}
				if f.To == nil || !f.To.Equal(to) {
					t.Fatalf("to = %v, want %v", f.To, to)
				}
			},
		},
		{
			name:  "pagination",
			query: "/api/v1/events?page=3&per_page=5",
			check: func(t *testing.T, f domain.PublicEventFilter) {
				if f.Page != 3 || f.PerPage != 5 {
					t.Fatalf("page/per_page = %d/%d, want 3/5", f.Page, f.PerPage)
				}
			},
		},
		{
			name:  "per_page is capped",
			query: "/api/v1/events?per_page=5000",
			check: func(t *testing.T, f domain.PublicEventFilter) {
				if f.PerPage != maxPerPage {
					t.Fatalf("per_page = %d, want the cap %d", f.PerPage, maxPerPage)
				}
			},
		},
		{
			name: "everything at once",
			query: "/api/v1/events?city=" + string(domain.CityAlmaty) + "&restaurant_id=" + rid.String() +
				"&from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339) + "&page=2&per_page=10",
			check: func(t *testing.T, f domain.PublicEventFilter) {
				if f.City == nil || *f.City != domain.CityAlmaty || f.RestaurantID == nil || *f.RestaurantID != rid ||
					f.From == nil || !f.From.Equal(from) || f.To == nil || !f.To.Equal(to) ||
					f.Page != 2 || f.PerPage != 10 {
					t.Fatalf("combined filters lost: %+v", f)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeFacade{}
			rec := do(t, newPublicRouter(f), tc.query)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
			if f.calls != 1 {
				t.Fatalf("usecase calls = %d, want 1", f.calls)
			}
			tc.check(t, f.gotFilter)
		})
	}
}

// A malformed filter is refused, not ignored: dropping restaurant_id would
// answer with the whole platform's events under the name of one venue, and
// dropping a date bound would answer a different question than the one asked.
func TestListUpcoming_MalformedFiltersAreRefused(t *testing.T) {
	for name, query := range map[string]string{
		"restaurant_id is not a uuid": "/api/v1/events?restaurant_id=not-a-uuid",
		"from is not RFC3339":         "/api/v1/events?from=2026-08-01",
		"to is not RFC3339":           "/api/v1/events?to=tomorrow",
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeFacade{}
			rec := do(t, newPublicRouter(f), query)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
			if f.calls != 0 {
				t.Fatal("a malformed filter must never reach the usecase")
			}
		})
	}
}

// An unknown city is NOT an error (the catalog listing treats it the same way):
// it is passed through and simply matches nothing.
func TestListUpcoming_UnknownCityIsPassedThrough(t *testing.T) {
	f := &fakeFacade{}
	rec := do(t, newPublicRouter(f), "/api/v1/events?city=Атлантида")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if f.gotFilter.City == nil || *f.gotFilter.City != domain.City("Атлантида") {
		t.Fatalf("city = %v, want the raw value passed through", f.gotFilter.City)
	}
}

func TestListUpcoming_ValidationErrorMapsTo422(t *testing.T) {
	f := &fakeFacade{err: domain.ErrValidation}
	rec := do(t, newPublicRouter(f), "/api/v1/events")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

// lang= localizes both the event's own text and the venue's name, and never
// invents a translation it does not have.
func TestListUpcoming_Localization(t *testing.T) {
	it := sampleItem(24*time.Hour, "Винный ужин", "Bistro", domain.CityAlmaty)
	f := &fakeFacade{items: []domain.EventListItem{it}}

	env := decode(t, do(t, newPublicRouter(f), "/api/v1/events?lang=en"))
	got := env.Data.Items[0]
	if got.Title != "Винный ужин (en)" || got.Restaurant.Name != "Bistro (en)" {
		t.Fatalf("lang=en not applied: title=%q venue=%q", got.Title, got.Restaurant.Name)
	}

	// No translation for kk → the base (ru) text, never an empty string.
	env = decode(t, do(t, newPublicRouter(&fakeFacade{items: []domain.EventListItem{it}}), "/api/v1/events?lang=kk"))
	if env.Data.Items[0].Title != "Винный ужин" {
		t.Fatalf("missing translation must fall back to the base text, got %q", env.Data.Items[0].Title)
	}
}

// The new cross-venue route must coexist with the venue-scoped ones and with
// the ticket routes that already own /events/:eventId/... — gin panics on a
// router conflict at registration time, i.e. at server start in production.
func TestPublicRoutesRegisterWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("route registration panicked (a router conflict): %v", rec)
		}
	}()
	h := NewHandler(nil)
	h.RegisterPublic(api)
	// GET /events lives on RegisterExplore now (the OptionalAuth group in
	// production, bootstrap/app.go) — registered on the SAME engine here to
	// prove the two route sets still coexist without conflict.
	h.RegisterExplore(api)
	// The ticket routes mount on a sibling group of the SAME engine and already
	// own /events/:eventId/tickets — mirrors bootstrap/app.go.
	api.Group("").POST("/events/:eventId/tickets", func(*gin.Context) {})

	want := map[string]bool{
		"GET /api/v1/events":                          false,
		"GET /api/v1/restaurants/:id/events":          false,
		"GET /api/v1/restaurants/:id/events/:eventId": false,
	}
	for _, route := range r.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("route not registered: %s", key)
		}
	}
}

// newDetailRouter mounts the public routes so the event-detail path
// (GET /restaurants/:id/events/:eventId) can be exercised against a canned event.
func newDetailRouter(f uc.Facade) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(f).RegisterPublic(r.Group("/api/v1"))
	return r
}

// detailEnvelope mirrors the standard envelope around a single event (the
// eventResponse shape is embedded in eventListItemResponse, so decoding into the
// item type reads the same fields plus an empty restaurant object).
type detailEnvelope struct {
	Data  eventListItemResponse `json:"data"`
	Error string                `json:"error"`
}

// The «Афиша» chips must arrive on the cross-venue list item, in order.
func TestListUpcoming_CarriesTags(t *testing.T) {
	it := sampleItem(24*time.Hour, "Винный ужин", "Bistro", domain.CityAlmaty)
	f := &fakeFacade{items: []domain.EventListItem{it}}

	env := decode(t, do(t, newPublicRouter(f), "/api/v1/events"))
	got := env.Data.Items[0].Tags
	if len(got) != 2 || got[0] != "Бранч" || got[1] != "Живая музыка" {
		t.Fatalf("list item tags = %#v, want [Бранч Живая музыка]", got)
	}
}

// An event with no chips serializes tags as [] — never null, never absent — so
// the app can render the chip row unconditionally.
func TestListUpcoming_EmptyTagsSerializeAsArray(t *testing.T) {
	it := sampleItem(24*time.Hour, "Без тегов", "Bistro", domain.CityAlmaty)
	it.Tags = nil
	f := &fakeFacade{items: []domain.EventListItem{it}}

	rec := do(t, newPublicRouter(f), "/api/v1/events")
	// Assert on the raw JSON: a decoded []string cannot tell [] from null.
	if !strings.Contains(rec.Body.String(), `"tags":[]`) {
		t.Fatalf("empty tags must serialize as [], body: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"tags":null`) {
		t.Fatalf("tags must never serialize as null, body: %s", rec.Body.String())
	}
}

// The event-detail endpoint carries the chips too, and an event without chips
// still answers tags:[] rather than null.
func TestGetPublic_CarriesTags(t *testing.T) {
	start := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	ev := &domain.Event{
		ID: uuid.New(), RestaurantID: ptrUUID(uuid.New()),
		Title: "Винный ужин", StartsAt: start, EndsAt: start.Add(2 * time.Hour),
		Status: domain.EventPublished, Tags: []string{"Коктейли", "Красивый вид"},
	}
	f := &fakeFacade{event: ev}

	rec := do(t, newDetailRouter(f), "/api/v1/restaurants/"+ev.RestaurantID.String()+"/events/"+ev.ID.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var env detailEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	got := env.Data.Tags
	if len(got) != 2 || got[0] != "Коктейли" || got[1] != "Красивый вид" {
		t.Fatalf("detail tags = %#v, want [Коктейли Красивый вид]", got)
	}

	// Same endpoint, an event with no chips → tags:[] in the raw body.
	ev.Tags = nil
	rec = do(t, newDetailRouter(&fakeFacade{event: ev}), "/api/v1/restaurants/"+ev.RestaurantID.String()+"/events/"+ev.ID.String())
	if !strings.Contains(rec.Body.String(), `"tags":[]`) || strings.Contains(rec.Body.String(), `"tags":null`) {
		t.Fatalf("detail empty tags must be [], not null, body: %s", rec.Body.String())
	}
}

// ptrUUID is the fixture helper for Event.RestaurantID, optional since
// migration 0085 (nil = a platform event, hosted by no venue).
func ptrUUID(id uuid.UUID) *uuid.UUID { return &id }

// --- ?sort=for_you (spec §5.6, criterion 19): OptionalAuth + the match field ---

// Without a token, ?sort=for_you must fall through to the exact same plain
// listing every other guest gets — the ranked usecase method is never even
// called, and no `match` field is added.
func TestListUpcoming_SortForYou_NoToken_FallsBackToPlainListing(t *testing.T) {
	it := sampleItem(24*time.Hour, "Винный ужин", "Bistro", domain.CityAlmaty)
	f := &fakeFacade{items: []domain.EventListItem{it}}

	rec := do(t, newPublicRouter(f), "/api/v1/events?sort=for_you")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if f.forYouCalls != 0 || f.calls != 1 {
		t.Fatalf("forYouCalls=%d calls=%d, want 0/1 (anonymous must use the plain listing)", f.forYouCalls, f.calls)
	}
	if strings.Contains(rec.Body.String(), `"match"`) {
		t.Fatalf("match must be absent for an anonymous caller, body: %s", rec.Body.String())
	}
}

// A signed-in guest who did NOT ask for sort=for_you gets today's plain
// listing too — the ranked path is opt-in, not a blanket "logged in" switch.
func TestListUpcoming_NoSort_WithToken_UsesPlainListing(t *testing.T) {
	it := sampleItem(24*time.Hour, "Винный ужин", "Bistro", domain.CityAlmaty)
	f := &fakeFacade{items: []domain.EventListItem{it}}
	au := middleware.AuthUser{ID: uuid.New(), Role: string(domain.RoleUser)}

	rec := do(t, newExploreRouterWithAuth(f, au), "/api/v1/events")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if f.forYouCalls != 0 || f.calls != 1 {
		t.Fatalf("forYouCalls=%d calls=%d, want 0/1 (no sort= must use the plain listing)", f.forYouCalls, f.calls)
	}
	if strings.Contains(rec.Body.String(), `"match"`) {
		t.Fatalf("match must be absent without sort=for_you, body: %s", rec.Body.String())
	}
}

// An unrecognized sort= value behaves exactly like no sort at all — never an
// error, per listUpcoming's own doc comment.
func TestListUpcoming_UnknownSortValue_UsesPlainListing(t *testing.T) {
	f := &fakeFacade{items: []domain.EventListItem{sampleItem(time.Hour, "X", "V", domain.CityAlmaty)}}
	au := middleware.AuthUser{ID: uuid.New(), Role: string(domain.RoleUser)}

	rec := do(t, newExploreRouterWithAuth(f, au), "/api/v1/events?sort=popular")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if f.forYouCalls != 0 || f.calls != 1 {
		t.Fatalf("forYouCalls=%d calls=%d, want 0/1", f.forYouCalls, f.calls)
	}
}

// The heart of criterion 19's transport half: sort=for_you WITH a token calls
// the ranked usecase method with the caller's own id, and every card in the
// response carries the `match` block it was scored with — the ranking itself
// (score desc, then starts_at, then id) is the usecase's job, proven by
// TestListPublicUpcomingForYou in usecase/events; this only proves the wire.
func TestListUpcoming_SortForYou_WithToken_UsesRankedListingAndAttachesMatch(t *testing.T) {
	winner := sampleItem(48*time.Hour, "Дальше, но вкуснее", "Osteria", domain.CityAlmaty)
	loser := sampleItem(2*time.Hour, "Скоро, но мимо", "Diner", domain.CityAlmaty)
	f := &fakeFacade{ranked: []uc.RankedEventListItem{
		{EventListItem: winner, Match: uc.EventTasteMatch{
			Score: 600,
			Reasons: []domain.TasteMatchReason{
				{Code: domain.TasteSignalCuisineMatch, Points: 400, Params: map[string]any{"cuisine_codes": []string{"italian"}}, Detail: "matches the guest's cuisine preferences"},
				{Code: domain.TasteSignalBudgetMatch, Points: 200, Detail: "matches the guest's budget tier"},
			},
		}},
		{EventListItem: loser, Match: uc.EventTasteMatch{
			Score:   0,
			Reasons: []domain.TasteMatchReason{{Code: domain.TasteSignalCuisineMatch, Points: 0, Detail: "outside the guest's cuisine preferences"}},
		}},
	}}
	au := middleware.AuthUser{ID: uuid.New(), Role: string(domain.RoleUser)}

	rec := do(t, newExploreRouterWithAuth(f, au), "/api/v1/events?sort=for_you&city="+string(domain.CityAlmaty))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if f.forYouCalls != 1 || f.calls != 0 {
		t.Fatalf("forYouCalls=%d calls=%d, want 1/0", f.forYouCalls, f.calls)
	}
	if f.gotUserID != au.ID {
		t.Fatalf("userID passed to the usecase = %s, want %s", f.gotUserID, au.ID)
	}

	env := decode(t, rec)
	if len(env.Data.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(env.Data.Items))
	}
	// The transport must hand the ranked order straight through — it is the
	// usecase's job to have sorted it, not this handler's.
	if env.Data.Items[0].ID != winner.ID.String() || env.Data.Items[1].ID != loser.ID.String() {
		t.Fatalf("order changed by the transport layer: %s, %s", env.Data.Items[0].ID, env.Data.Items[1].ID)
	}
	first := env.Data.Items[0]
	if first.Match == nil {
		t.Fatalf("match missing on a ranked card, body: %s", rec.Body.String())
	}
	if first.Match.Score != 600 || len(first.Match.Reasons) != 2 {
		t.Fatalf("match = %+v, want score 600 with 2 reasons", first.Match)
	}
	if first.Match.Reasons[0].Code != string(domain.TasteSignalCuisineMatch) || first.Match.Reasons[0].Points != 400 {
		t.Fatalf("reasons[0] = %+v, want cuisine_match/400", first.Match.Reasons[0])
	}
	if first.Match.Reasons[0].Params["cuisine_codes"] == nil {
		t.Fatalf("reasons[0].params dropped: %+v", first.Match.Reasons[0])
	}
	// Every scored card carries match, even a zero score — zero is a real
	// answer ("nothing matched"), not "not computed".
	second := env.Data.Items[1]
	if second.Match == nil || second.Match.Score != 0 {
		t.Fatalf("second card match = %+v, want a present zero-score match", second.Match)
	}
}
