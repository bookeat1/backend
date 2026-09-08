package story

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// TestListActiveByRestaurant exercises the one read the table serves: active
// stories of a restaurant, ordered by sort_order then created_at, with inactive
// cards and other venues' cards excluded.
func TestListActiveByRestaurant(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()

	ridA, ridB := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'A','Алматы','₸'),($2,'B','Алматы','₸')`,
		ridA, ridB); err != nil {
		t.Fatalf("seed restaurants: %v", err)
	}

	// Two active cards for A out of insertion order, one inactive card for A, and
	// one active card for B (another venue, must be excluded).
	caption := "Летнее меню"
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurant_stories (restaurant_id, image_url, caption, sort_order, is_active) VALUES
		 ($1,'https://cdn/a2.jpg',$2,2,true),
		 ($1,'https://cdn/a1.jpg',NULL,1,true),
		 ($1,'https://cdn/a_hidden.jpg',NULL,0,false),
		 ($3,'https://cdn/b1.jpg',NULL,1,true)`,
		ridA, caption, ridB); err != nil {
		t.Fatalf("seed stories: %v", err)
	}

	repo := New(pool)
	got, err := repo.ListActiveByRestaurant(ctx, ridA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (inactive + other venue excluded)", len(got))
	}
	// Ordered by sort_order ASC: a1 (1) before a2 (2).
	if got[0].ImageURL != "https://cdn/a1.jpg" || got[1].ImageURL != "https://cdn/a2.jpg" {
		t.Errorf("order mismatch: [%q, %q]", got[0].ImageURL, got[1].ImageURL)
	}
	// a1 has no caption (nil), a2 carries one.
	if got[0].Caption != nil {
		t.Errorf("story a1 caption = %v, want nil", *got[0].Caption)
	}
	if got[1].Caption == nil || *got[1].Caption != caption {
		t.Errorf("story a2 caption = %v, want %q", got[1].Caption, caption)
	}
	if !got[0].IsActive || got[0].RestaurantID != ridA {
		t.Errorf("scan mismatch: is_active=%v restaurant_id=%v", got[0].IsActive, got[0].RestaurantID)
	}
}

// TestListActiveByRestaurantStableTieBreak: two cards sharing both sort_order
// AND created_at (the realistic case — now() is constant within one INSERT
// transaction) must come back in a deterministic order, broken by id, and never
// reshuffle between reads.
func TestListActiveByRestaurantStableTieBreak(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'A','Алматы','₸')`,
		rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}

	// Same sort_order (0) and the same explicit created_at for both rows, so only
	// the id tie-break decides their order.
	loID, hiID := uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurant_stories (id, restaurant_id, image_url, sort_order, is_active, created_at) VALUES
		 ($2,$1,'https://cdn/hi.jpg',0,true,'2026-01-01T00:00:00Z'),
		 ($3,$1,'https://cdn/lo.jpg',0,true,'2026-01-01T00:00:00Z')`,
		rid, hiID, loID); err != nil {
		t.Fatalf("seed stories: %v", err)
	}

	repo := New(pool)
	first, err := repo.ListActiveByRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("len = %d, want 2", len(first))
	}
	// id ASC: the all-zeros id sorts before the all-fs id, regardless of insert order.
	if first[0].ID != loID || first[1].ID != hiID {
		t.Fatalf("tie-break order = [%v, %v], want [%v, %v]", first[0].ID, first[1].ID, loID, hiID)
	}
	// And it is stable — a second read returns the same order.
	second, err := repo.ListActiveByRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("list again: %v", err)
	}
	if second[0].ID != first[0].ID || second[1].ID != first[1].ID {
		t.Errorf("order not stable between reads: %v then %v", first, second)
	}
}

// TestListActiveByRestaurantEmpty: a restaurant with no stories is not an error,
// it lists as an empty result.
func TestListActiveByRestaurantEmpty(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()

	repo := New(pool)
	got, err := repo.ListActiveByRestaurant(ctx, uuid.New())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

// The caption's translations (migration 0101) must survive create, update and
// both listing reads — and a card without a caption must keep the column NULL,
// which is what the CHECK added by the same migration insists on.
func TestCaptionI18nRoundTrip(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'A','Алматы','₸')`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	repo := New(pool)

	caption := "Летнее меню"
	s := &domain.Story{
		RestaurantID: rid,
		ImageURL:     "https://cdn/a.jpg",
		Caption:      &caption,
		CaptionI18n:  domain.I18n{"ru": caption, "kk": "Жазғы мәзір"},
		IsActive:     true,
	}
	if err := repo.Create(ctx, s); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CaptionI18n["kk"] != "Жазғы мәзір" {
		t.Fatalf("caption_i18n did not survive the write: %v", got.CaptionI18n)
	}
	if v := got.CaptionI18n.Resolve(domain.LocaleEN, *got.Caption); v != caption {
		t.Errorf("en caption = %q, want the Russian fallback", v)
	}

	got.CaptionI18n = domain.I18n{"ru": caption, "en": "Summer menu"}
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	list, err := repo.ListActiveByRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].CaptionI18n["en"] != "Summer menu" || list[0].CaptionI18n["kk"] != "" {
		t.Fatalf("the public listing must carry the updated map, got %v", list)
	}
	admin, err := repo.ListByRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("admin list: %v", err)
	}
	if len(admin) != 1 || admin[0].CaptionI18n["en"] != "Summer menu" {
		t.Fatalf("the cabinet listing must carry the map, got %v", admin)
	}

	plain := &domain.Story{RestaurantID: rid, ImageURL: "https://cdn/b.jpg", IsActive: true}
	if err := repo.Create(ctx, plain); err != nil {
		t.Fatalf("create captionless: %v", err)
	}
	var isNull bool
	if err := pool.QueryRow(ctx,
		`SELECT caption_i18n IS NULL FROM restaurant_stories WHERE id = $1`, plain.ID).Scan(&isNull); err != nil {
		t.Fatalf("read raw column: %v", err)
	}
	if !isNull {
		t.Error("a card without a caption must leave caption_i18n NULL")
	}
}
