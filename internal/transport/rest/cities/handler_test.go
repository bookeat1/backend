package cities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	uc "backend-core/internal/usecase/cities"
)

// --- fakes ------------------------------------------------------------------

type fakeUC struct {
	items    []domain.CityEntry
	created  int
	hidden   []uuid.UUID
	deleted  int
	gotIDs   []uuid.UUID
	gotActor uc.Actor
	err      error
}

func (f *fakeUC) List(_ context.Context, a uc.Actor, includeInactive bool) ([]domain.CityEntry, error) {
	f.gotActor = a
	if f.err != nil {
		return nil, f.err
	}
	if includeInactive {
		return f.items, nil
	}
	out := make([]domain.CityEntry, 0, len(f.items))
	for _, c := range f.items {
		if c.IsActive {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeUC) Create(_ context.Context, a uc.Actor, in uc.SaveInput) (*domain.CityEntry, error) {
	f.gotActor, f.created = a, f.created+1
	if f.err != nil {
		return nil, f.err
	}
	c := domain.CityEntry{ID: uuid.New(), IsActive: true}
	if in.Code != nil {
		c.Code = *in.Code
	}
	if in.Name != nil {
		c.Name = *in.Name
	}
	return &c, nil
}

func (f *fakeUC) Update(_ context.Context, a uc.Actor, id uuid.UUID, _ uc.SaveInput) (*domain.CityEntry, error) {
	f.gotActor = a
	return &domain.CityEntry{ID: id, Code: "almaty", Name: "Алматы", IsActive: true}, f.err
}

func (f *fakeUC) SetActive(_ context.Context, a uc.Actor, id uuid.UUID, active bool) (*domain.CityEntry, error) {
	f.gotActor = a
	if !active {
		f.hidden = append(f.hidden, id)
	}
	return &domain.CityEntry{ID: id, Code: "almaty", Name: "Алматы", IsActive: active}, f.err
}

func (f *fakeUC) Reorder(_ context.Context, a uc.Actor, ids []uuid.UUID) ([]domain.CityEntry, error) {
	f.gotActor, f.gotIDs = a, ids
	return f.items, f.err
}

func (f *fakeUC) AddAlias(_ context.Context, a uc.Actor, id uuid.UUID, _ string) (*domain.CityEntry, error) {
	f.gotActor = a
	return &domain.CityEntry{ID: id, Code: "astana", Name: "Астана", IsActive: true}, f.err
}

func (f *fakeUC) Resolve(context.Context, string) (*domain.CityEntry, error) { return nil, f.err }

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

func seed() []domain.CityEntry {
	return []domain.CityEntry{
		{ID: uuid.New(), Code: "astana", Name: "Астана",
			NameI18n: domain.I18n{"en": "Astana", "kk": "Астана"}, DisplayOrder: 10, IsActive: true},
		{ID: uuid.New(), Code: "almaty", Name: "Алматы",
			NameI18n: domain.I18n{"en": "Almaty", "kk": "Алматы"}, DisplayOrder: 20, IsActive: true},
		{ID: uuid.New(), Code: "shymkent", Name: "Шымкент", DisplayOrder: 30, IsActive: false},
	}
}

// --- tests ------------------------------------------------------------------

// TestLegacyCitiesBodyIsUnchanged is the single most important test in this
// package. The build in the store parses GET /cities as a bare array of city
// NAMES inside the standard envelope, and it will keep doing so for as long as
// people postpone updates. The assertion is on the exact JSON bytes, not on a
// decoded structure: a change from ["Астана"] to [{"name":"Астана"}] decodes
// fine into `any` and breaks every phone out there.
func TestLegacyCitiesBodyIsUnchanged(t *testing.T) {
	f := &fakeUC{items: seed()}
	r := router(f, domain.RoleUser)

	w := send(t, r, http.MethodGet, "/api/v1/cities", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /cities = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	// TrimSpace only strips the encoder's trailing newline, which is what the
	// route has always emitted; every other byte is asserted literally.
	const want = `{"data":["Астана","Алматы"]}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("GET /cities body = %s, want %s (this is the frozen contract of the store build)", got, want)
	}
}

// TestLegacyCitiesBodyIgnoresLocale pins the second half of that contract: the
// names in the legacy body are the BASE Russian ones whatever language the
// client asks for, because the client sends the same string straight back as
// ?city= and the catalog compares it to the stored restaurants.city column.
// Translating them here would produce a filter that silently matches nothing.
func TestLegacyCitiesBodyIgnoresLocale(t *testing.T) {
	f := &fakeUC{items: seed()}
	r := router(f, domain.RoleUser)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cities", nil)
	req.Header.Set("Accept-Language", "en")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	const want = `{"data":["Астана","Алматы"]}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("GET /cities with Accept-Language: en = %s, want %s", got, want)
	}
}

// TestFullFormatServesTheDictionary: the same route, opted into by a new
// client, carries what the dictionary is for — stable ids and codes,
// translations, and the exact string to send back as a filter.
func TestFullFormatServesTheDictionary(t *testing.T) {
	items := seed()
	f := &fakeUC{items: items}
	r := router(f, domain.RoleUser)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cities?format=full", nil)
	req.Header.Set("Accept-Language", "en")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /cities?format=full = %d, want 200", w.Code)
	}

	var body struct {
		Data []cityResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, w.Body.String())
	}
	// Hidden entries are absent from the public read.
	if len(body.Data) != 2 {
		t.Fatalf("got %d cities, want 2 active ones (hidden must not be public)", len(body.Data))
	}
	if body.Data[0].Code != "astana" || body.Data[1].Code != "almaty" {
		t.Errorf("order = %q,%q, want astana,almaty — the usecase order must survive", body.Data[0].Code, body.Data[1].Code)
	}
	if body.Data[0].Name != "Astana" {
		t.Errorf("localized name = %q, want %q for Accept-Language: en", body.Data[0].Name, "Astana")
	}
	if body.Data[0].Value != "Астана" {
		t.Errorf("value = %q, want the base name %q: this is what goes back as ?city=", body.Data[0].Value, "Астана")
	}
	if body.Data[0].ID != items[0].ID.String() {
		t.Errorf("id = %q, want %q", body.Data[0].ID, items[0].ID)
	}
}

// TestVenueStaffCannotManageTheDictionary is the router half of the rule the
// usecase test also pins: the city list is the platform's. Two independent
// gates, because a route re-mounted on a wider group is exactly the mistake
// this feature cannot afford.
func TestVenueStaffCannotManageTheDictionary(t *testing.T) {
	id := uuid.New()
	calls := []struct {
		method, url string
		body        any
	}{
		{http.MethodGet, "/api/v1/admin/cities", nil},
		{http.MethodPost, "/api/v1/admin/cities", map[string]any{"code": "shymkent", "name": "Шымкент"}},
		{http.MethodPatch, "/api/v1/admin/cities/" + id.String(), map[string]any{"name": "Другой"}},
		{http.MethodDelete, "/api/v1/admin/cities/" + id.String(), nil},
		{http.MethodPut, "/api/v1/admin/cities/order", map[string]any{"city_ids": []string{id.String()}}},
		{http.MethodPost, "/api/v1/admin/cities/" + id.String() + "/aliases", map[string]any{"alias": "нур-султан"}},
	}

	for _, role := range []domain.Role{domain.RoleRestaurant, domain.RoleUser} {
		f := &fakeUC{items: seed()}
		r := router(f, role)
		for _, c := range calls {
			w := send(t, r, c.method, c.url, c.body, true)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s as %q = %d, want 403 (body %s)", c.method, c.url, role, w.Code, w.Body.String())
			}
		}
		if f.created != 0 || len(f.hidden) != 0 {
			t.Errorf("as %q the usecase was reached (created=%d hidden=%d); the gate must stop it at the router",
				role, f.created, len(f.hidden))
		}
	}

	// Anonymous: 401, not 403 — a missing token is a different answer from a
	// token that is simply not allowed here.
	f := &fakeUC{items: seed()}
	r := router(f, domain.RoleAdmin)
	if w := send(t, r, http.MethodPost, "/api/v1/admin/cities",
		map[string]any{"code": "shymkent", "name": "Шымкент"}, false); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous POST = %d, want 401", w.Code)
	}

	// And the superadmin does get through, so the test cannot pass by the
	// routes simply not existing.
	if w := send(t, r, http.MethodPost, "/api/v1/admin/cities",
		map[string]any{"code": "shymkent", "name": "Шымкент"}, true); w.Code != http.StatusCreated {
		t.Errorf("superadmin POST = %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	if f.gotActor.Role != domain.RoleAdmin {
		t.Errorf("actor role reaching the usecase = %q, want admin", f.gotActor.Role)
	}
}

// TestDeleteHidesRatherThanDeletes: venues reference a city by id and carry its
// name as a live string, so the destructive-looking verb must be a soft hide —
// and the response has to say so, or the panel cannot show what happened.
func TestDeleteHidesRatherThanDeletes(t *testing.T) {
	id := uuid.New()
	f := &fakeUC{items: seed()}
	r := router(f, domain.RoleAdmin)

	w := send(t, r, http.MethodDelete, "/api/v1/admin/cities/"+id.String(), nil, true)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if f.deleted != 0 {
		t.Fatal("the handler reached a hard delete; hiding is the only removal this dictionary has")
	}
	if len(f.hidden) != 1 || f.hidden[0] != id {
		t.Fatalf("hidden ids = %v, want exactly [%s]", f.hidden, id)
	}
	var body struct {
		Data cityResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Data.IsActive {
		t.Error("response says is_active=true after DELETE; the caller cannot see the city was hidden")
	}
}

// TestAdminListShowsHiddenCities: whoever hid a city has to be able to find it
// again, or hiding is indistinguishable from deleting.
func TestAdminListShowsHiddenCities(t *testing.T) {
	f := &fakeUC{items: seed()}
	r := router(f, domain.RoleAdmin)

	w := send(t, r, http.MethodGet, "/api/v1/admin/cities", nil, true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/cities = %d, want 200", w.Code)
	}
	var body struct {
		Data []cityResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 3 {
		t.Fatalf("admin sees %d cities, want all 3 including the hidden one", len(body.Data))
	}
}

// TestReorderPassesTheWholeOrder: the order arrives as a full sequence, and a
// single unparseable id fails the call instead of reordering a list the caller
// never sent.
func TestReorderPassesTheWholeOrder(t *testing.T) {
	items := seed()
	f := &fakeUC{items: items}
	r := router(f, domain.RoleAdmin)

	w := send(t, r, http.MethodPut, "/api/v1/admin/cities/order",
		map[string]any{"city_ids": []string{items[1].ID.String(), items[0].ID.String()}}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT order = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if len(f.gotIDs) != 2 || f.gotIDs[0] != items[1].ID || f.gotIDs[1] != items[0].ID {
		t.Fatalf("ids reaching the usecase = %v, want %v", f.gotIDs, []uuid.UUID{items[1].ID, items[0].ID})
	}

	f2 := &fakeUC{items: items}
	r2 := router(f2, domain.RoleAdmin)
	w = send(t, r2, http.MethodPut, "/api/v1/admin/cities/order",
		map[string]any{"city_ids": []string{items[0].ID.String(), "not-a-uuid"}}, true)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PUT order with a bad id = %d, want 422", w.Code)
	}
	if f2.gotIDs != nil {
		t.Error("a partially parsed order reached the usecase")
	}
}
