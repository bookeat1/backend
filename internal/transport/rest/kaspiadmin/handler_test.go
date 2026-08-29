package kaspiadmin

import (
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
	kaspigw "backend-core/internal/infrastructure/payment/kaspi"
	"backend-core/internal/transport/rest/middleware"
)

// What this suite defends:
//
//  1. The list is superadmin-only. It names every merchant on the platform's
//     payment service and feeds the setting that decides whose till a guest's
//     money lands in — a venue manager reaching it is a leak AND a step towards
//     misrouting money.
//  2. A Kaspi service that is down is a 503, never an empty list. An empty
//     picker reads as "there are no companies", and the operator's next move is
//     to go and create one that already exists.
//  3. The readiness flag survives the trip. `has_active_session` is the whole
//     reason this endpoint exists rather than a text field.

// --- fakes ------------------------------------------------------------------

type fakeDirectory struct {
	companies []kaspigw.Company
	err       error
	calls     int
}

func (f *fakeDirectory) ListCompanies(context.Context) ([]kaspigw.Company, error) {
	f.calls++
	return f.companies, f.err
}

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

// router mounts the route exactly as bootstrap/app.go does: the real
// middleware.Auth plus the real RequireRole(RoleAdmin), so these tests cover
// the router gate and not only the handler's own re-check.
func router(dir CompanyDirectory, role domain.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: role}))
	adminGlobal := authed.Group("")
	adminGlobal.Use(middleware.RequireRole(domain.RoleAdmin))
	NewHandler(dir, nil).RegisterAdminGlobal(adminGlobal)
	return r
}

func get(t *testing.T, r *gin.Engine, authed bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/kaspi/companies", nil)
	if authed {
		req.Header.Set("Authorization", "Bearer "+uuid.NewString())
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type envelope struct {
	Data  []companyResponse `json:"data"`
	Error string            `json:"error"`
}

func decode(t *testing.T, w *httptest.ResponseRecorder) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return env
}

// --- tests ------------------------------------------------------------------

func TestListCompaniesReturnsTheRegistry(t *testing.T) {
	okAt := time.Date(2026, 8, 27, 14, 36, 52, 0, time.UTC)
	dir := &fakeDirectory{companies: []kaspigw.Company{
		{ID: "2", Name: "ИП САРКУЛИН ДАМИР", Status: "active", OrgName: "ИП САРКУЛИН ДАМИР",
			HasActiveSession: true, ActiveCashiers: 1, LastSessionOKAt: &okAt},
		{ID: "3", Name: "ТОО «Без сессии»", Status: "active"},
	}}

	w := get(t, router(dir, domain.RoleAdmin), true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	env := decode(t, w)
	if len(env.Data) != 2 {
		t.Fatalf("got %d companies, want 2", len(env.Data))
	}
	first := env.Data[0]
	if first.ID != "2" {
		t.Errorf("id = %q, want \"2\" — this is what goes into account_ref", first.ID)
	}
	if !first.HasActiveSession || first.ActiveCashiers != 1 {
		t.Errorf("readiness lost in transport: %+v", first)
	}
	if first.LastSessionOKAt == nil || !first.LastSessionOKAt.Equal(okAt) {
		t.Errorf("last_session_ok_at = %v, want %v", first.LastSessionOKAt, okAt)
	}
	// A company that cannot take money must still be listed, plainly marked:
	// hiding it would leave an operator wondering where it went.
	if env.Data[1].HasActiveSession {
		t.Errorf("second company reported a session it does not have")
	}
}

func TestListCompaniesEmptyRegistryIsAnEmptyArray(t *testing.T) {
	w := get(t, router(&fakeDirectory{}, domain.RoleAdmin), true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// `[]`, never `null`: the panel maps over this.
	if body := w.Body.String(); !strings.Contains(body, `"data":[]`) {
		t.Errorf("body = %s, want an empty array", body)
	}
}

func TestListCompaniesServiceUnavailable(t *testing.T) {
	dir := &fakeDirectory{err: fmt.Errorf("kaspi directory: unreachable: %w", domain.ErrUnavailable)}

	w := get(t, router(dir, domain.RoleAdmin), true)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", w.Code, w.Body.String())
	}
	env := decode(t, w)
	if len(env.Data) != 0 {
		t.Errorf("a failed read must not answer with companies, got %v", env.Data)
	}
	if env.Error == "" {
		t.Errorf("503 carried no message")
	}
}

func TestListCompaniesNotConfiguredIsUnavailableNotAPanic(t *testing.T) {
	w := get(t, router(nil, domain.RoleAdmin), true)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestListCompaniesForbiddenForNonSuperadmin(t *testing.T) {
	for _, role := range []domain.Role{domain.RoleUser, domain.RoleRestaurant} {
		t.Run(string(role), func(t *testing.T) {
			dir := &fakeDirectory{companies: []kaspigw.Company{{ID: "2", Name: "ИП"}}}
			w := get(t, router(dir, role), true)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body.String())
			}
			if dir.calls != 0 {
				t.Errorf("the Kaspi service was asked %d times for a caller who may not see the answer", dir.calls)
			}
		})
	}
}

// The handler re-checks the role on its own, so a future mount on a group
// without RequireRole cannot silently open the list.
func TestListCompaniesHandlerRefusesWithoutTheGroupGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: domain.RoleRestaurant}))
	dir := &fakeDirectory{companies: []kaspigw.Company{{ID: "2"}}}
	NewHandler(dir, nil).RegisterAdminGlobal(authed) // deliberately ungated

	w := get(t, r, true)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 from the handler's own check", w.Code)
	}
	if dir.calls != 0 {
		t.Errorf("directory was read %d times despite the refusal", dir.calls)
	}
}

func TestListCompaniesUnauthenticated(t *testing.T) {
	w := get(t, router(&fakeDirectory{}, domain.RoleAdmin), false)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
