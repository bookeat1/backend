package restaurants

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
	bookinguc "backend-core/internal/usecase/bookings"
	uc "backend-core/internal/usecase/restaurants"
)

// This file answers the question the fake-based tests above cannot: does the
// REAL stack — Postgres rows → repository → facade → JSON — tell the guest the
// truth about a venue's hours and bookability?
//
// It runs over the real restaurant repositories and the real venue-state
// enrichment, with the timezone resolved through the SAME booking Config the
// availability engine uses. Only the clock is frozen, so "open now" is
// assertable at all.

type seededVenue struct {
	id       uuid.UUID
	name     string
	timezone string
	hours    []domain.WorkingHours
	tables   []domain.RestaurantTable
}

func openDay(dow int, open, close_ string) domain.WorkingHours {
	return domain.WorkingHours{DayOfWeek: dow, IsOpen: true, OpenTime: &open, CloseTime: &close_}
}

func closedDay(dow int) domain.WorkingHours {
	return domain.WorkingHours{DayOfWeek: dow, IsOpen: false}
}

func seedVenues(t *testing.T, pool *pgxpool.Pool, venues []seededVenue) {
	t.Helper()
	ctx := context.Background()
	repo := restrepo.New(pool)
	rel := restrepo.NewRelated(pool)
	txm := sqltx.NewManager(pool)

	for i, v := range venues {
		order := i
		if err := repo.Create(ctx, &domain.Restaurant{
			ID: v.id, Name: v.name, City: domain.CityAlmaty, PriceCategory: domain.PriceMid,
			IsActive: true, DisplayOrder: &order,
			OpeningHours: "Пн, Чт, Вс 11:00 — 22:00  Пт - Сб 11:00 — 01:00",
		}); err != nil {
			t.Fatalf("create %s: %v", v.name, err)
		}
		if v.timezone != "" {
			if _, err := pool.Exec(ctx, `UPDATE restaurants SET timezone=$2 WHERE id=$1`, v.id, v.timezone); err != nil {
				t.Fatalf("set timezone for %s: %v", v.name, err)
			}
		}
		err := txm.WithinTx(ctx, func(ctx context.Context) error {
			if len(v.hours) > 0 {
				if err := rel.ReplaceWorkingHours(ctx, v.id, v.hours); err != nil {
					return err
				}
			}
			if len(v.tables) > 0 {
				return rel.ReplaceTables(ctx, v.id, v.tables)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("seed related for %s: %v", v.name, err)
		}
	}
}

func TestPublicScheduleEndToEnd(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants")

	almaty, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	// Saturday 2026-07-25, 00:30 Almaty — inside Friday's 11:00–01:00 tail.
	now := time.Date(2026, 7, 25, 0, 30, 0, 0, almaty)

	table := func() []domain.RestaurantTable {
		return []domain.RestaurantTable{{Name: "T1", Capacity: 4, IsActive: true}}
	}
	fullWeek := func(open, close_ string) []domain.WorkingHours {
		out := make([]domain.WorkingHours, 0, 7)
		for dow := 0; dow < 7; dow++ {
			out = append(out, openDay(dow, open, close_))
		}
		return out
	}

	overnight := seededVenue{
		id: uuid.New(), name: "Overnight", hours: fullWeek("11:00", "01:00"), tables: table(),
	}
	closedToday := seededVenue{
		id: uuid.New(), name: "ClosedToday", tables: table(),
		// Friday shuts at 22:00 and Saturday is a day off, so 00:30 Sat is shut.
		hours: []domain.WorkingHours{
			openDay(5, "11:00", "22:00"), closedDay(6),
			openDay(0, "11:00", "22:00"), openDay(1, "11:00", "22:00"),
			openDay(2, "11:00", "22:00"), openDay(3, "11:00", "22:00"),
			openDay(4, "11:00", "22:00"),
		},
	}
	noHours := seededVenue{id: uuid.New(), name: "NoHours", tables: table()}
	noTables := seededVenue{
		id: uuid.New(), name: "Adept", hours: fullWeek("11:00", "22:00"),
	}
	// Same wall-clock instant, a zone two hours behind: 00:30 Almaty is 22:30
	// the previous evening in Istanbul, i.e. still inside Friday's 11:00–01:00.
	otherZone := seededVenue{
		id: uuid.New(), name: "Istanbul", timezone: "Europe/Istanbul",
		hours: fullWeek("11:00", "01:00"), tables: table(),
	}
	venues := []seededVenue{overnight, closedToday, noHours, noTables, otherZone}
	seedVenues(t, pool, venues)

	repo := restrepo.New(pool)
	rel := restrepo.NewRelated(pool)
	facade := uc.NewFacade(repo, rel, restrepo.NewCategories(pool), restrepo.NewPartnership(pool),
		sqltx.NewManager(pool),
		uc.WithVenueState(uc.NewVenueState(rel, bookinguc.Config{TimezoneFallback: "Asia/Almaty"},
			uc.WithVenueStateClock(func() time.Time { return now }))))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(facade, nil, nil).RegisterPublic(r.Group("/api/v1"))

	// ---- the LIST-shaped routes -------------------------------------------
	// Every route that serves catalog rows is exercised. A third list added
	// later must be added here too, or it will silently ship without the
	// fields — which is exactly how favorites first shipped without them.
	fetchList := func(path string) map[string]publicPayload {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, body %s", path, w.Code, w.Body.String())
		}
		var env struct {
			Data struct {
				Items []publicPayload `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		out := make(map[string]publicPayload, len(env.Data.Items))
		for _, it := range env.Data.Items {
			out[it.ID] = it
		}
		if len(out) != len(venues) {
			t.Fatalf("%s returned %d venues, want %d", path, len(out), len(venues))
		}
		return out
	}
	listRoutes := map[string]map[string]publicPayload{
		"list":   fetchList("/api/v1/restaurants?per_page=50"),
		"search": fetchList("/api/v1/restaurants/search?per_page=50"),
	}

	tests := []struct {
		name        string
		venue       seededVenue
		wantSched   bool
		wantOpenNow bool
		wantTZ      string
		wantBooking bool
	}{
		{"open past midnight reads as open at 00:30", overnight, true, true, "Asia/Almaty", true},
		{"closed today reads as shut", closedToday, true, false, "Asia/Almaty", true},
		{"no working-hours rows: schedule unknown", noHours, false, false, "", false},
		{"no tables: schedule known, bookings refused", noTables, true, false, "Asia/Almaty", false},
		{"venue timezone wins over the platform fallback", otherZone, true, true, "Europe/Istanbul", true},
	}
	for route, byID := range listRoutes {
		for _, tc := range tests {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				assertVenuePayload(t, byID[tc.venue.id.String()], tc.wantSched, tc.wantOpenNow, tc.wantTZ, tc.wantBooking)
			})
		}
	}

	// ---- detail: the same facts must come back on GET /restaurants/:id ----
	for _, tc := range tests {
		t.Run("detail/"+tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/"+tc.venue.id.String(), nil))
			if w.Code != http.StatusOK {
				t.Fatalf("detail = %d, body %s", w.Code, w.Body.String())
			}
			var env struct {
				Data publicPayload `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode detail: %v", err)
			}
			assertVenuePayload(t, env.Data, tc.wantSched, tc.wantOpenNow, tc.wantTZ, tc.wantBooking)
		})
	}

	// The past-midnight venue must flip to closed once its window really ends:
	// 01:00 is the exclusive end of Friday's window and Saturday opens at 11:00.
	afterClose := uc.NewFacade(repo, rel, restrepo.NewCategories(pool), restrepo.NewPartnership(pool),
		sqltx.NewManager(pool),
		uc.WithVenueState(uc.NewVenueState(rel, bookinguc.Config{TimezoneFallback: "Asia/Almaty"},
			uc.WithVenueStateClock(func() time.Time { return time.Date(2026, 7, 25, 2, 0, 0, 0, almaty) }))))
	agg, err := afterClose.Get(context.Background(), overnight.id)
	if err != nil {
		t.Fatalf("get after close: %v", err)
	}
	if agg.VenueState == nil || agg.VenueState.Schedule == nil || agg.VenueState.Schedule.OpenNow == nil {
		t.Fatalf("expected a computed open_now, got %+v", agg.VenueState)
	}
	if *agg.VenueState.Schedule.OpenNow {
		t.Error("venue must read as closed at 02:00, after its 01:00 close")
	}
}

func assertVenuePayload(t *testing.T, p publicPayload, wantSched, wantOpenNow bool, wantTZ string, wantBooking bool) {
	t.Helper()
	if p.OpeningHours == "" {
		t.Error("the legacy free-text opening_hours must still be served")
	}
	if p.AcceptsOnlineBookings == nil {
		t.Fatal("accepts_online_bookings must be present")
	}
	if *p.AcceptsOnlineBookings != wantBooking {
		t.Errorf("accepts_online_bookings = %v, want %v", *p.AcceptsOnlineBookings, wantBooking)
	}
	if !wantSched {
		if p.Schedule != nil {
			t.Errorf("schedule must be absent (unknown), got %+v", p.Schedule)
		}
		return
	}
	if p.Schedule == nil {
		t.Fatal("schedule missing")
	}
	if p.Schedule.Timezone != wantTZ {
		t.Errorf("timezone = %q, want %q", p.Schedule.Timezone, wantTZ)
	}
	if p.Schedule.OpenNow == nil {
		t.Fatal("open_now missing")
	}
	if *p.Schedule.OpenNow != wantOpenNow {
		t.Errorf("open_now = %v, want %v", *p.Schedule.OpenNow, wantOpenNow)
	}
	if len(p.Schedule.Days) == 0 {
		t.Error("a known schedule must carry days")
	}
}
