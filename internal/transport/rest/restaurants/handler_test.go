package restaurants

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/restaurants"
)

// fakeFacade serves one canned venue on both the listing and the detail route,
// so a test can assert the two payloads carry the SAME new fields.
type fakeFacade struct {
	item domain.RestaurantListItem
	agg  *domain.RestaurantAggregate

	// gotVenueFilter is the last domain.VenueStateFilter the handler passed
	// down, so a transport test can prove the query string was actually read
	// (before this feature the two parameters were parsed nowhere and the
	// server answered every catalog query with the whole catalog).
	gotVenueFilter domain.VenueStateFilter
	gotFilter      domain.RestaurantFilter
	gotSearch      domain.RestaurantSearchFilter
	err            error
}

func (f *fakeFacade) List(_ context.Context, flt domain.RestaurantFilter, vs domain.VenueStateFilter) ([]domain.RestaurantListItem, int, error) {
	f.gotFilter, f.gotVenueFilter = flt, vs
	if f.err != nil {
		return nil, 0, f.err
	}
	return []domain.RestaurantListItem{f.item}, 1, nil
}
func (f *fakeFacade) Search(_ context.Context, flt domain.RestaurantSearchFilter, vs domain.VenueStateFilter) ([]domain.RestaurantListItem, int, error) {
	f.gotSearch, f.gotVenueFilter = flt, vs
	if f.err != nil {
		return nil, 0, f.err
	}
	return []domain.RestaurantListItem{f.item}, 1, nil
}
func (f *fakeFacade) Get(context.Context, uuid.UUID) (*domain.RestaurantAggregate, error) {
	return f.agg, nil
}
func (f *fakeFacade) Categories(context.Context) ([]domain.RestaurantCategory, error) {
	return nil, nil
}
func (f *fakeFacade) Create(context.Context, uc.SaveInput) (*domain.RestaurantAggregate, error) {
	return nil, domain.ErrForbidden
}
func (f *fakeFacade) Update(context.Context, uuid.UUID, uc.SaveInput) (*domain.RestaurantAggregate, error) {
	return nil, domain.ErrForbidden
}
func (f *fakeFacade) SetActive(context.Context, uuid.UUID, bool) error             { return nil }
func (f *fakeFacade) SubmitPartnership(context.Context, uc.PartnershipInput) error { return nil }

// publicPayload is the slice of the JSON this feature is about, plus
// opening_hours to prove the legacy free-text field is untouched.
type publicPayload struct {
	ID           string `json:"id"`
	OpeningHours string `json:"opening_hours"`
	Schedule     *struct {
		Timezone string `json:"timezone"`
		OpenNow  *bool  `json:"open_now"`
		Days     []struct {
			DayOfWeek     int    `json:"day_of_week"`
			IsOpen        bool   `json:"is_open"`
			OpensAt       string `json:"opens_at"`
			ClosesAt      string `json:"closes_at"`
			ClosesNextDay bool   `json:"closes_next_day"`
		} `json:"days"`
		Exceptions []struct {
			Date          string `json:"date"`
			IsOpen        bool   `json:"is_open"`
			OpensAt       string `json:"opens_at"`
			ClosesAt      string `json:"closes_at"`
			ClosesNextDay bool   `json:"closes_next_day"`
			Note          string `json:"note"`
		} `json:"exceptions"`
		ExceptionsFrom  string `json:"exceptions_from"`
		ExceptionsUntil string `json:"exceptions_until"`
	} `json:"schedule"`
	AcceptsOnlineBookings *bool `json:"accepts_online_bookings"`
	PriceRange            *struct {
		Min int `json:"min"`
		Max int `json:"max"`
	} `json:"price_range"`
}

func newTestRouter(f uc.Facade) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(f, nil, nil).RegisterPublic(r.Group("/api/v1"))
	return r
}

func doGET(t *testing.T, r *gin.Engine, path string) map[string]json.RawMessage {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, body %s", path, w.Code, w.Body.String())
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, w.Body.String())
	}
	return env
}

// listPayload pulls the single item out of the paginated listing envelope.
func listPayload(t *testing.T, r *gin.Engine, path string) publicPayload {
	t.Helper()
	env := doGET(t, r, path)
	var page struct {
		Items []publicPayload `json:"items"`
	}
	if err := json.Unmarshal(env["data"], &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	return page.Items[0]
}

func detailPayload(t *testing.T, r *gin.Engine, path string) publicPayload {
	t.Helper()
	env := doGET(t, r, path)
	var p publicPayload
	if err := json.Unmarshal(env["data"], &p); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	return p
}

func openNow(v bool) *bool { return &v }

// activeVenue is a live catalog row shaped like the common
// "Пн, Чт, Вс 11:00 — 22:00  Пт - Сб 11:00 — 01:00" venue. IsActive matters:
// the public detail route 404s a deactivated venue before it ever serialises.
func activeVenue(id uuid.UUID) domain.Restaurant {
	return domain.Restaurant{
		ID:           id,
		IsActive:     true,
		OpeningHours: "Пн, Чт, Вс 11:00 — 22:00  Пт - Сб 11:00 — 01:00",
	}
}

func TestPublicPayloadCarriesScheduleAndBookability(t *testing.T) {
	id := uuid.New()
	st := &domain.PublicVenueState{
		AcceptsOnlineBookings: true,
		Schedule: &domain.WeeklySchedule{
			Timezone: "Asia/Almaty",
			OpenNow:  openNow(true),
			Days: []domain.ScheduleDay{
				{DayOfWeek: 0},
				{DayOfWeek: 5, IsOpen: true, OpenTime: "11:00", CloseTime: "01:00", ClosesNextDay: true},
			},
		},
	}
	rest := activeVenue(id)
	f := &fakeFacade{
		item: domain.RestaurantListItem{Restaurant: rest, VenueState: st},
		agg:  &domain.RestaurantAggregate{Restaurant: rest, VenueState: st},
	}
	r := newTestRouter(f)

	for _, tc := range []struct {
		name string
		got  publicPayload
	}{
		{"list", listPayload(t, r, "/api/v1/restaurants")},
		{"search", listPayload(t, r, "/api/v1/restaurants/search?q=x")},
		{"detail", detailPayload(t, r, "/api/v1/restaurants/"+id.String())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.got
			if p.OpeningHours == "" {
				t.Error("the legacy free-text opening_hours must stay in the payload")
			}
			if p.AcceptsOnlineBookings == nil || !*p.AcceptsOnlineBookings {
				t.Fatalf("accepts_online_bookings = %v, want true", p.AcceptsOnlineBookings)
			}
			if p.Schedule == nil {
				t.Fatal("schedule missing from the payload")
			}
			if p.Schedule.Timezone != "Asia/Almaty" {
				t.Errorf("timezone = %q, want Asia/Almaty", p.Schedule.Timezone)
			}
			if p.Schedule.OpenNow == nil || !*p.Schedule.OpenNow {
				t.Errorf("open_now = %v, want true", p.Schedule.OpenNow)
			}
			if len(p.Schedule.Days) != 2 {
				t.Fatalf("days = %d, want 2", len(p.Schedule.Days))
			}
			if d := p.Schedule.Days[0]; d.DayOfWeek != 0 || d.IsOpen || d.OpensAt != "" {
				t.Errorf("closed day = %+v, want day 0 closed with no times", d)
			}
			if d := p.Schedule.Days[1]; !d.IsOpen || d.OpensAt != "11:00" || d.ClosesAt != "01:00" || !d.ClosesNextDay {
				t.Errorf("past-midnight day = %+v", d)
			}
		})
	}
}

// A venue with no working-hours rows must serve NO schedule (so the client says
// "unknown"), while still stating that it cannot be booked.
func TestPublicPayloadOmitsScheduleWhenHoursUnknown(t *testing.T) {
	id := uuid.New()
	st := &domain.PublicVenueState{AcceptsOnlineBookings: false}
	rest := activeVenue(id)
	f := &fakeFacade{
		item: domain.RestaurantListItem{Restaurant: rest, VenueState: st},
		agg:  &domain.RestaurantAggregate{Restaurant: rest, VenueState: st},
	}
	r := newTestRouter(f)

	for name, p := range map[string]publicPayload{
		"list":   listPayload(t, r, "/api/v1/restaurants"),
		"detail": detailPayload(t, r, "/api/v1/restaurants/"+id.String()),
	} {
		if p.Schedule != nil {
			t.Errorf("%s: schedule must be absent when the hours are unknown, got %+v", name, p.Schedule)
		}
		if p.AcceptsOnlineBookings == nil || *p.AcceptsOnlineBookings {
			t.Errorf("%s: accepts_online_bookings = %v, want an explicit false", name, p.AcceptsOnlineBookings)
		}
		if p.OpeningHours == "" {
			t.Errorf("%s: opening_hours must survive untouched", name)
		}
	}
}

// Both fields disappear entirely when the server did not compute them, so no
// client can mistake "not computed" for "closed / not bookable".
func TestPublicPayloadOmitsBothFieldsWhenNotComputed(t *testing.T) {
	id := uuid.New()
	rest := activeVenue(id)
	f := &fakeFacade{
		item: domain.RestaurantListItem{Restaurant: rest},
		agg:  &domain.RestaurantAggregate{Restaurant: rest},
	}
	r := newTestRouter(f)

	env := doGET(t, r, "/api/v1/restaurants/"+id.String())
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(env["data"], &raw); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if _, ok := raw["schedule"]; ok {
		t.Error("schedule must be omitted when not computed")
	}
	if _, ok := raw["accepts_online_bookings"]; ok {
		t.Error("accepts_online_bookings must be omitted when not computed")
	}
	if _, ok := raw["opening_hours"]; !ok {
		t.Error("opening_hours must always be present")
	}
}

// TestPublicPayloadCarriesPriceRange proves the numeric average-check range is
// serialized as a nested {min,max} object on BOTH the listing/search items and
// the detail payload when the venue has declared both bounds.
func TestPublicPayloadCarriesPriceRange(t *testing.T) {
	id := uuid.New()
	rest := activeVenue(id)
	min, max := 8000, 15000
	rest.PriceMin, rest.PriceMax = &min, &max
	f := &fakeFacade{
		item: domain.RestaurantListItem{Restaurant: rest},
		agg:  &domain.RestaurantAggregate{Restaurant: rest},
	}
	r := newTestRouter(f)

	for _, tc := range []struct {
		name string
		got  publicPayload
	}{
		{"list", listPayload(t, r, "/api/v1/restaurants")},
		{"search", listPayload(t, r, "/api/v1/restaurants/search?q=x")},
		{"detail", detailPayload(t, r, "/api/v1/restaurants/"+id.String())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got.PriceRange == nil {
				t.Fatal("price_range missing from the payload")
			}
			if tc.got.PriceRange.Min != 8000 || tc.got.PriceRange.Max != 15000 {
				t.Errorf("price_range = %+v, want {8000 15000}", *tc.got.PriceRange)
			}
		})
	}
}

// TestPublicPayloadOmitsPriceRangeWhenUnset proves the object disappears
// entirely (not a 0–0) when the venue has declared no range, on both routes.
func TestPublicPayloadOmitsPriceRangeWhenUnset(t *testing.T) {
	id := uuid.New()
	rest := activeVenue(id) // PriceMin/PriceMax left nil
	f := &fakeFacade{
		item: domain.RestaurantListItem{Restaurant: rest},
		agg:  &domain.RestaurantAggregate{Restaurant: rest},
	}
	r := newTestRouter(f)

	// detail: assert the key is absent from the raw object, not merely nil.
	env := doGET(t, r, "/api/v1/restaurants/"+id.String())
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(env["data"], &raw); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if _, ok := raw["price_range"]; ok {
		t.Error("price_range must be omitted when the venue has no range")
	}

	// list item: same absence assertion on the paginated envelope's item.
	listEnv := doGET(t, r, "/api/v1/restaurants")
	var page struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(listEnv["data"], &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	if _, ok := page.Items[0]["price_range"]; ok {
		t.Error("price_range must be omitted from list items when the venue has no range")
	}
}

// The two catalog filters must actually reach the usecase. They used to reach
// nothing at all: the server parsed neither, answered every query with the whole
// catalog, and the app filtered on the client over one page.
func TestCatalogFiltersReachTheUsecase(t *testing.T) {
	tests := []struct {
		name          string
		query         string
		wantOpen      *bool
		wantBookable  *bool
		wantActiveMsg string
	}{
		{name: "neither parameter", query: ""},
		{name: "open_now=true", query: "?open_now=true", wantOpen: openNow(true)},
		{name: "open_now=false", query: "?open_now=false", wantOpen: openNow(false)},
		{name: "open_now=1 (ParseBool vocabulary, same as is_popular)", query: "?open_now=1", wantOpen: openNow(true)},
		{
			name: "accepts_online_bookings=true", query: "?accepts_online_bookings=true",
			wantBookable: openNow(true),
		},
		{
			name:  "both at once",
			query: "?open_now=true&accepts_online_bookings=false",
			// The "только по телефону, и сейчас открыто" combination.
			wantOpen: openNow(true), wantBookable: openNow(false),
		},
		{
			name: "garbage is ignored, exactly like is_popular=maybe",
			// Not a silent 500 and not a filtered answer: an unparseable value
			// leaves the filter absent, which is the existing convention.
			query: "?open_now=maybe",
		},
		{name: "empty value is no filter", query: "?open_now="},
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
				assertBoolFilter(t, "open_now", f.gotVenueFilter.OpenNow, tc.wantOpen)
				assertBoolFilter(t, "accepts_online_bookings", f.gotVenueFilter.AcceptsOnlineBookings, tc.wantBookable)
			})
		}
	}
}

func assertBoolFilter(t *testing.T, name string, got, want *bool) {
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

// A filtered query the server cannot evaluate must NOT answer 200 with the
// unfiltered catalog. 503 + the machine-readable code, so the client knows the
// list it got is not filtered.
func TestCatalogFilterUnavailableIs503WithCode(t *testing.T) {
	f := &fakeFacade{err: domain.WithCode(domain.CodeCatalogVenueStateUnavailable,
		fmt.Errorf("%w: venue state could not be computed", domain.ErrUnavailable))}
	r := newTestRouter(f)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/restaurants?open_now=true", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", w.Code, w.Body.String())
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != string(domain.CodeCatalogVenueStateUnavailable) {
		t.Errorf("code = %q, want %q", env.Code, domain.CodeCatalogVenueStateUnavailable)
	}
}

// per_page is echoed back capped, so `pages` (computed from total/per_page) and
// the number of items actually served describe the same page size.
func TestListEchoesNormalizedPaging(t *testing.T) {
	f := &fakeFacade{item: domain.RestaurantListItem{Restaurant: activeVenue(uuid.New())}}
	r := newTestRouter(f)

	env := doGET(t, r, "/api/v1/restaurants?page=0&per_page=1000")
	var page struct {
		Page    int `json:"page"`
		PerPage int `json:"per_page"`
	}
	if err := json.Unmarshal(env["data"], &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if page.Page != 1 {
		t.Errorf("page = %d, want 1", page.Page)
	}
	if page.PerPage != domain.MaxPerPage {
		t.Errorf("per_page = %d, want the cap %d", page.PerPage, domain.MaxPerPage)
	}
}
