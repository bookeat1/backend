package foodieoptions

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
	uc "backend-core/internal/usecase/foodieoptions"
)

// --- fakes -------------------------------------------------------------

type fakeUC struct {
	items    []domain.FoodieOption
	created  int
	gotActor uc.Actor
	gotInput uc.SaveInput
	err      error
}

func (f *fakeUC) List(_ context.Context, a uc.Actor, _ bool) ([]domain.FoodieOption, error) {
	f.gotActor = a
	return f.items, f.err
}

func (f *fakeUC) Create(_ context.Context, a uc.Actor, in uc.SaveInput) (*domain.FoodieOption, error) {
	f.gotActor, f.gotInput, f.created = a, in, f.created+1
	if f.err != nil {
		return nil, f.err
	}
	o := domain.FoodieOption{ID: uuid.New(), IsActive: true}
	if in.Kind != nil {
		o.Kind = *in.Kind
	}
	if in.Code != nil {
		o.Code = *in.Code
	}
	if in.Name != nil {
		o.Name = *in.Name
	}
	return &o, nil
}

func (f *fakeUC) Update(_ context.Context, a uc.Actor, id uuid.UUID, in uc.SaveInput) (*domain.FoodieOption, error) {
	f.gotActor, f.gotInput = a, in
	return &domain.FoodieOption{ID: id, Kind: domain.FoodieOptionKindDiet, Code: "halal", Name: "Халяль", IsActive: true}, f.err
}

func (f *fakeUC) SetActive(_ context.Context, a uc.Actor, id uuid.UUID, active bool) (*domain.FoodieOption, error) {
	f.gotActor = a
	return &domain.FoodieOption{ID: id, Kind: domain.FoodieOptionKindDiet, Code: "halal", Name: "Халяль", IsActive: active}, f.err
}

var _ uc.UseCase = (*fakeUC)(nil)

// --- auth plumbing: same shape as transport/rest/cuisines.handler_test.go —
// the real middleware.Auth + RequireRole(RoleAdmin), so the tests cover the
// ROUTER gate, not only the handler.

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

// --- tests ---------------------------------------------------------------

// TestVenueStaffCannotManageTheDictionary is the router half of the platform
// gate: a venue manager or a plain guest must never reach the usecase, and
// an anonymous caller gets 401, not 403.
func TestVenueStaffCannotManageTheDictionary(t *testing.T) {
	id := uuid.New()
	calls := []struct {
		method, url string
		body        any
	}{
		{http.MethodGet, "/api/v1/admin/foodie-profile/options", nil},
		{http.MethodPost, "/api/v1/admin/foodie-profile/options", map[string]any{"kind": "diet", "code": "kosher", "name": "Кошерное"}},
		{http.MethodPatch, "/api/v1/admin/foodie-profile/options/" + id.String(), map[string]any{"name": "Другое"}},
		{http.MethodDelete, "/api/v1/admin/foodie-profile/options/" + id.String(), nil},
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

	f := &fakeUC{}
	r := router(f, domain.RoleAdmin)
	if w := send(t, r, http.MethodPost, "/api/v1/admin/foodie-profile/options",
		map[string]any{"kind": "diet", "code": "kosher", "name": "Кошерное"}, false); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous POST = %d, want 401", w.Code)
	}

	if w := send(t, r, http.MethodPost, "/api/v1/admin/foodie-profile/options",
		map[string]any{"kind": "diet", "code": "kosher", "name": "Кошерное"}, true); w.Code != http.StatusCreated {
		t.Errorf("superadmin POST = %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	if f.gotActor.Role != domain.RoleAdmin {
		t.Errorf("actor role reaching the usecase = %q, want admin", f.gotActor.Role)
	}
}

// TestPublicListIsAnonymousAndBucketed pins the public contract shape (spec
// §5): four buckets, each item carrying name/name_i18n/image_url/
// display_order, budget items additionally carrying description/price_label/
// price_category.
func TestPublicListIsAnonymousAndBucketed(t *testing.T) {
	img := "https://cdn.example/korean.jpg"
	priceLabel := "до 5 000 ₸"
	priceCat := domain.PriceLow
	f := &fakeUC{items: []domain.FoodieOption{
		{ID: uuid.New(), Kind: domain.FoodieOptionKindCuisine, Code: "korean", Name: "Корейская", DisplayOrder: 10, IsActive: true, ImageURL: &img},
		{ID: uuid.New(), Kind: domain.FoodieOptionKindDiet, Code: "halal", Name: "Халяль", NameI18n: domain.I18n{"en": "Halal"}, DisplayOrder: 10, IsActive: true},
		{ID: uuid.New(), Kind: domain.FoodieOptionKindAllergy, Code: "nuts", Name: "Орехи", DisplayOrder: 10, IsActive: true},
		{
			ID: uuid.New(), Kind: domain.FoodieOptionKindBudget, Code: "budget", Name: "Бюджетный", DisplayOrder: 10, IsActive: true,
			PriceLabel: &priceLabel, PriceCategory: &priceCat,
		},
	}}
	r := router(f, domain.RoleUser)

	w := send(t, r, http.MethodGet, "/api/v1/foodie-profile/options", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /foodie-profile/options = %d, body %s", w.Code, w.Body.String())
	}
	var env struct {
		Data publicOptionsResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(env.Data.Cuisines) != 1 || env.Data.Cuisines[0].Code != "korean" {
		t.Fatalf("cuisines = %+v, want [korean]", env.Data.Cuisines)
	}
	if env.Data.Cuisines[0].ImageURL == nil || *env.Data.Cuisines[0].ImageURL != img {
		t.Error("image_url missing on the cuisine bucket item")
	}
	if len(env.Data.Diets) != 1 || len(env.Data.Allergies) != 1 || len(env.Data.Budgets) != 1 {
		t.Fatalf("buckets = %+v, want one entry each", env.Data)
	}
	budget := env.Data.Budgets[0]
	if budget.PriceLabel == nil || *budget.PriceLabel != priceLabel {
		t.Errorf("budget price_label = %v, want %q", budget.PriceLabel, priceLabel)
	}
	if budget.PriceCategory == nil || *budget.PriceCategory != string(domain.PriceLow) {
		t.Errorf("budget price_category = %v, want %q", budget.PriceCategory, domain.PriceLow)
	}
	// A non-budget item must never leak the budget-only fields.
	if env.Data.Cuisines[0].PriceCategory != nil {
		t.Error("cuisine item carries price_category, want nil")
	}

	// includeInactive is never honoured on the public route — even if the
	// usecase were misconfigured, the handler always calls List(..., false).
	if f.gotActor.Role != "" {
		t.Errorf("anonymous actor role = %q, want empty", f.gotActor.Role)
	}
}

// TestUpdateRejectsImmutableFieldChange exercises the wire-to-usecase path
// for criterion 7: the handler must let the usecase's ErrValidation surface
// as 422, not swallow or remap it.
func TestUpdateRejectsImmutableFieldChange(t *testing.T) {
	f := &fakeUC{err: domain.ErrValidation}
	r := router(f, domain.RoleAdmin)
	id := uuid.New()
	w := send(t, r, http.MethodPatch, "/api/v1/admin/foodie-profile/options/"+id.String(),
		map[string]any{"kind": "budget"}, true)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("PATCH with a rejected kind change = %d, want 422 (body %s)", w.Code, w.Body.String())
	}
}

// TestAdminListIncludesHiddenAndExtraFields pins criterion 5's admin-only
// fields.
func TestAdminListIncludesHiddenAndExtraFields(t *testing.T) {
	korean := uuid.New()
	f := &fakeUC{items: []domain.FoodieOption{
		{
			ID: korean, Kind: domain.FoodieOptionKindCuisine, Code: "korean", Name: "Корейская",
			DisplayOrder: 10, IsActive: false,
			Cuisines: []domain.Cuisine{{ID: uuid.New(), Code: "korean", Name: "Корейская", IsActive: true}},
		},
	}}
	r := router(f, domain.RoleAdmin)

	w := send(t, r, http.MethodGet, "/api/v1/admin/foodie-profile/options", nil, true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET admin list = %d, body %s", w.Code, w.Body.String())
	}
	var env struct {
		Data adminOptionsResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data.Cuisines) != 1 {
		t.Fatalf("admin list must include the hidden entry, got %+v", env.Data)
	}
	got := env.Data.Cuisines[0]
	if got.IsActive {
		t.Error("is_active = true, want false (it is hidden)")
	}
	if !got.AffectsMatching {
		t.Error("affects_matching = false, want true (it has a cuisine link)")
	}
	if len(got.CuisineIDs) != 1 || len(got.CuisineCodes) != 1 || got.CuisineCodes[0] != "korean" {
		t.Errorf("cuisine_ids/cuisine_codes = %v/%v, want one korean entry", got.CuisineIDs, got.CuisineCodes)
	}
	if f.gotActor.Role != domain.RoleAdmin {
		t.Errorf("actor role reaching the usecase = %q, want admin", f.gotActor.Role)
	}
}
