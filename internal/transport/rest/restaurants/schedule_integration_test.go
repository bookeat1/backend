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
	schedulerepo "backend-core/internal/infrastructure/postgres/schedule"
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

// TestCatalogFiltersEndToEnd runs the two server-side catalog filters through
// the real stack — Postgres rows → repository → facade → JSON. The fake-based
// tests cannot answer the two questions that actually broke in production:
// whether the filtered read still pages correctly over the whole matching set,
// and whether `total` counts the filtered set rather than the SQL-matched one.
func TestCatalogFiltersEndToEnd(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants")

	almaty, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// Friday 2026-07-24, 13:00 Almaty — inside an 11:00–22:00 day, before a
	// 19:00–23:00 one, and 10:00 in Istanbul (before an 11:00 open).
	now := time.Date(2026, 7, 24, 13, 0, 0, 0, almaty)

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

	openBookable := seededVenue{id: uuid.New(), name: "OpenBookable", hours: fullWeek("11:00", "22:00"), tables: table()}
	openNoTables := seededVenue{id: uuid.New(), name: "OpenNoTables", hours: fullWeek("11:00", "22:00")}
	shutBookable := seededVenue{id: uuid.New(), name: "ShutBookable", hours: fullWeek("19:00", "23:00"), tables: table()}
	// No working-hours rows at all — the live catalog has exactly one such
	// venue. "Unknown" must never be counted as open.
	noHours := seededVenue{id: uuid.New(), name: "NoHours", tables: table()}
	// Same instant, two zones apart: 13:00 in Almaty is 11:00 in Istanbul, an
	// hour before this venue opens. Judged against the platform's default zone
	// it would read as OPEN (13:00 is inside 12:00–22:00) — so this venue is
	// the one that fails if the filter ever stops using the venue's own clock.
	istanbul := seededVenue{id: uuid.New(), name: "Istanbul", timezone: "Europe/Istanbul",
		hours: fullWeek("12:00", "22:00"), tables: table()}

	// Open all week, with tables — and closed TODAY by a special day. Special
	// days are authoritative over the weekly rows (ADR-014), and the filter
	// stands on the same domain.IsOpenAt call the payload does, so this venue
	// must fall out of open_now=true. Its ability to take bookings at all is a
	// different question and must not move.
	holidayClosed := seededVenue{id: uuid.New(), name: "HolidayClosed",
		hours: fullWeek("11:00", "22:00"), tables: table()}

	venues := []seededVenue{openBookable, openNoTables, shutBookable, noHours, istanbul, holidayClosed}
	seedVenues(t, pool, venues)

	holidayNote := "Санитарный день"
	if err := schedulerepo.New(pool).Upsert(context.Background(), &domain.ScheduleOverride{
		RestaurantID: holidayClosed.id,
		// The frozen clock's own calendar date, in the venue's zone.
		Date:     domain.StartOfDay(now, almaty),
		IsClosed: true,
		Note:     &holidayNote,
	}); err != nil {
		t.Fatalf("upsert holiday override: %v", err)
	}

	repo := restrepo.New(pool)
	rel := restrepo.NewRelated(pool)
	facade := uc.NewFacade(repo, rel, restrepo.NewCategories(pool), restrepo.NewPartnership(pool),
		sqltx.NewManager(pool),
		uc.WithVenueState(uc.NewVenueState(rel, bookinguc.Config{TimezoneFallback: "Asia/Almaty"},
			uc.WithVenueStateClock(func() time.Time { return now }))))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(facade, nil, nil).RegisterPublic(r.Group("/api/v1"))

	type pageEnvelope struct {
		Items   []publicPayload `json:"items"`
		Total   int             `json:"total"`
		Page    int             `json:"page"`
		PerPage int             `json:"per_page"`
	}
	fetch := func(t *testing.T, path string) pageEnvelope {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, body %s", path, w.Code, w.Body.String())
		}
		var env struct {
			Data pageEnvelope `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return env.Data
	}
	namesOf := func(p pageEnvelope) []string {
		byID := map[string]string{}
		for _, v := range venues {
			byID[v.id.String()] = v.name
		}
		out := make([]string, 0, len(p.Items))
		for _, it := range p.Items {
			out = append(out, byID[it.ID])
		}
		return out
	}
	equal := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	filters := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "unfiltered: the whole catalog, exactly as before",
			query: "",
			want:  []string{"OpenBookable", "OpenNoTables", "ShutBookable", "NoHours", "Istanbul", "HolidayClosed"},
		},
		{
			name:  "open_now=true: the venue with no hours and the one in another zone are out",
			query: "&open_now=true",
			want:  []string{"OpenBookable", "OpenNoTables"},
		},
		{
			name:  "open_now=false: the complement — unknown hours AND the special-day closure",
			query: "&open_now=false",
			want:  []string{"ShutBookable", "NoHours", "Istanbul", "HolidayClosed"},
		},
		{
			name:  "accepts_online_bookings=true: a holiday closure does not make a venue unbookable",
			query: "&accepts_online_bookings=true",
			want:  []string{"OpenBookable", "ShutBookable", "Istanbul", "HolidayClosed"},
		},
		{
			name:  "accepts_online_bookings=false",
			query: "&accepts_online_bookings=false",
			want:  []string{"OpenNoTables", "NoHours"},
		},
		{
			name:  "both, AND-combined",
			query: "&open_now=true&accepts_online_bookings=true",
			want:  []string{"OpenBookable"},
		},
	}

	for _, route := range []string{"/api/v1/restaurants", "/api/v1/restaurants/search"} {
		for _, tc := range filters {
			t.Run(route+" "+tc.name, func(t *testing.T) {
				got := fetch(t, route+"?per_page=50"+tc.query)
				if names := namesOf(got); !equal(names, tc.want) {
					t.Errorf("items = %v, want %v", names, tc.want)
				}
				// The count the guest is shown must describe the filtered set.
				if got.Total != len(tc.want) {
					t.Errorf("total = %d, want %d", got.Total, len(tc.want))
				}
			})
		}
	}

	// Paging over a FILTERED set: the four bookable venues split across two
	// pages of two, and the total stays 4 on every one of them — including the
	// empty third. Before the filter moved to the server this is exactly what
	// could not work: the app counted the page it had.
	t.Run("pagination with a filter applied", func(t *testing.T) {
		for _, route := range []string{"/api/v1/restaurants", "/api/v1/restaurants/search"} {
			first := fetch(t, route+"?accepts_online_bookings=true&per_page=2&page=1")
			if names := namesOf(first); !equal(names, []string{"OpenBookable", "ShutBookable"}) {
				t.Errorf("%s page 1 = %v", route, names)
			}
			if first.Total != 4 {
				t.Errorf("%s page 1 total = %d, want 4", route, first.Total)
			}
			second := fetch(t, route+"?accepts_online_bookings=true&per_page=2&page=2")
			if names := namesOf(second); !equal(names, []string{"Istanbul", "HolidayClosed"}) {
				t.Errorf("%s page 2 = %v", route, names)
			}
			if second.Total != 4 {
				t.Errorf("%s page 2 total = %d, want 4", route, second.Total)
			}
			third := fetch(t, route+"?accepts_online_bookings=true&per_page=2&page=3")
			if len(third.Items) != 0 {
				t.Errorf("%s page 3 = %v, want an empty page", route, namesOf(third))
			}
			if third.Total != 4 {
				t.Errorf("%s page 3 total = %d, want 4", route, third.Total)
			}
		}
	})

	// The special-day closure, stated as its own case so a failure names it.
	t.Run("a venue closed by a special day drops out of open_now=true", func(t *testing.T) {
		for _, route := range []string{"/api/v1/restaurants", "/api/v1/restaurants/search"} {
			openPage := fetch(t, route+"?open_now=true&per_page=50")
			for _, name := range namesOf(openPage) {
				if name == "HolidayClosed" {
					t.Errorf("%s: a venue shut for a holiday was served as open now", route)
				}
			}
			// ...while its weekly rows still say it is open every day, which is
			// what makes this a test of the override and not of the week.
			all := fetch(t, route+"?per_page=50")
			found := false
			for _, it := range all.Items {
				if it.ID == holidayClosed.id.String() {
					found = true
					if it.Schedule == nil || len(it.Schedule.Days) != 7 {
						t.Errorf("%s: expected a full weekly schedule, got %+v", route, it.Schedule)
					}
					if it.AcceptsOnlineBookings == nil || !*it.AcceptsOnlineBookings {
						t.Errorf("%s: a holiday closure must not make the venue unbookable", route)
					}
				}
			}
			if !found {
				t.Errorf("%s: the holiday venue is missing from the unfiltered catalog", route)
			}
		}
	})

	// The SQL-backed filters and the venue-state ones compose: city narrows in
	// SQL first, the venue state is then evaluated over what survived.
	t.Run("combined with an SQL filter", func(t *testing.T) {
		got := fetch(t, "/api/v1/restaurants?city=Алматы&open_now=true&per_page=50")
		if names := namesOf(got); !equal(names, []string{"OpenBookable", "OpenNoTables"}) {
			t.Errorf("items = %v", names)
		}
		if got.Total != 2 {
			t.Errorf("total = %d, want 2", got.Total)
		}
		// A city nothing is seeded in must return nothing, filter or no filter.
		empty := fetch(t, "/api/v1/restaurants?city=Астана&open_now=true&per_page=50")
		if empty.Total != 0 || len(empty.Items) != 0 {
			t.Errorf("other city: total = %d, items = %v", empty.Total, namesOf(empty))
		}
	})
}
