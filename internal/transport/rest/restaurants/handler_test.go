package restaurants

import (
	"context"
	"encoding/json"
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
}

func (f *fakeFacade) List(context.Context, domain.RestaurantFilter) ([]domain.RestaurantListItem, int, error) {
	return []domain.RestaurantListItem{f.item}, 1, nil
}
func (f *fakeFacade) Search(context.Context, domain.RestaurantSearchFilter) ([]domain.RestaurantListItem, int, error) {
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
