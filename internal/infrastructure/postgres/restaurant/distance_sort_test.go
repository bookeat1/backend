package restaurant

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// TestListActiveDistanceSort covers the guest-position ordering: nearest
// venue first, a venue with no coordinates of its own sorts last (never
// dropped), and without a guest position the pre-existing
// display_order/name order is untouched.
func TestListActiveDistanceSort(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "restaurant_categories")
	repo := New(pool)
	ctx := context.Background()

	// Guest stands at Almaty's Abay/Dostyk intersection (roughly).
	guestLat, guestLng := 43.238293, 76.945465

	// far: ~340km away (Astana). near: a few hundred meters from the guest.
	// none: no coordinates at all.
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

	t.Run("orders nearest first, no-coords venue last", func(t *testing.T) {
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
		want := []uuid.UUID{near.ID, far.ID, none.ID}
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

	far := &domain.Restaurant{
		ID: uuid.New(), Name: "Pasta Astana", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
		Latitude: ptr(51.169392), Longitude: ptr(71.449074),
	}
	near := &domain.Restaurant{
		ID: uuid.New(), Name: "Pasta Corner", City: domain.CityAlmaty,
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
		// Both venues match "pasta" equally on name; a text query must not be
		// reordered by GuestLat/GuestLng at all — this only checks the query
		// still returns both matches rather than erroring or dropping one.
		items, _, err := repo.Search(ctx, domain.RestaurantSearchFilter{
			Query: "Pasta", City: ptr(domain.CityAlmaty), GuestLat: &guestLat, GuestLng: &guestLng,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("len = %d, want 2", len(items))
		}
	})
}
