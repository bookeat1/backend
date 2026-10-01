package restaurant

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// TestListActiveDistanceSort covers the guest-position ordering: nearest
// venue first, a venue with no coordinates of its own (or farther than
// maxGuestDistanceKm) sorts after it in the ordinary display_order/name order
// (never dropped), and without a guest position the pre-existing
// display_order/name order is untouched.
func TestListActiveDistanceSort(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	// Guest stands at Almaty's Abay/Dostyk intersection (roughly).
	guestLat, guestLng := 43.238293, 76.945465

	// far: ~340km away (Astana), beyond the radius. near: a few hundred meters
	// from the guest. none: no coordinates at all.
	far := &domain.Restaurant{
		ID: uuid.New(), Name: "Astana Place", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
		Latitude: ptr(51.169392), Longitude: ptr(71.449074),
	}
	near := &domain.Restaurant{
		ID: uuid.New(), Name: "Corner Cafe", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
		Latitude: ptr(43.239), Longitude: ptr(76.946),
	}
	none := &domain.Restaurant{
		ID: uuid.New(), Name: "Aaa No Coords", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
	}
	for _, m := range []*domain.Restaurant{far, near, none} {
		if err := repo.Create(ctx, m); err != nil {
			t.Fatalf("create %s: %v", m.Name, err)
		}
	}

	t.Run("orders nearest first, out-of-radius and no-coords venues after it by name", func(t *testing.T) {
		items, total, err := repo.ListActive(ctx, domain.RestaurantFilter{
			City: ptr(domain.CityAlmaty), GuestLat: &guestLat, GuestLng: &guestLng,
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if total != 3 || len(items) != 3 {
			t.Fatalf("total = %d, len = %d, want 3/3", total, len(items))
		}
		got := []uuid.UUID{items[0].ID, items[1].ID, items[2].ID}
		// far and none tie on distance (both NULL) and display_order (both
		// NULL), so name decides: "Aaa No Coords" < "Astana Place".
		want := []uuid.UUID{near.ID, none.ID, far.ID}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("position %d = %s, want %s (order %v)", i, got[i], want[i], got)
			}
		}
	})

	t.Run("without guest position falls back to name order", func(t *testing.T) {
		items, _, err := repo.ListActive(ctx, domain.RestaurantFilter{City: ptr(domain.CityAlmaty)})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(items) != 3 || items[0].ID != none.ID {
			t.Errorf("first item = %+v, want %q (name-order fallback, no display_order set)", items, none.Name)
		}
	})

	t.Run("half a pair is ignored, same as no position", func(t *testing.T) {
		items, _, err := repo.ListActive(ctx, domain.RestaurantFilter{City: ptr(domain.CityAlmaty), GuestLat: &guestLat})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(items) != 3 || items[0].ID != none.ID {
			t.Errorf("first item id = %s, want %s (half a pair must not sort by distance)", items[0].ID, none.ID)
		}
	})
}

// TestSearchDistanceSort mirrors TestListActiveDistanceSort for the
// text-less browse path of Search, and checks a non-empty query keeps
// ranking by relevance instead of distance.
func TestSearchDistanceSort(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	guestLat, guestLng := 43.238293, 76.945465

	// "far" gets the smaller id on purpose, see the text-query subtest below.
	lowID, highID := uuid.New(), uuid.New()
	if lowID.String() > highID.String() {
		lowID, highID = highID, lowID
	}
	far := &domain.Restaurant{
		ID: lowID, Name: "Pasta Astana", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
		Latitude: ptr(51.169392), Longitude: ptr(71.449074),
	}
	near := &domain.Restaurant{
		ID: highID, Name: "Pasta Corner", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
		Latitude: ptr(43.239), Longitude: ptr(76.946),
	}
	for _, m := range []*domain.Restaurant{far, near} {
		if err := repo.Create(ctx, m); err != nil {
			t.Fatalf("create %s: %v", m.Name, err)
		}
	}

	t.Run("browse (no query) orders by distance", func(t *testing.T) {
		items, _, err := repo.Search(ctx, domain.RestaurantSearchFilter{
			City: ptr(domain.CityAlmaty), GuestLat: &guestLat, GuestLng: &guestLng,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(items) != 2 || items[0].ID != near.ID {
			t.Errorf("first item = %+v, want %q nearest first", items, near.Name)
		}
	})

	t.Run("text query keeps relevance order over distance", func(t *testing.T) {
		// Both venues match "Pasta" equally on name, so relevance ties and the
		// final tie-break is id ASC. Distance would put "Pasta Corner" first;
		// the ids are arranged so that id order puts "Pasta Astana" first. A
		// text query that was (wrongly) re-sorted by GuestLat/GuestLng would
		// flip the pair.
		wantFirst, wantSecond := far.ID, near.ID

		withCoords, _, err := repo.Search(ctx, domain.RestaurantSearchFilter{
			Query: "Pasta", City: ptr(domain.CityAlmaty), GuestLat: &guestLat, GuestLng: &guestLng,
		})
		if err != nil {
			t.Fatalf("search with coords: %v", err)
		}
		withoutCoords, _, err := repo.Search(ctx, domain.RestaurantSearchFilter{
			Query: "Pasta", City: ptr(domain.CityAlmaty),
		})
		if err != nil {
			t.Fatalf("search without coords: %v", err)
		}
		if len(withCoords) != 2 || len(withoutCoords) != 2 {
			t.Fatalf("len = %d / %d, want 2 / 2", len(withCoords), len(withoutCoords))
		}
		for i := range withCoords {
			if withCoords[i].ID != withoutCoords[i].ID {
				t.Errorf("position %d: with coords %s, without %s: a text query must ignore GuestLat/GuestLng",
					i, withCoords[i].ID, withoutCoords[i].ID)
			}
		}
		if withCoords[0].ID != wantFirst || withCoords[1].ID != wantSecond {
			t.Errorf("order = [%s %s], want [%s %s] (relevance tie, id ASC), not distance order",
				withCoords[0].ID, withCoords[1].ID, wantFirst, wantSecond)
		}
	})
}

// TestDistanceSortRadiusAndTieBreak covers the B1 hardening: a venue farther
// than maxGuestDistanceKm sorts in the tail with no-coordinates venues (so a
// guest in Astana looking at an Almaty catalog sees the ordinary order, not
// noise), and equal distances are ordered deterministically by id.
func TestDistanceSortRadiusAndTieBreak(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	mk := func(name string, order *int, lat, lng *float64) *domain.Restaurant {
		return &domain.Restaurant{
			ID: uuid.New(), Name: name, City: domain.CityAlmaty,
			PriceCategory: domain.PriceMid, IsActive: true,
			DisplayOrder: order, Latitude: lat, Longitude: lng,
		}
	}
	create := func(ms ...*domain.Restaurant) {
		t.Helper()
		for _, m := range ms {
			if err := repo.Create(ctx, m); err != nil {
				t.Fatalf("create %s: %v", m.Name, err)
			}
		}
	}
	ids := func(items []domain.RestaurantListItem) []uuid.UUID {
		out := make([]uuid.UUID, len(items))
		for i := range items {
			out[i] = items[i].ID
		}
		return out
	}
	equal := func(a, b []uuid.UUID) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// Editorial order (display_order) deliberately disagrees with distance.
	// Guest in Almaty: within ~1 km of "In Town", ~340 km from Astana.
	guestLat, guestLng := 43.238293, 76.945465
	inTown := mk("Zeta In Town", ptr(30), ptr(43.239), ptr(76.946))
	astana := mk("Alpha Astana", ptr(10), ptr(51.169392), ptr(71.449074))
	noCoords := mk("Beta No Coords", ptr(5), nil, nil)
	create(inTown, astana, noCoords)

	t.Run("venue beyond the radius sorts with no-coords venues in editorial order", func(t *testing.T) {
		for name, run := range map[string]func() ([]domain.RestaurantListItem, error){
			"ListActive": func() ([]domain.RestaurantListItem, error) {
				items, _, err := repo.ListActive(ctx, domain.RestaurantFilter{City: ptr(domain.CityAlmaty), GuestLat: &guestLat, GuestLng: &guestLng})
				return items, err
			},
			"Search": func() ([]domain.RestaurantListItem, error) {
				items, _, err := repo.Search(ctx, domain.RestaurantSearchFilter{City: ptr(domain.CityAlmaty), GuestLat: &guestLat, GuestLng: &guestLng})
				return items, err
			},
		} {
			items, err := run()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			// Near first, then the tail by display_order: NoCoords (5) before Astana (10),
			// even though Astana has coordinates. Pure distance order would put Astana
			// (340 km) ahead of the venue with no coordinates.
			want := []uuid.UUID{inTown.ID, noCoords.ID, astana.ID}
			if got := ids(items); !equal(got, want) {
				t.Errorf("%s order = %v, want %v", name, got, want)
			}
		}
	})

	t.Run("guest far from every venue gets the same order as no coordinates", func(t *testing.T) {
		// Guest in Astana, whole catalog in Almaty: every venue is out of range.
		testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
		// Two is slightly nearer to Astana than One (higher latitude), so a
		// raw distance sort would put Two first; editorial order says One.
		a := mk("Almaty One", ptr(1), ptr(43.25), ptr(76.95))
		b := mk("Almaty Two", ptr(2), ptr(43.30), ptr(76.90))
		c := mk("Almaty Three", ptr(3), nil, nil)
		create(a, b, c)
		astanaLat, astanaLng := 51.1694, 71.4491

		for name, withGuest := range map[string]func() ([]domain.RestaurantListItem, error){
			"ListActive": func() ([]domain.RestaurantListItem, error) {
				items, _, err := repo.ListActive(ctx, domain.RestaurantFilter{City: ptr(domain.CityAlmaty), GuestLat: &astanaLat, GuestLng: &astanaLng})
				return items, err
			},
			"Search": func() ([]domain.RestaurantListItem, error) {
				items, _, err := repo.Search(ctx, domain.RestaurantSearchFilter{City: ptr(domain.CityAlmaty), GuestLat: &astanaLat, GuestLng: &astanaLng})
				return items, err
			},
		} {
			got, err := withGuest()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			want := []uuid.UUID{a.ID, b.ID, c.ID} // display_order 1, 2, 3
			if !equal(ids(got), want) {
				t.Errorf("%s order = %v, want display_order order %v", name, ids(got), want)
			}
		}
	})

	t.Run("equal distance and equal editorial keys fall back to id ASC", func(t *testing.T) {
		testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
		// Same name, same pin, same display_order: only id can order them.
		// The row inserted first gets the LARGER id, so heap order (what an
		// unordered tie would return) is the opposite of id ASC.
		x := mk("Twin", ptr(1), ptr(43.24), ptr(76.95))
		y := mk("Twin", ptr(1), ptr(43.24), ptr(76.95))
		if x.ID.String() < y.ID.String() {
			x.ID, y.ID = y.ID, x.ID
		}
		create(x, y)
		want := []uuid.UUID{y.ID, x.ID}
		items, _, err := repo.ListActive(ctx, domain.RestaurantFilter{City: ptr(domain.CityAlmaty), GuestLat: &guestLat, GuestLng: &guestLng})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if got := ids(items); !equal(got, want) {
			t.Errorf("order = %v, want id ASC %v", got, want)
		}
	})
}
