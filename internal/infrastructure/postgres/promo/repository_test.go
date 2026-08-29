package promo

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

func seedRestaurant(ctx context.Context, t *testing.T, pool sqltx.Querier, name string) uuid.UUID {
	t.Helper()
	repo := restaurant.New(pool)
	r := &domain.Restaurant{ID: uuid.New(), Name: name, City: domain.CityAlmaty, PriceCategory: domain.PriceMid, IsActive: true}
	if err := repo.Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return r.ID
}

func mkPromo(rid uuid.UUID, status domain.PromoStatus, startsIn, dur time.Duration) *domain.Promo {
	start := time.Now().Add(startsIn).UTC().Truncate(time.Second)
	return &domain.Promo{
		RestaurantID: &rid,
		Title:        "P",
		StartsAt:     start,
		EndsAt:       start.Add(dur),
		Status:       status,
	}
}

func TestListActive_OnlyPublishedWithinWindow(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "promos", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Bistro")
	repo := New(pool)

	// published, window contains now: SHOWN
	live := mkPromo(rid, domain.PromoPublished, -time.Hour, 2*time.Hour)
	// published, not started yet: HIDDEN
	future := mkPromo(rid, domain.PromoPublished, time.Hour, 2*time.Hour)
	// published, already expired: HIDDEN
	expired := mkPromo(rid, domain.PromoPublished, -48*time.Hour, time.Hour)
	// draft within window: HIDDEN
	draft := mkPromo(rid, domain.PromoDraft, -time.Hour, 2*time.Hour)
	// hidden within window: HIDDEN
	hidden := mkPromo(rid, domain.PromoHidden, -time.Hour, 2*time.Hour)
	for _, p := range []*domain.Promo{live, future, expired, draft, hidden} {
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("create promo: %v", err)
		}
	}

	items, total, err := repo.ListActive(ctx, rid, time.Now(), 1, 20)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("expected exactly 1 active promo, got total=%d len=%d", total, len(items))
	}
	if items[0].ID != live.ID {
		t.Fatalf("wrong active promo returned: %s", items[0].ID)
	}
}

func TestListByRestaurant_StatusFilter(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "promos", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Bistro")
	repo := New(pool)

	for _, p := range []*domain.Promo{
		mkPromo(rid, domain.PromoDraft, time.Hour, time.Hour),
		mkPromo(rid, domain.PromoPublished, time.Hour, time.Hour),
		mkPromo(rid, domain.PromoHidden, time.Hour, time.Hour),
	} {
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	// No filter → all 3.
	_, total, err := repo.ListByRestaurant(ctx, rid, nil, 1, 20)
	if err != nil || total != 3 {
		t.Fatalf("unfiltered admin list: total=%d err=%v", total, err)
	}
	// Filter to draft → 1.
	_, total, err = repo.ListByRestaurant(ctx, rid, []domain.PromoStatus{domain.PromoDraft}, 1, 20)
	if err != nil || total != 1 {
		t.Fatalf("draft-filtered admin list: total=%d err=%v", total, err)
	}
}

// The promo cover round-trips as a full public URL, and "no picture" stays a
// real NULL — the API must be able to say "there is no image" instead of
// inventing one.
func TestCoverImageURL_RoundTripAndNull(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "promos", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Bistro")
	repo := New(pool)

	cover := "https://pub-41b6f06fc8e74b6e959cdd6def081e22.r2.dev/promos/happy-hour.jpg"
	withCover := mkPromo(rid, domain.PromoPublished, -time.Hour, 2*time.Hour)
	withCover.CoverImageURL = &cover
	withoutCover := mkPromo(rid, domain.PromoPublished, -time.Hour, 2*time.Hour)
	for _, p := range []*domain.Promo{withCover, withoutCover} {
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("create promo: %v", err)
		}
	}

	got, err := repo.GetByID(ctx, withCover.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CoverImageURL == nil || *got.CoverImageURL != cover {
		t.Fatalf("cover = %v, want %q", got.CoverImageURL, cover)
	}

	got, err = repo.GetByID(ctx, withoutCover.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CoverImageURL != nil {
		t.Fatalf("cover = %v, want nil for a promo with no picture", *got.CoverImageURL)
	}

	// And an update can both set and clear it.
	got.CoverImageURL = &cover
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	reread, err := repo.GetByID(ctx, withoutCover.ID)
	if err != nil || reread.CoverImageURL == nil || *reread.CoverImageURL != cover {
		t.Fatalf("cover after update = %v (err %v)", reread.CoverImageURL, err)
	}
	reread.CoverImageURL = nil
	if err := repo.Update(ctx, reread); err != nil {
		t.Fatalf("update: %v", err)
	}
	cleared, err := repo.GetByID(ctx, withoutCover.ID)
	if err != nil || cleared.CoverImageURL != nil {
		t.Fatalf("cover after clearing = %v (err %v)", cleared.CoverImageURL, err)
	}
}

// discount_percent round-trips as an int and "no discount" stays a real NULL —
// the «−30%» badge value must be distinguishable from a promo that has no badge
// at all (a NULL is not a 0%). An update can both set and clear it.
func TestDiscountPercent_RoundTripAndNull(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "promos", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Bistro")
	repo := New(pool)

	thirty := 30
	withDiscount := mkPromo(rid, domain.PromoPublished, -time.Hour, 2*time.Hour)
	withDiscount.DiscountPercent = &thirty
	withoutDiscount := mkPromo(rid, domain.PromoPublished, -time.Hour, 2*time.Hour)
	for _, p := range []*domain.Promo{withDiscount, withoutDiscount} {
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("create promo: %v", err)
		}
	}

	got, err := repo.GetByID(ctx, withDiscount.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DiscountPercent == nil || *got.DiscountPercent != thirty {
		t.Fatalf("discount = %v, want %d", got.DiscountPercent, thirty)
	}

	got, err = repo.GetByID(ctx, withoutDiscount.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DiscountPercent != nil {
		t.Fatalf("discount = %v, want nil for a promo with no badge", *got.DiscountPercent)
	}

	// An update can set the discount on a promo that had none.
	got.DiscountPercent = &thirty
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	reread, err := repo.GetByID(ctx, withoutDiscount.ID)
	if err != nil || reread.DiscountPercent == nil || *reread.DiscountPercent != thirty {
		t.Fatalf("discount after update = %v (err %v)", reread.DiscountPercent, err)
	}
	// ...and clear it again.
	reread.DiscountPercent = nil
	if err := repo.Update(ctx, reread); err != nil {
		t.Fatalf("update: %v", err)
	}
	cleared, err := repo.GetByID(ctx, withoutDiscount.ID)
	if err != nil || cleared.DiscountPercent != nil {
		t.Fatalf("discount after clearing = %v (err %v)", cleared.DiscountPercent, err)
	}
}

// The DB CHECK is the schema's last line of defense: an out-of-range discount
// must be refused even if some future code path skips the usecase validation.
func TestDiscountPercent_CheckRejectsOutOfRange(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "promos", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Bistro")
	repo := New(pool)

	over := 150
	bad := mkPromo(rid, domain.PromoPublished, -time.Hour, 2*time.Hour)
	bad.DiscountPercent = &over
	if err := repo.Create(ctx, bad); err == nil {
		t.Fatal("a discount over 100 must be refused by the CHECK constraint, got no error")
	}
}

// The fine print's translations must survive a round trip through the column
// added by migration 0101 — on create, on update, and through the LIST read
// the guest listing and the favorites screen share (ScanListItem).
func TestTermsI18nRoundTrip(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "promos", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Bistro")
	repo := New(pool)

	p := mkPromo(rid, domain.PromoPublished, -time.Hour, 2*time.Hour)
	p.Terms = "Только зал"
	p.TermsI18n = domain.I18n{"ru": "Только зал", "kk": "Тек залда"}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TermsI18n["kk"] != "Тек залда" || got.TermsI18n["ru"] != "Только зал" {
		t.Fatalf("terms_i18n did not survive the write: %v", got.TermsI18n)
	}
	if v := got.TermsI18n.Resolve(domain.LocaleKK, got.Terms); v != "Тек залда" {
		t.Errorf("kk read = %q", v)
	}
	if v := got.TermsI18n.Resolve(domain.LocaleEN, got.Terms); v != "Только зал" {
		t.Errorf("en read = %q, want the Russian fallback", v)
	}

	got.TermsI18n = domain.I18n{"ru": "Только зал", "en": "Dine-in only"}
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, err := repo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if again.TermsI18n["en"] != "Dine-in only" || again.TermsI18n["kk"] != "" {
		t.Fatalf("update did not replace the map: %v", again.TermsI18n)
	}

	items, _, err := repo.ListActive(ctx, rid, time.Now(), 1, 20)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(items) != 1 || items[0].TermsI18n["en"] != "Dine-in only" {
		t.Fatalf("the listing read must carry terms_i18n, got %v", items)
	}

	// A promo with no translations keeps the column NULL rather than `{}`.
	plain := mkPromo(rid, domain.PromoDraft, -time.Hour, 2*time.Hour)
	if err := repo.Create(ctx, plain); err != nil {
		t.Fatalf("create plain: %v", err)
	}
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT terms_i18n IS NULL FROM promos WHERE id = $1`, plain.ID).Scan(&isNull); err != nil {
		t.Fatalf("read raw column: %v", err)
	}
	if !isNull {
		t.Error("a promo without translations must leave terms_i18n NULL")
	}
}
