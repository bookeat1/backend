package city

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// newCity builds a throwaway dictionary entry. Every test cleans its own
// entries up: `cities` is seeded by the migration and shared with every other
// package's tests, so it must be left exactly as the migration made it.
func newCity(code, name string) *domain.CityEntry {
	return &domain.CityEntry{ID: uuid.New(), Code: code, Name: name, DisplayOrder: 100, IsActive: true}
}

func cleanup(t *testing.T, pool *pgxpool.Pool, ids ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range ids {
			if _, err := pool.Exec(ctx, `UPDATE restaurants SET city_id = NULL WHERE city_id = $1`, id); err != nil {
				t.Errorf("cleanup unlink %s: %v", id, err)
			}
			if _, err := pool.Exec(ctx, `DELETE FROM cities WHERE id = $1`, id); err != nil {
				t.Errorf("cleanup city %s: %v", id, err)
			}
		}
	})
}

// TestCreateRegistersItsOwnAliases: everything that resolves a city — the
// catalog filter and the insert trigger — goes through city_aliases. A city
// created without its own name and code in there would be a city nothing can
// find, which is the one failure mode a dictionary must not have.
func TestCreateRegistersItsOwnAliases(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	repo := New(pool)

	c := newCity("shymkent", "Шымкент")
	cleanup(t, pool, c.ID)
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, spelling := range []string{"Шымкент", "  шымкент ", "SHYMKENT", "shymkent"} {
		got, err := repo.ResolveAlias(ctx, spelling)
		if err != nil {
			t.Errorf("ResolveAlias(%q) = %v", spelling, err)
			continue
		}
		if got.ID != c.ID {
			t.Errorf("ResolveAlias(%q) found %s, want %s", spelling, got.ID, c.ID)
		}
	}

	if _, err := repo.ResolveAlias(ctx, "Тараз"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("ResolveAlias of an unknown spelling = %v, want ErrNotFound", err)
	}
}

// TestDuplicateNameIsRefusedByTheDatabase: the unique indexes are the ONLY
// duplicate guard, because a read-then-write check loses the race between two
// admins. The normalized-name index is what stops «Алматы» and «алматы» from
// becoming two cities the way «Европейская»/«европейская» became two cuisines.
func TestDuplicateNameIsRefusedByTheDatabase(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	repo := New(pool)

	dup := newCity("almaty_2", "  алматы ")
	cleanup(t, pool, dup.ID)
	if err := repo.Create(ctx, dup); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("Create with a differently-cased duplicate name = %v, want ErrAlreadyExists", err)
	}

	dupCode := newCity("almaty", "Алматы Второй")
	cleanup(t, pool, dupCode.ID)
	if err := repo.Create(ctx, dupCode); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("Create with a duplicate code = %v, want ErrAlreadyExists", err)
	}
}

// TestAliasCannotBeStolenFromAnotherCity: an alias moving to another city would
// silently move every venue matching that spelling into another city's results.
func TestAliasCannotBeStolenFromAnotherCity(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	repo := New(pool)

	c := newCity("taraz", "Тараз")
	cleanup(t, pool, c.ID)
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// «Алматы» already belongs to the seeded city.
	if err := repo.AddAlias(ctx, c.ID, "Алматы"); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("stealing an alias = %v, want ErrAlreadyExists", err)
	}
	// Re-adding its own alias is a no-op, not a conflict.
	if err := repo.AddAlias(ctx, c.ID, "Тараз"); err != nil {
		t.Fatalf("re-adding an own alias = %v, want no error", err)
	}
}

// TestRenameKeepsTheOldSpellingResolvable is the backward-compatibility rule
// spelled out in the repository: a phone in the store may keep sending the
// previous name as ?city= for months, and it must keep finding the same venues.
func TestRenameKeepsTheOldSpellingResolvable(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	repo := New(pool)

	c := newCity("kokshetau", "Кокшетау")
	cleanup(t, pool, c.ID)
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	c.Name = "Кокчетав"
	if err := repo.Update(ctx, c); err != nil {
		t.Fatalf("Update: %v", err)
	}
	for _, spelling := range []string{"Кокшетау", "Кокчетав", "kokshetau"} {
		got, err := repo.ResolveAlias(ctx, spelling)
		if err != nil {
			t.Errorf("ResolveAlias(%q) after rename = %v", spelling, err)
			continue
		}
		if got.ID != c.ID {
			t.Errorf("ResolveAlias(%q) found %s, want %s", spelling, got.ID, c.ID)
		}
	}
}

// TestRenamePropagatesToTheVenueStringAndKeepsTheLink walks the whole rename
// through the real database, trigger included: the venues' derived string
// follows the dictionary, and their city_id does NOT get nulled on the way —
// which is exactly what would happen if the new name were not an alias yet.
func TestRenamePropagatesToTheVenueStringAndKeepsTheLink(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	testdb.Truncate(t, pool, "restaurants")

	repo := New(pool)
	c := newCity("pavlodar", "Павлодар")
	cleanup(t, pool, c.ID)
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	venue := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'Venue','Павлодар','₸₸')`,
		venue); err != nil {
		t.Fatalf("seed venue: %v", err)
	}

	c.Name = "Павлодар Сити"
	if err := repo.Update(ctx, c); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// This is the second half of the usecase's transaction, run here against
	// the real trigger.
	if _, err := pool.Exec(ctx,
		`UPDATE restaurants SET city = $2 WHERE city_id = $1`, c.ID, c.Name); err != nil {
		t.Fatalf("rename venue strings: %v", err)
	}

	var stored string
	var linked *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT city, city_id FROM restaurants WHERE id = $1`, venue).Scan(&stored, &linked); err != nil {
		t.Fatalf("read venue: %v", err)
	}
	if stored != "Павлодар Сити" {
		t.Errorf("venue city = %q, want the new name", stored)
	}
	if linked == nil || *linked != c.ID {
		t.Fatalf("venue link = %v, want %s: the rename must not detach the venue", linked, c.ID)
	}
}

// TestReorderIsSetBasedAndIgnoresStrangers: reordering is cosmetic, so a stale
// id in the caller's list must not fail the batch — but it must not silently
// create anything either.
func TestReorderIsSetBasedAndIgnoresStrangers(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	repo := New(pool)

	a := newCity("reorder_a", "Реордер А")
	b := newCity("reorder_b", "Реордер Б")
	cleanup(t, pool, a.ID, b.ID)
	for _, c := range []*domain.CityEntry{a, b} {
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("Create %s: %v", c.Code, err)
		}
	}

	before := countRows(t, pool, "cities")
	if err := repo.Reorder(ctx, []uuid.UUID{b.ID, uuid.New(), a.ID}); err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	if got := countRows(t, pool, "cities"); got != before {
		t.Fatalf("cities count changed from %d to %d during a reorder", before, got)
	}

	gotB, err := repo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	gotA, err := repo.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if gotB.DisplayOrder >= gotA.DisplayOrder {
		t.Errorf("display orders b=%d a=%d, want b before a", gotB.DisplayOrder, gotA.DisplayOrder)
	}
}

// TestHiddenCityIsAbsentFromThePublicListAndPresentForTheAdmin: hiding must
// remove a city from the guest's chips without making it unrecoverable for the
// person who hid it.
func TestHiddenCityIsAbsentFromThePublicListAndPresentForTheAdmin(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	repo := New(pool)

	c := newCity("semey", "Семей")
	cleanup(t, pool, c.ID)
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	c.IsActive = false
	if err := repo.Update(ctx, c); err != nil {
		t.Fatalf("Update: %v", err)
	}

	public, err := repo.List(ctx, domain.CityFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, got := range public {
		if got.ID == c.ID {
			t.Fatal("a hidden city is still in the public list")
		}
	}

	all, err := repo.List(ctx, domain.CityFilter{IncludeInactive: true})
	if err != nil {
		t.Fatalf("List(admin): %v", err)
	}
	found := false
	for _, got := range all {
		if got.ID == c.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("a hidden city is invisible to the admin too; it could never be brought back")
	}

	// The row itself survived: a hidden city is still referenced by venues.
	if _, err := repo.GetByID(ctx, c.ID); err != nil {
		t.Fatalf("hidden city is gone from the table: %v", err)
	}
}

// TestCityWithVenuesCannotBeDeleted pins the database-level half of "hide
// instead of delete": even a direct DELETE is refused while a venue points at
// the city, so no future script can take the venues' city away with it.
func TestCityWithVenuesCannotBeDeleted(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	testdb.Truncate(t, pool, "restaurants")

	repo := New(pool)
	c := newCity("atyrau", "Атырау")
	cleanup(t, pool, c.ID)
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'Venue','Атырау','₸₸')`,
		uuid.New()); err != nil {
		t.Fatalf("seed venue: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM cities WHERE id = $1`, c.ID); err == nil {
		t.Fatal("a city with venues was deleted; the foreign key must be RESTRICT")
	}
}
