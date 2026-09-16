package tastematch

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	cuisinerepo "backend-core/internal/infrastructure/postgres/cuisine"
	"backend-core/internal/infrastructure/postgres/foodieprofile"
	restaurantrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	"backend-core/internal/infrastructure/sqltx"
)

// loaderTables lists every table these tests own, children first. Truncated
// before each test so one test's leftovers cannot pass another.
var loaderTables = []string{
	"user_foodie_cuisines", "user_foodie_diets", "user_foodie_allergies",
	"bookings", "restaurant_cuisines", "cuisine_aliases", "cuisines",
	"restaurants", "users",
}

func setup(t *testing.T) (Loader, sqltx.Querier, context.Context) {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, loaderTables...)
	l := NewLoader(foodieprofile.New(pool), userrepo.New(pool), cuisinerepo.New(pool), bookingrepo.New(pool))
	ctx := context.Background()
	return l, pool, ctx
}

func seedUser(ctx context.Context, t *testing.T, pool sqltx.Querier, budget *string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	u := &domain.User{ID: id, FullName: "Guest", Role: domain.RoleUser, PreferredLanguage: "ru", FoodieBudgetTier: budget}
	if err := userrepo.New(pool).Create(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func seedCuisine(ctx context.Context, t *testing.T, pool sqltx.Querier, code string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO cuisines (id, code, name) VALUES ($1,$2,$3)`, id, code, code); err != nil {
		t.Fatalf("seed cuisine %s: %v", code, err)
	}
	return id
}

func seedRestaurant(ctx context.Context, t *testing.T, pool sqltx.Querier, name string, cuisineIDs ...uuid.UUID) uuid.UUID {
	t.Helper()
	r := &domain.Restaurant{ID: uuid.New(), Name: name, City: domain.CityAlmaty, PriceCategory: domain.PriceMid, IsActive: true}
	if err := restaurantrepo.New(pool).Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	for i, cid := range cuisineIDs {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurant_cuisines (restaurant_id, cuisine_id, position) VALUES ($1,$2,$3)`,
			r.ID, cid, i); err != nil {
			t.Fatalf("link cuisine: %v", err)
		}
	}
	return r.ID
}

func seedBooking(ctx context.Context, t *testing.T, pool sqltx.Querier, userID, restaurantID uuid.UUID, status domain.BookingStatus, startsAt time.Time) {
	t.Helper()
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: restaurantID, UserID: &userID,
		Name: "Guest", Phone: "+7 (777) 000-00-00", Email: "guest@example.com",
		PhoneNormalized: "+77770000000", Guests: 2,
		StartsAt: startsAt, EndsAt: startsAt.Add(2 * time.Hour),
		Status: status, Source: domain.SourceApp,
	}
	if err := bookingrepo.New(pool).Create(ctx, b); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
}

// TestLoadTasteProfile_ExplicitCuisines: a guest who filled the wizard's
// cuisine step never needs booking history — CuisineCodes come straight from
// FoodieCuisineDictionaryCodes, budget from users.foodie_budget_tier.
func TestLoadTasteProfile_ExplicitCuisines(t *testing.T) {
	loader, pool, ctx := setup(t)
	budget := domain.FoodieBudgetTierMid
	uid := seedUser(ctx, t, pool, &budget)
	if err := foodieprofile.New(pool).Replace(ctx, uid, domain.FoodieProfilePreferences{
		Cuisines: []string{domain.FoodieCuisineItalian, domain.FoodieCuisineKazakh},
		Diets:    []string{domain.FoodieDietHalal},
	}); err != nil {
		t.Fatalf("replace foodie profile: %v", err)
	}

	got, err := loader.LoadTasteProfile(ctx, uid)
	if err != nil {
		t.Fatalf("LoadTasteProfile: %v", err)
	}

	wantCuisines := map[string]bool{"italian": true, "kazakh": true}
	if len(got.CuisineCodes) != 2 {
		t.Fatalf("CuisineCodes = %v, want 2 entries matching %v", got.CuisineCodes, wantCuisines)
	}
	for _, c := range got.CuisineCodes {
		if !wantCuisines[c] {
			t.Errorf("unexpected cuisine code %q in %v", c, got.CuisineCodes)
		}
	}
	if got.Budget == nil || *got.Budget != domain.PriceMid {
		t.Errorf("Budget = %v, want PriceMid", got.Budget)
	}
	if len(got.Diets) != 1 || got.Diets[0] != domain.FoodieDietHalal {
		t.Errorf("Diets = %v, want [halal]", got.Diets)
	}
	if len(got.BookedCuisineCodes) != 0 {
		t.Errorf("BookedCuisineCodes = %v, want empty (no bookings)", got.BookedCuisineCodes)
	}
	if len(got.BookedRestaurantIDs) != 0 {
		t.Errorf("BookedRestaurantIDs = %v, want empty (no bookings)", got.BookedRestaurantIDs)
	}
}

// TestLoadTasteProfile_NoExplicitCuisinesButBooked: a guest who never opened
// the wizard's cuisine step but has booking history gets ONLY the implicit
// signal — CuisineCodes stays empty (ScoreTasteMatch's own job to fall back),
// BookedCuisineCodes/BookedRestaurantIDs carry the history.
func TestLoadTasteProfile_NoExplicitCuisinesButBooked(t *testing.T) {
	loader, pool, ctx := setup(t)
	uid := seedUser(ctx, t, pool, nil)

	seafoodID := seedCuisine(ctx, t, pool, "seafood")
	europeanID := seedCuisine(ctx, t, pool, "european")
	venue := seedRestaurant(ctx, t, pool, "Ocean Basket", seafoodID, europeanID)

	base := time.Date(2026, 9, 1, 19, 0, 0, 0, time.UTC)
	seedBooking(ctx, t, pool, uid, venue, domain.BookingCompleted, base)

	got, err := loader.LoadTasteProfile(ctx, uid)
	if err != nil {
		t.Fatalf("LoadTasteProfile: %v", err)
	}
	if len(got.CuisineCodes) != 0 {
		t.Errorf("CuisineCodes = %v, want empty (guest never picked a cuisine tile)", got.CuisineCodes)
	}
	if len(got.BookedRestaurantIDs) != 1 || got.BookedRestaurantIDs[0] != venue {
		t.Errorf("BookedRestaurantIDs = %v, want [%s]", got.BookedRestaurantIDs, venue)
	}
	wantCuisines := map[string]bool{"seafood": true, "european": true}
	if len(got.BookedCuisineCodes) != 2 {
		t.Fatalf("BookedCuisineCodes = %v, want 2 entries matching %v", got.BookedCuisineCodes, wantCuisines)
	}
	for _, c := range got.BookedCuisineCodes {
		if !wantCuisines[c] {
			t.Errorf("unexpected booked cuisine code %q in %v", c, got.BookedCuisineCodes)
		}
	}
}

// TestLoadTasteProfile_NoProfileNoBookings: a guest who never opened the
// wizard and never booked anything gets a zero-value TasteProfile, never an
// error — same "empty, not ErrNotFound" convention as
// domain.FoodieProfileRepository.Get.
func TestLoadTasteProfile_NoProfileNoBookings(t *testing.T) {
	loader, pool, ctx := setup(t)
	uid := seedUser(ctx, t, pool, nil)

	got, err := loader.LoadTasteProfile(ctx, uid)
	if err != nil {
		t.Fatalf("LoadTasteProfile: %v", err)
	}
	if len(got.CuisineCodes) != 0 || len(got.Diets) != 0 || len(got.BookedCuisineCodes) != 0 || len(got.BookedRestaurantIDs) != 0 {
		t.Fatalf("got %+v, want every collection empty", got)
	}
	if got.Budget != nil {
		t.Fatalf("Budget = %v, want nil", got.Budget)
	}
}

// TestLoadTasteProfile_CancelledBookingExcludedNoShowIncluded pins spec
// §5.1's exact rule: "бронировал" = status ∉ {cancelled}. A cancelled
// booking's venue must never appear; an auto no_show's venue must.
func TestLoadTasteProfile_CancelledBookingExcludedNoShowIncluded(t *testing.T) {
	loader, pool, ctx := setup(t)
	uid := seedUser(ctx, t, pool, nil)

	kazakhID := seedCuisine(ctx, t, pool, "kazakh")
	italianID := seedCuisine(ctx, t, pool, "italian")
	cancelledVenue := seedRestaurant(ctx, t, pool, "Cancelled Place", kazakhID)
	noShowVenue := seedRestaurant(ctx, t, pool, "No-show Place", italianID)

	base := time.Date(2026, 9, 1, 19, 0, 0, 0, time.UTC)
	seedBooking(ctx, t, pool, uid, cancelledVenue, domain.BookingCancelled, base)
	seedBooking(ctx, t, pool, uid, noShowVenue, domain.BookingNoShow, base.Add(24*time.Hour))

	got, err := loader.LoadTasteProfile(ctx, uid)
	if err != nil {
		t.Fatalf("LoadTasteProfile: %v", err)
	}
	if len(got.BookedRestaurantIDs) != 1 || got.BookedRestaurantIDs[0] != noShowVenue {
		t.Fatalf("BookedRestaurantIDs = %v, want only the no_show venue [%s] (cancelled must be excluded)", got.BookedRestaurantIDs, noShowVenue)
	}
	if len(got.BookedCuisineCodes) != 1 || got.BookedCuisineCodes[0] != "italian" {
		t.Fatalf("BookedCuisineCodes = %v, want [italian] only", got.BookedCuisineCodes)
	}
}

// TestLoadTasteProfile_Deterministic: two calls over the same data return the
// same TasteProfile — the property BE-2/BE-3/BE-4 all rely on to agree with
// each other on "this guest's taste".
func TestLoadTasteProfile_Deterministic(t *testing.T) {
	loader, pool, ctx := setup(t)
	uid := seedUser(ctx, t, pool, nil)
	kazakhID := seedCuisine(ctx, t, pool, "kazakh")
	italianID := seedCuisine(ctx, t, pool, "italian")
	v1 := seedRestaurant(ctx, t, pool, "V1", kazakhID)
	v2 := seedRestaurant(ctx, t, pool, "V2", italianID)
	base := time.Date(2026, 9, 1, 19, 0, 0, 0, time.UTC)
	seedBooking(ctx, t, pool, uid, v1, domain.BookingCompleted, base)
	seedBooking(ctx, t, pool, uid, v2, domain.BookingConfirmed, base.Add(48*time.Hour))

	got1, err := loader.LoadTasteProfile(ctx, uid)
	if err != nil {
		t.Fatalf("LoadTasteProfile (1): %v", err)
	}
	got2, err := loader.LoadTasteProfile(ctx, uid)
	if err != nil {
		t.Fatalf("LoadTasteProfile (2): %v", err)
	}
	if len(got1.BookedCuisineCodes) != len(got2.BookedCuisineCodes) {
		t.Fatalf("BookedCuisineCodes differ between calls: %v vs %v", got1.BookedCuisineCodes, got2.BookedCuisineCodes)
	}
	for i := range got1.BookedCuisineCodes {
		if got1.BookedCuisineCodes[i] != got2.BookedCuisineCodes[i] {
			t.Fatalf("BookedCuisineCodes order differs between calls: %v vs %v", got1.BookedCuisineCodes, got2.BookedCuisineCodes)
		}
	}
}
