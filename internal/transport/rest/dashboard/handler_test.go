package dashboard

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
	uc "backend-core/internal/usecase/dashboard"
)

// --- auth plumbing: the router runs the real middleware.Auth + RequireRole, so
// the access token is the user id and the role comes from fakeUsers (mirrors the
// myrestaurants / reviews handler tests).

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

// fakeRepo satisfies the usecase's readRepo port structurally.
type fakeRepo struct {
	overview domain.PlatformOverview
	counts   []domain.BookingStatusCount
	captured domain.MoneyAggregate
	refunded domain.MoneyAggregate
	top      []domain.TopRestaurant
}

func (f fakeRepo) Overview(context.Context) (domain.PlatformOverview, error) {
	return f.overview, nil
}
func (f fakeRepo) BookingsByStatus(context.Context, any, any) ([]domain.BookingStatusCount, error) {
	return f.counts, nil
}
func (f fakeRepo) PaymentsGMV(context.Context, any, any, string) (domain.MoneyAggregate, domain.MoneyAggregate, error) {
	return f.captured, f.refunded, nil
}
func (f fakeRepo) TopRestaurantsByBookings(context.Context, any, any, int) ([]domain.TopRestaurant, error) {
	return f.top, nil
}
func (f fakeRepo) TopRestaurantsByGMV(context.Context, any, any, string, int) ([]domain.TopRestaurant, error) {
	return f.top, nil
}

// newRouter builds the router the SAME way bootstrap/app.go mounts the
// dashboard: authed (real middleware.Auth) → RequireRole(RoleAdmin) → handler.
func newRouter(role domain.Role, repo fakeRepo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: role}))
	adminGlobal := authed.Group("")
	adminGlobal.Use(middleware.RequireRole(domain.RoleAdmin))
	NewHandler(uc.NewUseCase(repo)).RegisterRoutes(adminGlobal)
	return r
}

func get(r *gin.Engine, path string, token *uuid.UUID) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != nil {
		req.Header.Set("Authorization", "Bearer "+token.String())
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

var endpoints = []string{
	"/api/v1/admin/dashboard/overview",
	"/api/v1/admin/dashboard/bookings",
	"/api/v1/admin/dashboard/payments",
	"/api/v1/admin/dashboard/top-restaurants",
}

// Every non-superadmin role is 403 on EVERY dashboard endpoint.
func TestNonSuperadminForbiddenOnEveryEndpoint(t *testing.T) {
	roles := []domain.Role{domain.RoleUser, domain.RoleRestaurant}
	for _, role := range roles {
		r := newRouter(role, fakeRepo{})
		id := uuid.New()
		for _, ep := range endpoints {
			w := get(r, ep, &id)
			if w.Code != http.StatusForbidden {
				t.Fatalf("role %q %s: got %d, want 403", role, ep, w.Code)
			}
		}
	}
}

// No token → 401 on every endpoint.
func TestUnauthenticatedOnEveryEndpoint(t *testing.T) {
	r := newRouter(domain.RoleAdmin, fakeRepo{})
	for _, ep := range endpoints {
		w := get(r, ep, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s no token: got %d, want 401", ep, w.Code)
		}
	}
}

func TestOverviewSuperadmin(t *testing.T) {
	repo := fakeRepo{overview: domain.PlatformOverview{
		TotalRestaurants: 12, ActiveRestaurants: 9, TotalUsers: 340,
		TotalBookings: 1500, BookingsLast7Days: 40, BookingsLast30Days: 210,
	}}
	r := newRouter(domain.RoleAdmin, repo)
	id := uuid.New()
	w := get(r, "/api/v1/admin/dashboard/overview", &id)
	if w.Code != http.StatusOK {
		t.Fatalf("overview: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			TotalRestaurants   int64 `json:"total_restaurants"`
			ActiveRestaurants  int64 `json:"active_restaurants"`
			TotalUsers         int64 `json:"total_users"`
			TotalBookings      int64 `json:"total_bookings"`
			BookingsLast7Days  int64 `json:"bookings_last_7_days"`
			BookingsLast30Days int64 `json:"bookings_last_30_days"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.TotalRestaurants != 12 || env.Data.ActiveRestaurants != 9 ||
		env.Data.TotalUsers != 340 || env.Data.TotalBookings != 1500 ||
		env.Data.BookingsLast7Days != 40 || env.Data.BookingsLast30Days != 210 {
		t.Fatalf("overview payload wrong: %+v", env.Data)
	}
}

func TestPaymentsSuperadminMinorUnits(t *testing.T) {
	repo := fakeRepo{
		captured: domain.MoneyAggregate{AmountMinor: 750000, Count: 6},
		refunded: domain.MoneyAggregate{AmountMinor: 90000, Count: 2},
	}
	r := newRouter(domain.RoleAdmin, repo)
	id := uuid.New()
	w := get(r, "/api/v1/admin/dashboard/payments", &id)
	if w.Code != http.StatusOK {
		t.Fatalf("payments: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Currency string `json:"currency"`
			Captured struct {
				AmountMinor int64 `json:"amount_minor"`
				Count       int64 `json:"count"`
			} `json:"captured"`
			Refunded struct {
				AmountMinor int64 `json:"amount_minor"`
				Count       int64 `json:"count"`
			} `json:"refunded"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Currency != "KZT" {
		t.Fatalf("currency: got %q, want KZT", env.Data.Currency)
	}
	if env.Data.Captured.AmountMinor != 750000 || env.Data.Captured.Count != 6 {
		t.Fatalf("captured: %+v", env.Data.Captured)
	}
	if env.Data.Refunded.AmountMinor != 90000 || env.Data.Refunded.Count != 2 {
		t.Fatalf("refunded: %+v", env.Data.Refunded)
	}
}

func TestBookingsInvalidPeriodIs422(t *testing.T) {
	r := newRouter(domain.RoleAdmin, fakeRepo{})
	id := uuid.New()
	// Unparseable "from" → 422 (transport rejects before the usecase).
	w := get(r, "/api/v1/admin/dashboard/bookings?from=not-a-date", &id)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad from: got %d, want 422", w.Code)
	}
	// Inverted window from>to → usecase ErrValidation → 422.
	w = get(r, "/api/v1/admin/dashboard/bookings?from=2026-07-10&to=2026-07-01", &id)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("inverted window: got %d, want 422", w.Code)
	}
}

func TestTopRestaurantsLocalizedName(t *testing.T) {
	rid := uuid.New()
	repo := fakeRepo{top: []domain.TopRestaurant{
		{RestaurantID: rid, Name: "Базовое", NameI18n: domain.I18n{"en": "Basil"}, BookingsCount: 33},
	}}
	r := newRouter(domain.RoleAdmin, repo)
	id := uuid.New()
	w := get(r, "/api/v1/admin/dashboard/top-restaurants?lang=en", &id)
	if w.Code != http.StatusOK {
		t.Fatalf("top: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Restaurants []struct {
				RestaurantID  uuid.UUID `json:"restaurant_id"`
				Name          string    `json:"name"`
				BookingsCount int64     `json:"bookings_count"`
			} `json:"restaurants"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data.Restaurants) != 1 {
		t.Fatalf("restaurants len: got %d, want 1", len(env.Data.Restaurants))
	}
	got := env.Data.Restaurants[0]
	if got.Name != "Basil" {
		t.Fatalf("localized name: got %q, want Basil", got.Name)
	}
	if got.BookingsCount != 33 || got.RestaurantID != rid {
		t.Fatalf("row wrong: %+v", got)
	}
}
