package story

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// The story lifetime (migration 0088) against a real Postgres. Three claims,
// and the middle one is the one a careless implementation breaks:
//
//  1. a story whose expires_at has passed is NOT served to guests;
//  2. a story with NO expiry (NULL — every row that existed before 0088) IS
//     still served, exactly as before;
//  3. the venue cabinet still lists the expired one, so it can be extended or
//     deleted instead of silently vanishing from the venue's own screen.
//
// The clock is passed in, never read from the database, so the test names the
// instant instead of sleeping past one.
func TestListActiveByRestaurantHidesExpiredStories(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'A','Алматы','₸')`,
		rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	// Four active cards, differing only in their expiry, seeded out of display
	// order so the assertion below also proves the surviving cards keep their
	// sort_order relation when the expired ones drop out.
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurant_stories (restaurant_id, image_url, sort_order, is_active, expires_at) VALUES
		 ($1,'https://cdn/never.jpg',      2, true, NULL),
		 ($1,'https://cdn/expired.jpg',    0, true, $2),
		 ($1,'https://cdn/future.jpg',     1, true, $3),
		 ($1,'https://cdn/just-now.jpg',   3, true, $4)`,
		rid,
		now.Add(-time.Minute), // yesterday's oysters
		now.Add(24*time.Hour), // the 24h default a venue would accept
		now,                   // expires exactly at now: the boundary
	); err != nil {
		t.Fatalf("seed stories: %v", err)
	}

	repo := New(pool)
	got, err := repo.ListActiveByRestaurant(ctx, rid, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var urls []string
	for _, s := range got {
		urls = append(urls, s.ImageURL)
	}
	// future (sort_order 1) then never (2). The expired card and the one whose
	// expiry lands exactly on now are gone — the predicate is expires_at > now,
	// so "expires at 12:00" means "not shown at 12:00", the same strictness
	// domain.Story.IsExpired uses.
	want := []string{"https://cdn/future.jpg", "https://cdn/never.jpg"}
	if len(urls) != len(want) {
		t.Fatalf("public list = %v, want %v", urls, want)
	}
	for i := range want {
		if urls[i] != want[i] {
			t.Fatalf("public list = %v, want %v", urls, want)
		}
	}

	// Claim 2, stated on its own so a regression names itself: the NULL card is
	// the shape every pre-0088 story has, and it must survive the deploy.
	var served bool
	for _, s := range got {
		if s.ImageURL == "https://cdn/never.jpg" {
			served = true
			if s.ExpiresAt != nil {
				t.Errorf("a story with no expiry must scan as nil, got %v", *s.ExpiresAt)
			}
		}
	}
	if !served {
		t.Error("a story with NO expiry stopped being served to guests")
	}

	// Claim 3: the cabinet read is clock-free and still shows all four.
	admin, err := repo.ListByRestaurant(ctx, rid)
	if err != nil {
		t.Fatalf("admin list: %v", err)
	}
	if len(admin) != 4 {
		t.Fatalf("admin list len = %d, want 4 (the expired card must stay visible to the venue)", len(admin))
	}
	var sawExpired bool
	for _, s := range admin {
		if s.ImageURL == "https://cdn/expired.jpg" {
			sawExpired = true
			if s.ExpiresAt == nil {
				t.Error("the expired card's expires_at must round-trip so the cabinet can pre-fill the form")
			}
		}
	}
	if !sawExpired {
		t.Error("the expired card is missing from the venue cabinet's list")
	}
}

// Expiry is a SECOND axis, not a rename of is_active: a card can be inactive but
// unexpired, or expired but still switched on, and only the combination decides
// what a guest sees. Extending an expired card (a later expires_at) brings it
// back without touching the switch, and clearing the expiry (NULL) makes it
// permanent again — both are the operations the cabinet offers on an expired
// card, so both are exercised through the repository here.
func TestExpiryRoundTripsAndIsIndependentOfIsActive(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_stories", "restaurants")
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'A','Алматы','₸')`,
		rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	repo := New(pool)

	// Switched OFF but unexpired: hidden by is_active alone.
	inactive := seedStory(t, ctx, repo, rid, "https://cdn/off.jpg", false, &future)
	// Switched ON but expired: hidden by the timer alone.
	expired := seedStory(t, ctx, repo, rid, "https://cdn/stale.jpg", true, &past)

	if got, err := repo.ListActiveByRestaurant(ctx, rid, now); err != nil {
		t.Fatalf("list: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("neither card may reach a guest, got %d", len(got))
	}

	// Extend the expired one: the venue moves the deadline, the switch is
	// untouched, and the card is served again.
	expired.ExpiresAt = &future
	if err := repo.Update(ctx, expired); err != nil {
		t.Fatalf("extend: %v", err)
	}
	got, err := repo.ListActiveByRestaurant(ctx, rid, now)
	if err != nil {
		t.Fatalf("list after extend: %v", err)
	}
	if len(got) != 1 || got[0].ID != expired.ID {
		t.Fatalf("the extended card must be served again, got %+v", got)
	}

	// Clear the expiry entirely: permanent again, and still nothing to do with
	// the inactive card, which stays hidden.
	expired.ExpiresAt = nil
	if err := repo.Update(ctx, expired); err != nil {
		t.Fatalf("clear expiry: %v", err)
	}
	stored, err := repo.GetByID(ctx, expired.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.ExpiresAt != nil {
		t.Fatalf("clearing the expiry must store NULL, got %v", *stored.ExpiresAt)
	}
	got, err = repo.ListActiveByRestaurant(ctx, rid, now.Add(10*365*24*time.Hour))
	if err != nil {
		t.Fatalf("list far in the future: %v", err)
	}
	if len(got) != 1 || got[0].ID != expired.ID {
		t.Fatalf("a card with no expiry must be served at any instant, got %+v", got)
	}
	if _, err := repo.GetByID(ctx, inactive.ID); err != nil {
		t.Fatalf("the inactive card must still be resolvable by id: %v", err)
	}
}

// seedStory inserts one story through the repository (so the INSERT column list
// is exercised too) and returns it with its generated id.
func seedStory(t *testing.T, ctx context.Context, repo *Repository, rid uuid.UUID,
	imageURL string, isActive bool, expiresAt *time.Time,
) *domain.Story {
	t.Helper()
	s := &domain.Story{
		RestaurantID: rid,
		ImageURL:     imageURL,
		IsActive:     isActive,
		ExpiresAt:    expiresAt,
	}
	if err := repo.Create(ctx, s); err != nil {
		t.Fatalf("seed story %s: %v", imageURL, err)
	}
	return s
}
