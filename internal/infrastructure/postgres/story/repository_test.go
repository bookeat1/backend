package story

import (
	"context"
	"testing"

	"github.com/google/uuid"

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
