package promo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

// Промо без заведения — то же решение, что у событий в 0084/0085, и проверяется
// оно там же, где живёт: в SQL. Колонка города, LEFT JOIN и COALESCE — ничего
// из этого фейк не воспроизводит.

var platformPromoTables = []string{"promo_images", "promos", "restaurants"}

func seedVenueIn(ctx context.Context, t *testing.T, pool sqltx.Querier, name string, city domain.City) uuid.UUID {
	t.Helper()
	r := &domain.Restaurant{ID: uuid.New(), Name: name, City: city, PriceCategory: domain.PriceMid, IsActive: true}
	if err := restaurant.New(pool).Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant %s: %v", name, err)
	}
	return r.ID
}

// seedLivePromo writes a published promo whose window contains now. rid nil =
// the platform's own offer; city nil = no override.
func seedLivePromo(ctx context.Context, t *testing.T, repo *Repository, rid *uuid.UUID, title string, city *string) uuid.UUID {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	p := &domain.Promo{
		RestaurantID: rid, Title: title, Status: domain.PromoPublished,
		StartsAt: now.Add(-time.Hour), EndsAt: now.Add(24 * time.Hour),
	}
	if city != nil {
		c := domain.City(*city)
		p.City = &c
	}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create promo %s: %v", title, err)
	}
	return p.ID
}

func promoTitlesFor(ctx context.Context, t *testing.T, repo *Repository, city string) []string {
	t.Helper()
	f := domain.PublicPromoFilter{}
	if city != "" {
		c := domain.City(city)
		f.City = &c
	}
	items, total, err := repo.ListPublicActive(ctx, f, time.Now())
	if err != nil {
		t.Fatalf("ListPublicActive(city=%s): %v", city, err)
	}
	if total != len(items) {
		t.Fatalf("total %d disagrees with the %d rows returned", total, len(items))
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}

func assertTitles(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A promo with no override lives in its venue's city — the behaviour every
// promo had before the column existed, and the one that must not move.
func TestListPublicActive_PromoLivesInItsVenueCity(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, platformPromoTables...)
	ctx := context.Background()
	repo := New(pool)

	almaty := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	astana := seedVenueIn(ctx, t, pool, "Bistro Astana", domain.CityAstana)
	seedLivePromo(ctx, t, repo, &almaty, "Скидка в Алматы", nil)
	seedLivePromo(ctx, t, repo, &astana, "Скидка в Астане", nil)

	assertTitles(t, promoTitlesFor(ctx, t, repo, string(domain.CityAlmaty)), "Скидка в Алматы")
	assertTitles(t, promoTitlesFor(ctx, t, repo, string(domain.CityAstana)), "Скидка в Астане")
}

// A platform promo with no city appears in every city; pinned to one, only
// there. Same three-state rule the events listing follows.
func TestListPublicActive_PlatformPromoCity(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, platformPromoTables...)
	ctx := context.Background()
	repo := New(pool)

	almaty := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	seedLivePromo(ctx, t, repo, &almaty, "Скидка в Алматы", nil)
	seedLivePromo(ctx, t, repo, nil, "Акция платформы", nil)
	astana := string(domain.CityAstana)
	seedLivePromo(ctx, t, repo, nil, "Акция для Астаны", &astana)

	assertTitles(t, promoTitlesFor(ctx, t, repo, string(domain.CityAlmaty)), "Скидка в Алматы", "Акция платформы")
	assertTitles(t, promoTitlesFor(ctx, t, repo, string(domain.CityAstana)), "Акция платформы", "Акция для Астаны")
}

// The city override is canonicalized on write by trg_promos_sync_city: the
// listing compares exact strings, so a code or a historical spelling stored raw
// would be linked to the right city and found by no filter at all.
func TestPromoCityOverrideIsCanonicalizedOnWrite(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, platformPromoTables...)
	ctx := context.Background()
	repo := New(pool)

	for _, written := range []string{"almaty", "alma-ata", "Алма-Ата", "  алматы  "} {
		t.Run(written, func(t *testing.T) {
			id := seedLivePromo(ctx, t, repo, nil, "Акция "+written, &written)
			got, err := repo.GetByID(ctx, id)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.City == nil || *got.City != domain.CityAlmaty {
				t.Fatalf("stored city = %v, want %q", got.City, domain.CityAlmaty)
			}
		})
	}

	// An empty override has exactly ONE representation — NULL, never ''.
	blank := "   "
	id := seedLivePromo(ctx, t, repo, nil, "Без города", &blank)
	got, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.City != nil {
		t.Fatalf("stored city = %v, want nil", got.City)
	}
}

// A deactivated venue still takes its promos off the listing; the platform's
// own promo, having no venue to deactivate, stays.
func TestListPublicActive_DeactivatedVenueStillHidesItsPromos(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, platformPromoTables...)
	ctx := context.Background()
	repo := New(pool)

	venue := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	seedLivePromo(ctx, t, repo, &venue, "Скидка Bistro", nil)
	seedLivePromo(ctx, t, repo, nil, "Акция платформы", nil)

	if _, err := pool.Exec(ctx, `UPDATE restaurants SET is_active = false WHERE id = $1`, venue); err != nil {
		t.Fatalf("deactivate venue: %v", err)
	}
	assertTitles(t, promoTitlesFor(ctx, t, repo, string(domain.CityAlmaty)), "Акция платформы")
}

// The listing's venue block is optional, and the detail read applies the same
// visibility rule as the listing.
func TestGetPublicPromo(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, platformPromoTables...)
	ctx := context.Background()
	repo := New(pool)

	venue := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	bound := seedLivePromo(ctx, t, repo, &venue, "Скидка Bistro", nil)
	platform := seedLivePromo(ctx, t, repo, nil, "Акция платформы", nil)

	got, err := repo.GetPublic(ctx, platform, time.Now())
	if err != nil {
		t.Fatalf("GetPublic(platform): %v", err)
	}
	if got.Restaurant != nil || got.RestaurantID != nil {
		t.Fatalf("a platform promo must carry no venue, got %+v", got.Restaurant)
	}

	got, err = repo.GetPublic(ctx, bound, time.Now())
	if err != nil {
		t.Fatalf("GetPublic(venue-bound): %v", err)
	}
	if got.Restaurant == nil || got.Restaurant.ID != venue || got.Restaurant.Name != "Bistro Almaty" {
		t.Fatalf("a venue-bound promo must carry its venue, got %+v", got.Restaurant)
	}

	// An offer whose window has not opened is not an offer yet.
	now := time.Now().UTC()
	future := &domain.Promo{Title: "Скоро", Status: domain.PromoPublished,
		StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour)}
	if err := repo.Create(ctx, future); err != nil {
		t.Fatalf("create future promo: %v", err)
	}
	if _, err := repo.GetPublic(ctx, future.ID, time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetPublic(ctx, uuid.New(), time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
	}
}

func TestListPlatformPromos(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, platformPromoTables...)
	ctx := context.Background()
	repo := New(pool)

	venue := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	seedLivePromo(ctx, t, repo, &venue, "Скидка Bistro", nil)
	seedLivePromo(ctx, t, repo, nil, "Акция платформы", nil)

	items, total, err := repo.ListPlatform(ctx, nil, 1, 20)
	if err != nil {
		t.Fatalf("ListPlatform: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Title != "Акция платформы" {
		t.Fatalf("got %d items %+v, want only the platform's own", total, items)
	}

	own, ownTotal, err := repo.ListByRestaurant(ctx, venue, nil, 1, 20)
	if err != nil {
		t.Fatalf("ListByRestaurant: %v", err)
	}
	if ownTotal != 1 || len(own) != 1 || own[0].Title != "Скидка Bistro" {
		t.Fatalf("got %d items %+v, want only the venue's own", ownTotal, own)
	}
}
