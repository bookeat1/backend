package cuisines

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	cuisinerepo "backend-core/internal/infrastructure/postgres/cuisine"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
	uc "backend-core/internal/usecase/cuisines"
)

// A cuisine created through POST /admin/cuisines must be findable by the
// catalog filter the moment it exists.
//
// Why this test is written at the HTTP level and against a real database: the
// two halves that have to agree live in different packages and neither one is
// wrong on its own. The dictionary write lives in
// infrastructure/postgres/cuisine; the filter
// (restaurant.cuisineMatchExpr) resolves a `?cuisine=` key through
// cuisine_aliases, and a cuisine's own CODE works as a filter key only because
// migrations 0079/0080 seeded it as an alias. The admin handler did not, so
// every cuisine added after those migrations was invisible to a filter by its
// own code — reproduced on production 2026-09-01 with «Индийская» / `indian`:
// zero aliases, zero venues found, while the same venue was found by the
// Russian name (that goes through the legacy cuisine_type branch of the same
// expression).
//
// A unit test with a fake repository cannot catch this: the fake would happily
// "create" the cuisine and the filter would never be involved.

func seedVenueWithLegacyString(t *testing.T, pool *pgxpool.Pool, name, cuisineType string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category, cuisine_type, is_active)
		 VALUES ($1,$2,'Алматы','₸₸',$3,true)`, id, name, cuisineType); err != nil {
		t.Fatalf("seed venue: %v", err)
	}
	return id
}

func TestCuisineCreatedThroughTheAdminRouteIsFoundByItsCode(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_cuisines", "cuisine_aliases", "cuisines", "restaurants")
	ctx := context.Background()

	repo := cuisinerepo.New(pool)
	u := uc.NewUseCase(repo, nil, nil, sqltx.NewManager(pool))
	r := router(u, domain.RoleAdmin)

	// 1. The platform adds a cuisine exactly the way the panel does.
	w := send(t, r, http.MethodPost, "/api/v1/admin/cuisines",
		map[string]any{"code": "indian", "name": "Индийская"}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /admin/cuisines = %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v (body %s)", err, w.Body.String())
	}
	cuisineID, err := uuid.Parse(created.Data.ID)
	if err != nil {
		t.Fatalf("created id %q: %v", created.Data.ID, err)
	}

	// 2. A venue is linked to it, and its legacy cuisine_type carries the
	//    RUSSIAN name — the state the production venue "Taj India" is in.
	venue := seedVenueWithLegacyString(t, pool, "Taj India", "Индийская")
	if err := sqltx.NewManager(pool).WithinTx(ctx, func(ctx context.Context) error {
		return repo.SetForRestaurant(ctx, venue, []uuid.UUID{cuisineID})
	}); err != nil {
		t.Fatalf("link venue to cuisine: %v", err)
	}

	search := restrepo.New(pool)

	// 3. The filter by the cuisine's own CODE — this is what the app sends and
	//    what returned nothing on production.
	items, total, err := search.Search(ctx, domain.RestaurantSearchFilter{Cuisines: []string{"indian"}})
	if err != nil {
		t.Fatalf("search by code: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Restaurant.ID != venue {
		t.Fatalf("search ?cuisine=indian found %d venues (%v), want the linked one — "+
			"a cuisine created through the admin route must be seeded into cuisine_aliases",
			total, items)
	}

	// 4. The filter by the cuisine's NAME must work through the dictionary too,
	//    not only through the venue's legacy string. Checked on a venue that
	//    has NO legacy string at all, so the second branch of the expression
	//    cannot answer for the first one.
	linkOnly := seedVenueWithLegacyString(t, pool, "Spice Room", "")
	if err := sqltx.NewManager(pool).WithinTx(ctx, func(ctx context.Context) error {
		return repo.SetForRestaurant(ctx, linkOnly, []uuid.UUID{cuisineID})
	}); err != nil {
		t.Fatalf("link second venue: %v", err)
	}
	items, total, err = search.Search(ctx, domain.RestaurantSearchFilter{Cuisines: []string{"ИНДИЙСКАЯ"}})
	if err != nil {
		t.Fatalf("search by name: %v", err)
	}
	if total != 2 {
		t.Fatalf("search ?cuisine=ИНДИЙСКАЯ found %d venues, want both linked venues (%v)", total, items)
	}

	// 5. Renaming the entry through the panel. The NEW spelling must start
	//    working and the OLD one must keep working: an app already installed
	//    filters by the chip it scraped yesterday, and the venues' own
	//    cuisine_type strings still spell the old name.
	w = send(t, r, http.MethodPatch, "/api/v1/admin/cuisines/"+cuisineID.String(),
		map[string]any{"code": "indian_cuisine", "name": "Индийская кухня"}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH /admin/cuisines = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	for _, key := range []string{"indian_cuisine", "Индийская кухня", "indian", "Индийская"} {
		_, total, err = search.Search(ctx, domain.RestaurantSearchFilter{Cuisines: []string{key}})
		if err != nil {
			t.Fatalf("search %q: %v", key, err)
		}
		if total != 2 {
			t.Errorf("after the rename, ?cuisine=%q found %d venues, want 2 "+
				"(a renamed cuisine keeps answering to its former spelling)", key, total)
		}
	}
}
