package venuefeatures

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	featrepo "backend-core/internal/infrastructure/postgres/venuefeature"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/transport/rest/middleware"
	uc "backend-core/internal/usecase/venuefeatures"
)

// A venue feature created through POST /admin/venue-features must be findable
// by the catalog filter the moment it exists.
//
// Why this test is written at the HTTP level and against a real database: the
// two halves that have to agree live in different packages and neither one is
// wrong on its own. The dictionary write lives in
// infrastructure/postgres/venuefeature; the filter
// (restaurant.featureVenueMatchExpr) resolves a `?features=` key through
// venue_feature_aliases, and a feature's own CODE and NAME work as filter keys
// only because migration 0082 seeded them as aliases. The admin handler did
// not, so every feature added after that migration was invisible to the
// filter.
//
// This is WORSE than the identical cuisine bug (fixed in PR #113): the cuisine
// expression has a fallback branch over the venue's legacy cuisine_type
// string, so a new cuisine was at least findable by its Russian name. The
// feature expression consists of the alias branch ONLY, so an alias-less
// feature is invisible completely — by code and by name alike.
//
// A unit test with a fake repository cannot catch this: the fake would happily
// "create" the feature and the filter would never be involved.

// --- auth plumbing: the router is built the way bootstrap/app.go mounts these
// routes — the real middleware.Auth plus the real RequireRole(RoleAdmin) — so
// the test covers the ROUTER gate, not only the handler. Same fakes as the
// cuisines package next door.

type fakeIssuer struct{}

func (fakeIssuer) IssueAccess(id uuid.UUID, _ string) (string, time.Time, error) {
	return id.String(), time.Now().Add(time.Hour), nil
}

func (fakeIssuer) ParseAccess(token string) (uuid.UUID, string, error) {
	id, err := uuid.Parse(token)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("bad token")
	}
	return id, "", nil
}

type fakeUsers struct{ role domain.Role }

func (f fakeUsers) Create(context.Context, *domain.User) error { return nil }
func (f fakeUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	return &domain.User{ID: id, Role: f.role, IsActive: true}, nil
}
func (f fakeUsers) GetByEmail(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (f fakeUsers) GetByPhone(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (f fakeUsers) Update(context.Context, *domain.User) error { return nil }
func (f fakeUsers) Delete(context.Context, uuid.UUID) error    { return nil }

func router(u uc.UseCase, role domain.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	h := NewHandler(u)
	h.RegisterPublic(api)
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: role}))
	adminGlobal := authed.Group("")
	adminGlobal.Use(middleware.RequireRole(domain.RoleAdmin))
	h.RegisterAdminGlobal(adminGlobal)
	return r
}

func send(t *testing.T, r *gin.Engine, method, url string, body any, authed bool) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		payload = b
	}
	req := httptest.NewRequest(method, url, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if authed {
		req.Header.Set("Authorization", "Bearer "+uuid.NewString())
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func seedVenue(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category, is_active)
		 VALUES ($1,$2,'Алматы','₸₸',true)`, id, name); err != nil {
		t.Fatalf("seed venue: %v", err)
	}
	return id
}

func TestFeatureCreatedThroughTheAdminRouteIsFoundByTheCatalogFilter(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurant_venue_features", "venue_feature_aliases", "venue_features", "restaurants")
	ctx := context.Background()

	repo := featrepo.New(pool)
	u := uc.NewUseCase(repo, nil, sqltx.NewManager(pool))
	r := router(u, domain.RoleAdmin)

	// 1. The platform adds a feature exactly the way the panel does.
	w := send(t, r, http.MethodPost, "/api/v1/admin/venue-features",
		map[string]any{"code": "coworking", "name": "Коворкинг"}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /admin/venue-features = %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v (body %s)", err, w.Body.String())
	}
	featureID, err := uuid.Parse(created.Data.ID)
	if err != nil {
		t.Fatalf("created id %q: %v", created.Data.ID, err)
	}

	// 2. A venue is linked to it.
	venue := seedVenue(t, pool, "Рабочее место")
	if err := sqltx.NewManager(pool).WithinTx(ctx, func(ctx context.Context) error {
		return repo.SetForRestaurant(ctx, venue, []uuid.UUID{featureID})
	}); err != nil {
		t.Fatalf("link venue to feature: %v", err)
	}

	search := restrepo.New(pool)

	// 3. The filter by the feature's own CODE — what the app's filter sheet
	//    sends — and by its NAME. For features there is no fallback branch at
	//    all, so on the pre-fix code BOTH lookups return nothing.
	for _, key := range []string{"coworking", "Коворкинг"} {
		items, total, err := search.Search(ctx, domain.RestaurantSearchFilter{Features: []string{key}})
		if err != nil {
			t.Fatalf("search by %q: %v", key, err)
		}
		if total != 1 || len(items) != 1 || items[0].Restaurant.ID != venue {
			t.Fatalf("search ?features=%q found %d venues (%v), want the linked one — "+
				"a feature created through the admin route must be seeded into venue_feature_aliases",
				key, total, items)
		}
	}

	// 4. Renaming the entry through the panel. The NEW spelling must start
	//    working and the OLD one must keep working: an app already installed
	//    filters by the chip it scraped yesterday.
	w = send(t, r, http.MethodPatch, "/api/v1/admin/venue-features/"+featureID.String(),
		map[string]any{"code": "coworking_zone", "name": "Коворкинг-зона"}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH /admin/venue-features = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	for _, key := range []string{"coworking_zone", "Коворкинг-зона", "coworking", "Коворкинг"} {
		_, total, err := search.Search(ctx, domain.RestaurantSearchFilter{Features: []string{key}})
		if err != nil {
			t.Fatalf("search %q: %v", key, err)
		}
		if total != 1 {
			t.Errorf("after the rename, ?features=%q found %d venues, want 1 "+
				"(a renamed feature keeps answering to its former spelling)", key, total)
		}
	}
}
