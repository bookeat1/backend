package myrestaurants

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	uc "backend-core/internal/usecase/restaurants"
)

// --- auth plumbing: the router runs the real middleware.Auth, so the access
// token is the user id and the role comes from fakeUsers (mirrors the reviews /
// bookings handler tests).

type fakeIssuer struct{}

func (fakeIssuer) IssueAccess(id uuid.UUID, role string) (string, time.Time, error) {
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

// --- fakes for the two narrow usecase ports (accepted structurally by
// uc.NewMyRestaurantsUseCase, same as the admin.Handler test builds the real
// usecase from fake ports).

type fakeMemberships struct {
	byUser map[uuid.UUID][]domain.StaffMembership
}

func (f fakeMemberships) ListMembershipsByUser(_ context.Context, id uuid.UUID) ([]domain.StaffMembership, error) {
	return f.byUser[id], nil
}

type fakeBriefs struct{ all []domain.RestaurantBrief }

func (f fakeBriefs) ListManageableBrief(context.Context) ([]domain.RestaurantBrief, error) {
	return f.all, nil
}

func newRouter(role domain.Role, m fakeMemberships, b fakeBriefs) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	usecase := uc.NewMyRestaurantsUseCase(m, b)
	h := NewHandler(usecase)
	authed := r.Group("/api/v1")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: role}))
	h.RegisterRoutes(authed)
	return r
}

type envelope struct {
	Data struct {
		Restaurants []restaurantResponse `json:"restaurants"`
	} `json:"data"`
	Error string `json:"error"`
}

func get(r *gin.Engine, path string, token *uuid.UUID, lang string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != nil {
		req.Header.Set("Authorization", "Bearer "+token.String())
	}
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestMyRestaurants_Unauthorized(t *testing.T) {
	r := newRouter(domain.RoleUser, fakeMemberships{}, fakeBriefs{})
	w := get(r, "/api/v1/admin/my-restaurants", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", w.Code)
	}
}

func TestMyRestaurants_StaffHappyPath_Localized(t *testing.T) {
	user := uuid.New()
	restA := uuid.New()
	m := fakeMemberships{byUser: map[uuid.UUID][]domain.StaffMembership{
		user: {{
			RestaurantID: restA, Name: "Альфа", NameI18n: domain.I18n{"en": "Alpha"},
			Role: domain.StaffRoleOwner,
		}},
	}}
	r := newRouter(domain.RoleRestaurant, m, fakeBriefs{})

	w := get(r, "/api/v1/admin/my-restaurants", &user, "en")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data.Restaurants) != 1 {
		t.Fatalf("restaurants = %+v, want 1", env.Data.Restaurants)
	}
	got := env.Data.Restaurants[0]
	if got.ID != restA || got.Role != "owner" {
		t.Errorf("entry = %+v, want id=%s role=owner", got, restA)
	}
	if got.Name != "Alpha" {
		t.Errorf("name = %q, want localized 'Alpha' (Accept-Language: en)", got.Name)
	}
}

func TestMyRestaurants_NoMembership_EmptyArray(t *testing.T) {
	user := uuid.New()
	r := newRouter(domain.RoleUser, fakeMemberships{byUser: map[uuid.UUID][]domain.StaffMembership{}}, fakeBriefs{})

	w := get(r, "/api/v1/admin/my-restaurants", &user, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// Must serialize as [] (non-null) so the client can iterate unconditionally.
	if body := w.Body.String(); body == "" || !containsEmptyArray(body) {
		t.Fatalf("body = %s, want restaurants: []", body)
	}
}

func containsEmptyArray(body string) bool {
	var env envelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return false
	}
	return env.Data.Restaurants != nil && len(env.Data.Restaurants) == 0
}

func TestMyRestaurants_Superadmin_AllVenues(t *testing.T) {
	admin := uuid.New()
	restA, restB := uuid.New(), uuid.New()
	b := fakeBriefs{all: []domain.RestaurantBrief{{ID: restA, Name: "Alpha"}, {ID: restB, Name: "Bravo"}}}
	// No memberships for the admin: the result must come from the brief reader.
	r := newRouter(domain.RoleAdmin, fakeMemberships{}, b)

	w := get(r, "/api/v1/admin/my-restaurants", &admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data.Restaurants) != 2 {
		t.Fatalf("restaurants = %+v, want 2 venues for superadmin", env.Data.Restaurants)
	}
	for _, it := range env.Data.Restaurants {
		if it.Role != string(domain.RoleAdmin) {
			t.Errorf("role = %q, want %q", it.Role, domain.RoleAdmin)
		}
	}
}
