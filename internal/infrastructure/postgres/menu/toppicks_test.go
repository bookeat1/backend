package menu

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func seedVenue(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category, is_active)
		 VALUES ($1,$2,'Алматы','₸',true)`, id, name); err != nil {
		t.Fatalf("seed venue %s: %v", name, err)
	}
	return id
}

func seedItem(t *testing.T, repo *Repository, rid uuid.UUID, name string, available bool) uuid.UUID {
	t.Helper()
	m := &domain.MenuItem{ID: uuid.New(), RestaurantID: rid, Name: name, Price: "1000.00", IsAvailable: available}
	if err := repo.Create(context.Background(), m); err != nil {
		t.Fatalf("create dish %s: %v", name, err)
	}
	return m.ID
}

// Every existing row must come out of the migration unmarked — that is what
// makes the change invisible until a venue actually curates its rail.
func TestNewDishesStartUnmarked(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "restaurants")
	repo := New(pool)
	rid := seedVenue(t, pool, "Venue")
	id := seedItem(t, repo, rid, "Блюдо", true)

	m, err := repo.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.TopPickPosition != nil {
		t.Fatalf("a freshly created dish must not be marked, got slot %v", *m.TopPickPosition)
	}
}

// The slot cap and the one-dish-per-slot rule are DATABASE constraints, not app
// checks. This test is what proves an app-level bug cannot produce a 40-dish
// rail or two dishes fighting over slot 1.
func TestTopPickSlotsAreConstrainedByTheDatabase(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "restaurants")
	ctx := context.Background()
	repo := New(pool)
	rid := seedVenue(t, pool, "Venue")
	first := seedItem(t, repo, rid, "Первое", true)
	second := seedItem(t, repo, rid, "Второе", true)

	slot := 1
	if err := repo.SetTopPickPosition(ctx, rid, first, &slot); err != nil {
		t.Fatalf("mark first: %v", err)
	}
	if err := repo.SetTopPickPosition(ctx, rid, second, &slot); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("two dishes must not share a slot: want ErrAlreadyExists, got %v", err)
	}
	for _, bad := range []int{0, domain.MenuTopPickLimit + 1} {
		b := bad
		if err := repo.SetTopPickPosition(ctx, rid, second, &b); err == nil {
			t.Fatalf("slot %d must be rejected by the CHECK constraint", bad)
		}
	}

	// The same slot in ANOTHER venue is fine — the uniqueness is per venue.
	other := seedVenue(t, pool, "Other venue")
	otherItem := seedItem(t, repo, other, "Их блюдо", true)
	if err := repo.SetTopPickPosition(ctx, other, otherItem, &slot); err != nil {
		t.Fatalf("slot 1 of another venue: %v", err)
	}
}

// The tenant guard is the WHERE clause: a manager who guesses another venue's
// item id gets a 404, not a place in that venue's shop window.
func TestSetTopPickPositionRefusesAnotherVenuesDish(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "restaurants")
	ctx := context.Background()
	repo := New(pool)
	mine := seedVenue(t, pool, "Mine")
	theirs := seedVenue(t, pool, "Theirs")
	foreign := seedItem(t, repo, theirs, "Чужое", true)

	slot := 1
	if err := repo.SetTopPickPosition(ctx, mine, foreign, &slot); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	m, err := repo.GetByID(ctx, foreign)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.TopPickPosition != nil {
		t.Fatal("another venue's dish was marked")
	}
}

// ListTopPicks is the EDITOR view: it keeps a stop-listed dish visible (it
// still holds a slot) and it orders by the venue's own arrangement.
func TestListTopPicksKeepsStoppedDishesAndOrdersBySlot(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "restaurants")
	ctx := context.Background()
	repo := New(pool)
	rid := seedVenue(t, pool, "Venue")
	stopped := seedItem(t, repo, rid, "В стоп-листе", false)
	available := seedItem(t, repo, rid, "Доступное", true)
	seedItem(t, repo, rid, "Не отмечено", true)

	two, one := 2, 1
	if err := repo.SetTopPickPosition(ctx, rid, stopped, &two); err != nil {
		t.Fatalf("mark stopped: %v", err)
	}
	if err := repo.SetTopPickPosition(ctx, rid, available, &one); err != nil {
		t.Fatalf("mark available: %v", err)
	}

	got, err := repo.ListTopPicks(ctx, rid)
	if err != nil {
		t.Fatalf("list top picks: %v", err)
	}
	if len(got) != 2 || got[0].ID != available || got[1].ID != stopped {
		t.Fatalf("want [available, stopped] by slot, got %d rows: %+v", len(got), got)
	}
}

// Deleting a dish must take its mark with it — the mark lives on the row, so a
// deleted dish can never linger in the rail or hold a slot hostage.
func TestDeletingAMarkedDishFreesItsSlot(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "restaurants")
	ctx := context.Background()
	repo := New(pool)
	rid := seedVenue(t, pool, "Venue")
	gone := seedItem(t, repo, rid, "Удалим", true)
	next := seedItem(t, repo, rid, "Следующее", true)

	slot := 1
	if err := repo.SetTopPickPosition(ctx, rid, gone, &slot); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := repo.Delete(ctx, gone); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := repo.SetTopPickPosition(ctx, rid, next, &slot); err != nil {
		t.Fatalf("slot 1 must be free after the dish was deleted: %v", err)
	}
	picks, err := repo.ListTopPicks(ctx, rid)
	if err != nil {
		t.Fatalf("list top picks: %v", err)
	}
	if len(picks) != 1 || picks[0].ID != next {
		t.Fatalf("want only the surviving dish on the rail, got %+v", picks)
	}
}

// Editing a dish through the normal menu PATCH must not silently unmark it:
// Update does not write top_pick_position at all, and this is the test that
// keeps it that way.
func TestUpdatingADishKeepsItsTopPickSlot(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "restaurants")
	ctx := context.Background()
	repo := New(pool)
	rid := seedVenue(t, pool, "Venue")
	id := seedItem(t, repo, rid, "Блюдо", true)

	slot := 3
	if err := repo.SetTopPickPosition(ctx, rid, id, &slot); err != nil {
		t.Fatalf("mark: %v", err)
	}
	m, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	m.Name = "Блюдо с новым названием"
	if err := repo.Update(ctx, m); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if after.TopPickPosition == nil || *after.TopPickPosition != 3 {
		t.Fatalf("editing a dish must keep its slot, got %v", after.TopPickPosition)
	}
}

func TestClearTopPicksOnlyTouchesTheVenuesOwnRail(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "menu_items", "restaurants")
	ctx := context.Background()
	repo := New(pool)
	mine := seedVenue(t, pool, "Mine")
	theirs := seedVenue(t, pool, "Theirs")
	a := seedItem(t, repo, mine, "Моё 1", true)
	b := seedItem(t, repo, mine, "Моё 2", true)
	c := seedItem(t, repo, theirs, "Их", true)

	one, two := 1, 2
	for _, p := range []struct {
		rid  uuid.UUID
		id   uuid.UUID
		slot *int
	}{{mine, a, &one}, {mine, b, &two}, {theirs, c, &one}} {
		if err := repo.SetTopPickPosition(ctx, p.rid, p.id, p.slot); err != nil {
			t.Fatalf("mark: %v", err)
		}
	}

	n, err := repo.ClearTopPicks(ctx, mine)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 cleared, got %d", n)
	}
	theirPicks, err := repo.ListTopPicks(ctx, theirs)
	if err != nil {
		t.Fatalf("list theirs: %v", err)
	}
	if len(theirPicks) != 1 {
		t.Fatalf("another venue's rail was cleared too: %+v", theirPicks)
	}
}
