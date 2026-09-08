package cuisines

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

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	uc "backend-core/internal/usecase/cuisines"
)

// --- fakes ------------------------------------------------------------------

type fakeUC struct {
	items    []domain.Cuisine
	created  int
	gotIDs   []uuid.UUID
	gotActor uc.Actor
	err      error
}

func (f *fakeUC) List(_ context.Context, a uc.Actor, _ bool) ([]domain.Cuisine, error) {
	f.gotActor = a
	return f.items, f.err
}

func (f *fakeUC) Create(_ context.Context, a uc.Actor, in uc.SaveInput) (*domain.Cuisine, error) {
	f.gotActor, f.created = a, f.created+1
	if f.err != nil {
		return nil, f.err
	}
	c := domain.Cuisine{ID: uuid.New(), IsActive: true}
	if in.Code != nil {
		c.Code = *in.Code
	}
	if in.Name != nil {
		c.Name = *in.Name
	}
	return &c, nil
}

func (f *fakeUC) Update(_ context.Context, a uc.Actor, id uuid.UUID, _ uc.SaveInput) (*domain.Cuisine, error) {
	f.gotActor = a
	return &domain.Cuisine{ID: id, Code: "european", Name: "Европейская", IsActive: true}, f.err
}

func (f *fakeUC) SetActive(_ context.Context, a uc.Actor, id uuid.UUID, active bool) (*domain.Cuisine, error) {
	f.gotActor = a
	return &domain.Cuisine{ID: id, Code: "european", Name: "Европейская", IsActive: active}, f.err
}

func (f *fakeUC) ForRestaurant(context.Context, uuid.UUID) ([]domain.Cuisine, error) {
	return f.items, f.err
}

func (f *fakeUC) SetForRestaurant(_ context.Context, a uc.Actor, _ uuid.UUID, ids []uuid.UUID) ([]domain.Cuisine, error) {
	f.gotActor, f.gotIDs = a, ids
	return f.items, f.err
}

var _ uc.UseCase = (*fakeUC)(nil)

// --- auth plumbing: the router is built the way bootstrap/app.go mounts these
// routes — the real middleware.Auth plus the real RequireRole(RoleAdmin) — so
// the tests cover the ROUTER gate, not only the handler.

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

// --- tests ------------------------------------------------------------------

// TestVenueStaffCannotManageTheDictionary is the router half of the same rule
// the usecase test pins: the venue picks, the platform decides. Two independent
// gates, because a route re-mounted on a wider group is exactly the mistake
// this feature cannot afford.
func TestVenueStaffCannotManageTheDictionary(t *testing.T) {
	id := uuid.New()
	calls := []struct {
		method, url string
		body        any
	}{
		{http.MethodGet, "/api/v1/admin/cuisines", nil},
		{http.MethodPost, "/api/v1/admin/cuisines", map[string]any{"code": "authors", "name": "Авторская"}},
		{http.MethodPatch, "/api/v1/admin/cuisines/" + id.String(), map[string]any{"name": "Другая"}},
		{http.MethodDelete, "/api/v1/admin/cuisines/" + id.String(), nil},
	}

	for _, role := range []domain.Role{domain.RoleRestaurant, domain.RoleUser} {
		f := &fakeUC{}
		r := router(f, role)
		for _, c := range calls {
			w := send(t, r, c.method, c.url, c.body, true)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s as %q = %d, want 403 (body %s)", c.method, c.url, role, w.Code, w.Body.String())
			}
		}
		if f.created != 0 {
			t.Errorf("as %q the usecase was reached %d times; the gate must stop it at the router", role, f.created)
		}
	}

	// Anonymous: 401, not 403 — a missing token is a different answer from a
	// token that is simply not allowed here.
	f := &fakeUC{}
	r := router(f, domain.RoleAdmin)
	if w := send(t, r, http.MethodPost, "/api/v1/admin/cuisines",
		map[string]any{"code": "authors", "name": "Авторская"}, false); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous POST = %d, want 401", w.Code)
	}

	// And the superadmin does get through, so the test cannot pass by the
	// route simply not existing.
	if w := send(t, r, http.MethodPost, "/api/v1/admin/cuisines",
		map[string]any{"code": "authors", "name": "Авторская"}, true); w.Code != http.StatusCreated {
		t.Errorf("superadmin POST = %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	if f.gotActor.Role != domain.RoleAdmin {
		t.Errorf("actor role reaching the usecase = %q, want admin", f.gotActor.Role)
	}
}

// TestPublicListIsAnonymousAndOrdered: the app fetches the dictionary before
// anyone signs in, so the route must answer without a token, and it must serve
// the order the usecase gave it (the row of chips is not alphabetized twice).
func TestPublicListIsAnonymousAndOrdered(t *testing.T) {
	img := "https://cdn.example/eu.jpg"
	f := &fakeUC{items: []domain.Cuisine{
		{ID: uuid.New(), Code: "european", Name: "Европейская", DisplayOrder: 10, IsActive: true, ImageURL: &img},
		{ID: uuid.New(), Code: "kazakh", Name: "Казахская", NameI18n: domain.I18n{"en": "Kazakh"}, DisplayOrder: 20, IsActive: true},
	}}
	r := router(f, domain.RoleUser)

	w := send(t, r, http.MethodGet, "/api/v1/cuisines", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /cuisines = %d, body %s", w.Code, w.Body.String())
	}
	var env struct {
		Data []cuisineResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(env.Data) != 2 || env.Data[0].Code != "european" {
		t.Fatalf("payload = %+v, want the usecase order preserved", env.Data)
	}
	if env.Data[0].ImageURL == nil || *env.Data[0].ImageURL != img {
		t.Error("image_url is missing: a new cuisine must be able to ship a picture without a store release")
	}

	// ?lang= resolves the localized name while keeping the raw map available.
	w = send(t, r, http.MethodGet, "/api/v1/cuisines?lang=en", nil, false)
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode (en): %v", err)
	}
	if env.Data[1].Name != "Kazakh" {
		t.Errorf("localized name = %q, want %q", env.Data[1].Name, "Kazakh")
	}
	// The entry WITHOUT a translation falls back to the base name rather than
	// showing an empty chip.
	if env.Data[0].Name != "Европейская" {
		t.Errorf("untranslated name = %q, want the Russian base", env.Data[0].Name)
	}
}
