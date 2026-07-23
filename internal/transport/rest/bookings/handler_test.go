package bookings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	uc "backend-core/internal/usecase/bookings"
)

// deps bundles what a test router is built from, so each test overrides only
// what it cares about.
type deps struct {
	facade     *fakeFacade
	create     *fakeCreate
	idempotent uc.IdempotentCreateUseCase
	status     *fakeStatus
	update     *fakeUpdate
	avail      *fakeAvail
	blacklist  *fakeBlacklist
	policy     *fakePolicy
	role       domain.Role
	manages    bool
}

func newDeps() *deps {
	create := &fakeCreate{}
	return &deps{
		facade: &fakeFacade{}, create: create,
		idempotent: uc.NewIdempotentCreateUseCase(create, newFakeKeys(), fakeTx{}),
		status:     &fakeStatus{}, update: &fakeUpdate{}, avail: &fakeAvail{},
		blacklist: &fakeBlacklist{}, policy: &fakePolicy{}, role: domain.RoleUser, manages: false,
	}
}

// newRouter mirrors the mounting done in bootstrap.NewApp: public group,
// authenticated group, and a restaurant-scoped group behind the real
// RequireRestaurantManager middleware.
func newRouter(d *deps) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(d.facade, d.create, d.idempotent, d.status, d.update, d.avail, d.blacklist, d.policy)

	api := r.Group("/api/v1")
	h.RegisterPublic(api)

	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: d.role}))
	h.RegisterRoutes(authed)

	scoped := authed.Group("")
	scoped.Use(middleware.RequireRestaurantManager(fakeManagers{manages: d.manages}, "id"))
	h.RegisterRestaurantScoped(scoped)
	return r
}

func do(r *gin.Engine, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// authHeader builds the Bearer header the fake issuer understands.
func authHeader(userID uuid.UUID) map[string]string {
	return map[string]string{"Authorization": "Bearer " + userID.String()}
}

// TestManagerOfAnotherRestaurantForbidden covers the spec §7 requirement: every
// manager endpoint must answer 403 to a manager of a different venue. Venue
// -scoped routes are stopped by the middleware; booking-scoped ones are stopped
// by the usecase, which is why both paths are asserted here.
func TestManagerOfAnotherRestaurantForbidden(t *testing.T) {
	rid, bid, uid := uuid.New(), uuid.New(), uuid.New()

	t.Run("restaurant-scoped routes", func(t *testing.T) {
		d := newDeps()
		d.role = domain.RoleRestaurant
		d.manages = false // manages some OTHER restaurant
		r := newRouter(d)

		cases := []struct{ method, path string }{
			{http.MethodGet, "/api/v1/restaurants/" + rid.String() + "/bookings"},
			{http.MethodPost, "/api/v1/restaurants/" + rid.String() + "/bookings"},
			{http.MethodGet, "/api/v1/restaurants/" + rid.String() + "/blacklist"},
			{http.MethodPost, "/api/v1/restaurants/" + rid.String() + "/blacklist"},
			{http.MethodDelete, "/api/v1/restaurants/" + rid.String() + "/blacklist/" + uuid.New().String()},
			{http.MethodGet, "/api/v1/restaurants/" + rid.String() + "/booking-policy"},
			{http.MethodPatch, "/api/v1/restaurants/" + rid.String() + "/booking-policy"},
		}
		for _, tc := range cases {
			w := do(r, tc.method, tc.path, gin.H{}, authHeader(uid))
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s: status = %d, want 403 (body %s)", tc.method, tc.path, w.Code, w.Body)
			}
		}
	})

	t.Run("booking-scoped routes", func(t *testing.T) {
		d := newDeps()
		d.role = domain.RoleRestaurant
		d.manages = false
		// The usecases reject the wrong venue's staff; the transport layer only
		// has to map that to 403 and stop.
		forbidden := fmt.Errorf("%w: booking belongs to another restaurant", domain.ErrForbidden)
		d.status.err = forbidden
		d.update.err = forbidden
		d.facade.err = forbidden
		r := newRouter(d)

		base := "/api/v1/bookings/" + bid.String()
		cases := []struct{ method, path string }{
			{http.MethodPatch, base},
			{http.MethodPost, base + "/confirm"},
			{http.MethodPost, base + "/reject"},
			{http.MethodPost, base + "/arrive"},
			{http.MethodPost, base + "/complete"},
			{http.MethodPost, base + "/no-show"},
			{http.MethodGet, base},
			{http.MethodGet, base + "/messages"},
		}
		for _, tc := range cases {
			w := do(r, tc.method, tc.path, gin.H{}, authHeader(uid))
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s: status = %d, want 403 (body %s)", tc.method, tc.path, w.Code, w.Body)
			}
		}
	})
}

// TestGuestForeignBookingNotFound: a guest asking for someone else's booking
// must get 404, never 403 — 403 would confirm the id exists (spec §7 / access.go).
func TestGuestForeignBookingNotFound(t *testing.T) {
	d := newDeps()
	d.facade.err = fmt.Errorf("%w: booking", domain.ErrNotFound)
	r := newRouter(d)

	base := "/api/v1/bookings/" + uuid.New().String()
	for _, path := range []string{base, base + "/messages", base + "/history"} {
		w := do(r, http.MethodGet, path, nil, authHeader(uuid.New()))
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404 (body %s)", path, w.Code, w.Body)
		}
	}
}

func TestUnauthenticatedRejected(t *testing.T) {
	r := newRouter(newDeps())
	w := do(r, http.MethodGet, "/api/v1/bookings", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAvailabilityIsPublic(t *testing.T) {
	r := newRouter(newDeps())
	w := do(r, http.MethodGet, "/api/v1/restaurants/"+uuid.New().String()+"/availability?date=2026-08-01&guests=4", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
}

func createBody(rid uuid.UUID, guests int) gin.H {
	return gin.H{
		"restaurant_id": rid.String(),
		"name":          "Damir",
		"phone":         "+77771234567",
		"guests":        guests,
		"starts_at":     "2026-08-01T18:00:00Z",
	}
}

// TestCreateIdempotency is the spec §7 contract: the same key with the same
// body replays the first result and creates exactly one booking; the same key
// with a different body is a conflict.
func TestCreateIdempotency(t *testing.T) {
	d := newDeps()
	r := newRouter(d)
	uid := uuid.New()
	rid := uuid.New()
	headers := authHeader(uid)
	headers[idempotencyHeader] = "key-1"

	first := do(r, http.MethodPost, "/api/v1/bookings", createBody(rid, 2), headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("first POST: status = %d, want 201 (body %s)", first.Code, first.Body)
	}
	second := do(r, http.MethodPost, "/api/v1/bookings", createBody(rid, 2), headers)
	if second.Code != http.StatusCreated {
		t.Fatalf("replay: status = %d, want 201 (body %s)", second.Code, second.Body)
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("replay returned a different payload:\n first  = %s\n second = %s", first.Body, second.Body)
	}
	if d.create.calls != 1 {
		t.Errorf("create called %d times, want 1 — a retry must not book twice", d.create.calls)
	}

	// Same key, different body → 409, and still no second booking.
	conflict := do(r, http.MethodPost, "/api/v1/bookings", createBody(rid, 5), headers)
	if conflict.Code != http.StatusConflict {
		t.Errorf("key reuse with another body: status = %d, want 409 (body %s)", conflict.Code, conflict.Body)
	}
	if d.create.calls != 1 {
		t.Errorf("create called %d times after the conflicting retry, want 1", d.create.calls)
	}
}

// TestCreateRequiresIdempotencyKey: the header is mandatory (spec §7).
func TestCreateRequiresIdempotencyKey(t *testing.T) {
	d := newDeps()
	r := newRouter(d)
	w := do(r, http.MethodPost, "/api/v1/bookings", createBody(uuid.New(), 2), authHeader(uuid.New()))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
	}
	if d.create.calls != 0 {
		t.Errorf("create called %d times without a key, want 0", d.create.calls)
	}
}

// TestGuestCannotForcePlacement: table pinning and force are staff powers; the
// guest route must drop them before the usecase ever sees them.
func TestGuestCannotForcePlacement(t *testing.T) {
	d := newDeps()
	captured := &capturingCreate{inner: d.create}
	d.create = &fakeCreate{}
	d.idempotent = uc.NewIdempotentCreateUseCase(captured, newFakeKeys(), fakeTx{})
	r := newRouter(d)

	body := createBody(uuid.New(), 2)
	body["force"] = true
	body["table_ids"] = []string{uuid.New().String()}
	body["source"] = "admin"
	body["user_id"] = uuid.New().String()

	uid := uuid.New()
	headers := authHeader(uid)
	headers[idempotencyHeader] = "key-2"
	if w := do(r, http.MethodPost, "/api/v1/bookings", body, headers); w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body)
	}
	got := captured.last
	if got.Force || len(got.TableIDs) != 0 {
		t.Errorf("guest placement fields leaked into the usecase: force=%v tables=%v", got.Force, got.TableIDs)
	}
	if got.Source != domain.SourceApp {
		t.Errorf("source = %q, want %q", got.Source, domain.SourceApp)
	}
	if got.UserID == nil || *got.UserID != uid {
		t.Errorf("user_id = %v, want the authenticated user %s", got.UserID, uid)
	}
}

type capturingCreate struct {
	inner *fakeCreate
	last  uc.CreateInput
}

func (c *capturingCreate) Create(ctx context.Context, actor uc.Actor, in uc.CreateInput) (*uc.BookingDetails, error) {
	c.last = in
	return c.inner.Create(ctx, actor, in)
}
