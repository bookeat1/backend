package favorites

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	favoriterepo "backend-core/internal/infrastructure/postgres/favorite"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/transport/rest/middleware"
	restaurantsrest "backend-core/internal/transport/rest/restaurants"
	bookinguc "backend-core/internal/usecase/bookings"
	favoritesuc "backend-core/internal/usecase/favorites"
	restaurantsuc "backend-core/internal/usecase/restaurants"
)

// The guest sees the SAME venue on three screens — catalog, search, favorites —
// all rendered from the same public shape. They must therefore carry the same
// schedule and the same bookability flag.
//
// This is not hypothetical: the enrichment first shipped on the catalog facade
// only, and favorites read the repository directly, so every favorited venue
// came back with no `schedule` at all. A client cannot tell that apart from "no
// hours recorded" and would have shown "часы неизвестны" next to a venue whose
// full week it had just rendered on the home screen.
//
// If someone adds a FOURTH list of restaurants, add it to this test — that is
// cheaper than finding the inconsistency in the app.

// ---- auth plumbing (mirrors the admin/bookings handler tests) --------------

type fakeIssuer struct{}

func (fakeIssuer) IssueAccess(id uuid.UUID, role string) (string, time.Time, error) {
	return id.String(), time.Now().Add(time.Hour), nil
}
func (fakeIssuer) ParseAccess(token string) (uuid.UUID, string, error) {
	id, err := uuid.Parse(token)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("bad token")
	}
	return id, "", nil
}

// publicPayload is the slice of the catalog JSON this test compares across
// screens.
type publicPayload struct {
	ID       string `json:"id"`
	Schedule *struct {
		Timezone string `json:"timezone"`
		OpenNow  *bool  `json:"open_now"`
		Days     []struct {
			DayOfWeek     int    `json:"day_of_week"`
			IsOpen        bool   `json:"is_open"`
			OpensAt       string `json:"opens_at"`
			ClosesAt      string `json:"closes_at"`
			ClosesNextDay bool   `json:"closes_next_day"`
		} `json:"days"`
	} `json:"schedule"`
	AcceptsOnlineBookings *bool `json:"accepts_online_bookings"`
}

func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	email := fmt.Sprintf("fav-%s@example.com", uuid.NewString())
	u := &domain.User{
		ID: uuid.New(), Email: &email, FullName: "Fav",
		Role: domain.RoleUser, IsActive: true,
	}
	if err := userrepo.New(pool).Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u.ID
}

func TestFavoritesMatchCatalogAndSearch(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "users")
	ctx := context.Background()

	almaty, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// Saturday 2026-07-25, 00:30 Almaty — inside Friday's 11:00–01:00 tail, so
	// open_now is a non-trivial true that a naive implementation gets wrong.
	now := time.Date(2026, 7, 25, 0, 30, 0, 0, almaty)

	repo := restrepo.New(pool)
	rel := restrepo.NewRelated(pool)
	txm := sqltx.NewManager(pool)

	rid := uuid.New()
	order := 0
	if err := repo.Create(ctx, &domain.Restaurant{
		ID: rid, Name: "Del Papa", City: domain.CityAlmaty, PriceCategory: domain.PriceMid,
		IsActive: true, DisplayOrder: &order,
		OpeningHours: "Пн, Чт, Вс 11:00 — 22:00  Пт - Сб 11:00 — 01:00",
	}); err != nil {
		t.Fatalf("create restaurant: %v", err)
	}
	open, close_ := "11:00", "01:00"
	hours := make([]domain.WorkingHours, 0, 7)
	for dow := 0; dow < 7; dow++ {
		o, c := open, close_
		hours = append(hours, domain.WorkingHours{DayOfWeek: dow, IsOpen: true, OpenTime: &o, CloseTime: &c})
	}
	err = txm.WithinTx(ctx, func(ctx context.Context) error {
		if err := rel.ReplaceWorkingHours(ctx, rid, hours); err != nil {
			return err
		}
		return rel.ReplaceTables(ctx, rid, []domain.RestaurantTable{{Name: "T1", Capacity: 4, IsActive: true}})
	})
	if err != nil {
		t.Fatalf("seed related: %v", err)
	}

	uid := seedUser(t, pool)
	favRepo := favoriterepo.New(pool)
	if err := favRepo.Add(ctx, uid, rid); err != nil {
		t.Fatalf("add favorite: %v", err)
	}

	// ONE shared enricher, exactly like bootstrap/deps.go wires it.
	venueState := restaurantsuc.NewVenueState(rel, bookinguc.Config{TimezoneFallback: "Asia/Almaty"},
		restaurantsuc.WithVenueStateClock(func() time.Time { return now }))
	catalog := restaurantsuc.NewFacade(repo, rel, restrepo.NewCategories(pool),
		restrepo.NewPartnership(pool), txm, restaurantsuc.WithVenueState(venueState))
	favs := favoritesuc.NewFacade(favRepo, favoritesuc.WithVenueState(venueState))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	restaurantsrest.NewHandler(catalog, nil, nil).RegisterPublic(r.Group("/api/v1"))
	authed := r.Group("/api/v1")
	authed.Use(middleware.Auth(fakeIssuer{}, userrepo.New(pool)))
	NewHandler(favs).RegisterRoutes(authed)

	get := func(path string, authAs *uuid.UUID) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if authAs != nil {
			req.Header.Set("Authorization", "Bearer "+authAs.String())
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, body %s", path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	firstOfList := func(body []byte) publicPayload {
		t.Helper()
		var env struct {
			Data struct {
				Items []publicPayload `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("decode list: %v (%s)", err, body)
		}
		if len(env.Data.Items) != 1 {
			t.Fatalf("items = %d, want 1", len(env.Data.Items))
		}
		return env.Data.Items[0]
	}

	fromCatalog := firstOfList(get("/api/v1/restaurants", nil))
	fromSearch := firstOfList(get("/api/v1/restaurants/search?q=Papa", nil))

	// The favorites route answers with a bare array, not a paginated page.
	var favEnv struct {
		Data []publicPayload `json:"data"`
	}
	favBody := get("/api/v1/favorites", &uid)
	if err := json.Unmarshal(favBody, &favEnv); err != nil {
		t.Fatalf("decode favorites: %v (%s)", err, favBody)
	}
	if len(favEnv.Data) != 1 {
		t.Fatalf("favorites = %d items, want 1 (%s)", len(favEnv.Data), favBody)
	}
	fromFavorites := favEnv.Data[0]

	// The catalog answer is the reference: it must be a real, non-trivial one,
	// or "all three agree" would be satisfied by three empty payloads.
	if fromCatalog.Schedule == nil || fromCatalog.Schedule.OpenNow == nil || !*fromCatalog.Schedule.OpenNow {
		t.Fatalf("catalog reference payload is not the expected open-past-midnight venue: %+v", fromCatalog.Schedule)
	}
	if fromCatalog.AcceptsOnlineBookings == nil || !*fromCatalog.AcceptsOnlineBookings {
		t.Fatalf("catalog reference must be bookable, got %v", fromCatalog.AcceptsOnlineBookings)
	}
	if len(fromCatalog.Schedule.Days) != 7 {
		t.Fatalf("catalog days = %d, want 7", len(fromCatalog.Schedule.Days))
	}

	for _, tc := range []struct {
		screen string
		got    publicPayload
	}{
		{"search", fromSearch},
		{"favorites", fromFavorites},
	} {
		t.Run(tc.screen+" matches the catalog", func(t *testing.T) {
			if tc.got.ID != fromCatalog.ID {
				t.Fatalf("id = %s, want %s", tc.got.ID, fromCatalog.ID)
			}
			want, err := json.Marshal(fromCatalog.Schedule)
			if err != nil {
				t.Fatalf("marshal reference schedule: %v", err)
			}
			got, err := json.Marshal(tc.got.Schedule)
			if err != nil {
				t.Fatalf("marshal schedule: %v", err)
			}
			if string(got) != string(want) {
				t.Errorf("schedule differs from the catalog\n got: %s\nwant: %s", got, want)
			}
			if tc.got.AcceptsOnlineBookings == nil {
				t.Fatal("accepts_online_bookings missing")
			}
			if *tc.got.AcceptsOnlineBookings != *fromCatalog.AcceptsOnlineBookings {
				t.Errorf("accepts_online_bookings = %v, want %v",
					*tc.got.AcceptsOnlineBookings, *fromCatalog.AcceptsOnlineBookings)
			}
		})
	}
}
