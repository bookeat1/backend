package homepicks

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

// What this repository promises is a property of SQL — an order that is really
// stored, a replacement that is all-or-nothing, a row that leaves with its
// venue. None of it can be checked against a fake, so these run on a real
// Postgres (skipped in -short / without TEST_DATABASE_URL, like every other
// integration test here).

func setup(t *testing.T) (*pgxpool.Pool, *Repository, context.Context) {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "home_picks", "restaurants")
	return pool, New(pool, sqltx.NewManager(pool)), context.Background()
}

func seedVenue(t *testing.T, pool *pgxpool.Pool, ctx context.Context, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, is_active)
		 VALUES ($1, $2, $3, $4, true)`,
		id, name, string(domain.CityAlmaty), string(domain.PriceMid)); err != nil {
		t.Fatalf("seed venue %s: %v", name, err)
	}
	return id
}

func TestListIDsKeepsTheStoredOrder(t *testing.T) {
	pool, repo, ctx := setup(t)
	a := seedVenue(t, pool, ctx, "А")
	b := seedVenue(t, pool, ctx, "Б")
	c := seedVenue(t, pool, ctx, "В")

	if err := repo.Replace(ctx, "Алматы", []uuid.UUID{c, a, b}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := repo.ListIDs(ctx, "Алматы")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []uuid.UUID{c, a, b}
	if len(got) != len(want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("list = %v, want %v", got, want)
		}
	}
}

// The positions really are 1..n, not "whatever the insert order happened to
// be": the read's ORDER BY is only worth something if the numbers under it are.
func TestReplaceStoresConsecutivePositions(t *testing.T) {
	pool, repo, ctx := setup(t)
	a := seedVenue(t, pool, ctx, "А")
	b := seedVenue(t, pool, ctx, "Б")

	if err := repo.Replace(ctx, "Алматы", []uuid.UUID{b, a}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got := map[uuid.UUID]int{}
	rows, err := pool.Query(ctx, `SELECT restaurant_id, position FROM home_picks WHERE city = 'Алматы'`)
	if err != nil {
		t.Fatalf("read positions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var pos int
		if err := rows.Scan(&id, &pos); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = pos
	}
	if got[b] != 1 || got[a] != 2 {
		t.Fatalf("positions = %v, want b=1 a=2", got)
	}
}

// The reorder case the deferred unique constraint exists for: the new order
// reuses the same slots as the old one, so a non-deferred constraint would trip
// on the intermediate state.
func TestReplaceCanSwapTwoVenuesRoundInOneCall(t *testing.T) {
	pool, repo, ctx := setup(t)
	a := seedVenue(t, pool, ctx, "А")
	b := seedVenue(t, pool, ctx, "Б")

	if err := repo.Replace(ctx, "Алматы", []uuid.UUID{a, b}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if err := repo.Replace(ctx, "Алматы", []uuid.UUID{b, a}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	got, err := repo.ListIDs(ctx, "Алматы")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0] != b || got[1] != a {
		t.Fatalf("after the swap = %v, want [b a]", got)
	}
}

// Replacement is a REPLACEMENT: what was there and is not named any more is
// gone, not merged with the new list.
func TestReplaceDropsWhatIsNoLongerNamed(t *testing.T) {
	pool, repo, ctx := setup(t)
	a := seedVenue(t, pool, ctx, "А")
	b := seedVenue(t, pool, ctx, "Б")

	if err := repo.Replace(ctx, "Алматы", []uuid.UUID{a, b}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if err := repo.Replace(ctx, "Алматы", []uuid.UUID{a}); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	got, err := repo.ListIDs(ctx, "Алматы")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0] != a {
		t.Fatalf("list = %v, want just a", got)
	}
}

// Clearing is the "back to automatic" switch, and it must leave nothing behind.
func TestReplaceWithNothingClearsTheCity(t *testing.T) {
	pool, repo, ctx := setup(t)
	a := seedVenue(t, pool, ctx, "А")

	if err := repo.Replace(ctx, "Алматы", []uuid.UUID{a}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := repo.Replace(ctx, "Алматы", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err := repo.ListIDs(ctx, "Алматы")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("list = %v, want empty", got)
	}
}

// Cities are independent keys, and the all-cities key (”) is just another one
// of them — writing one must not touch another.
func TestCitiesAndTheAllCitiesListAreIndependent(t *testing.T) {
	pool, repo, ctx := setup(t)
	a := seedVenue(t, pool, ctx, "А")
	b := seedVenue(t, pool, ctx, "Б")

	if err := repo.Replace(ctx, "Алматы", []uuid.UUID{a}); err != nil {
		t.Fatalf("replace almaty: %v", err)
	}
	if err := repo.Replace(ctx, domain.HomePicksAllCities, []uuid.UUID{b}); err != nil {
		t.Fatalf("replace all-cities: %v", err)
	}
	if err := repo.Replace(ctx, "Алматы", nil); err != nil {
		t.Fatalf("clear almaty: %v", err)
	}
	shared, err := repo.ListIDs(ctx, domain.HomePicksAllCities)
	if err != nil {
		t.Fatalf("list all-cities: %v", err)
	}
	if len(shared) != 1 || shared[0] != b {
		t.Fatalf("all-cities list = %v, want just b — clearing a city must not touch it", shared)
	}
}

// The all-cities key is a stored ” rather than NULL precisely so the primary
// key and the position uniqueness protect it like any other city. This is that
// claim, checked against the database rather than against the migration's
// comment.
func TestTheAllCitiesListIsProtectedByTheSameConstraints(t *testing.T) {
	pool, repo, ctx := setup(t)
	a := seedVenue(t, pool, ctx, "А")
	b := seedVenue(t, pool, ctx, "Б")

	if err := repo.Replace(ctx, domain.HomePicksAllCities, []uuid.UUID{a}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO home_picks (city, restaurant_id, position) VALUES ('', $1, 1)`, a); err == nil {
		t.Fatal("the same venue twice in the all-cities list must be refused")
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO home_picks (city, restaurant_id, position) VALUES ('', $1, 1)`, b); err == nil {
		t.Fatal("two venues on the same slot of the all-cities list must be refused")
	}
}

// A deleted venue takes its membership with it, so no read ever has to filter a
// dangling row.
func TestDeletingAVenueRemovesItFromTheList(t *testing.T) {
	pool, repo, ctx := setup(t)
	a := seedVenue(t, pool, ctx, "А")
	b := seedVenue(t, pool, ctx, "Б")

	if err := repo.Replace(ctx, "Алматы", []uuid.UUID{a, b}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM restaurants WHERE id = $1`, a); err != nil {
		t.Fatalf("delete venue: %v", err)
	}
	got, err := repo.ListIDs(ctx, "Алматы")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0] != b {
		t.Fatalf("list = %v, want just b", got)
	}
}

// A venue that does not exist is a 404, and — because the whole replacement is
// one transaction — the list that was there is still there afterwards. A
// half-written rail would be far worse than a refused save.
func TestReplaceWithAnUnknownVenueChangesNothing(t *testing.T) {
	pool, repo, ctx := setup(t)
	a := seedVenue(t, pool, ctx, "А")

	if err := repo.Replace(ctx, "Алматы", []uuid.UUID{a}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	err := repo.Replace(ctx, "Алматы", []uuid.UUID{a, uuid.New()})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	got, lerr := repo.ListIDs(ctx, "Алматы")
	if lerr != nil {
		t.Fatalf("list: %v", lerr)
	}
	if len(got) != 1 || got[0] != a {
		t.Fatalf("list = %v, want the previous list intact", got)
	}
}

func TestListIDsOfAnUncuratedCityIsEmptyNotAnError(t *testing.T) {
	_, repo, ctx := setup(t)
	got, err := repo.ListIDs(ctx, "Караганда")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("list = %v, want empty", got)
	}
}
