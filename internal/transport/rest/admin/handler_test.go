package admin

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
	adminuc "backend-core/internal/usecase/admin"
	"backend-core/internal/usecase/bookings"
	"backend-core/internal/usecase/menu"
	"backend-core/internal/usecase/restaurants"
)

// ---- auth plumbing (mirrors the bookings/reviews handler tests) ------------

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

type fakeManagers struct{ manages bool }

func (f fakeManagers) Manages(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return f.manages, nil
}

// ---- usecase-port fakes ----------------------------------------------------
// Each fake carries a settable err so a handler test can drive the domain
// error → HTTP status mapping; canned success values drive the happy paths.

type fakePerms struct{ allow bool }

func (f fakePerms) HasPermission(context.Context, uuid.UUID, uuid.UUID, domain.Permission) (bool, error) {
	return f.allow, nil
}

type fakeRest struct{ err error }

func (f *fakeRest) Get(_ context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id, Name: "Тест"}}, nil
}
func (f *fakeRest) Update(_ context.Context, id uuid.UUID, _ restaurants.SaveInput) (*domain.RestaurantAggregate, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id, Name: "Тест"}}, nil
}

type fakeMenu struct{ err error }

func (f *fakeMenu) ListByRestaurant(_ context.Context, _ uuid.UUID, _ *string) ([]domain.MenuItem, error) {
	return nil, f.err
}
func (f *fakeMenu) Categories(_ context.Context) ([]domain.MenuCategory, error) { return nil, f.err }
func (f *fakeMenu) Create(_ context.Context, rid uuid.UUID, _ menu.ItemInput) (*domain.MenuItem, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.MenuItem{ID: uuid.New(), RestaurantID: rid}, nil
}
func (f *fakeMenu) Update(_ context.Context, rid, itemID uuid.UUID, _ menu.ItemInput) (*domain.MenuItem, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.MenuItem{ID: itemID, RestaurantID: rid}, nil
}
func (f *fakeMenu) Delete(_ context.Context, _, _ uuid.UUID) error { return f.err }
func (f *fakeMenu) SetAvailable(_ context.Context, _, _ uuid.UUID, _ bool) error {
	return f.err
}
func (f *fakeMenu) SetAvailableBulk(_ context.Context, _ uuid.UUID, ids []uuid.UUID, _ bool) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return len(ids), nil
}

type fakeWH struct{ err error }

func (f *fakeWH) ListWorkingHours(_ context.Context, _ uuid.UUID) ([]domain.WorkingHours, error) {
	return nil, f.err
}
func (f *fakeWH) ReplaceWorkingHours(_ context.Context, _ uuid.UUID, _ []domain.WorkingHours) error {
	return f.err
}

type fakeOverrides struct{ err error }

func (f *fakeOverrides) ListByRestaurant(_ context.Context, _ uuid.UUID) ([]domain.ScheduleOverride, error) {
	return nil, f.err
}
func (f *fakeOverrides) GetForBookingInstant(_ context.Context, _ uuid.UUID, _ time.Time, _ string) (*domain.ScheduleOverride, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeOverrides) Upsert(_ context.Context, _ *domain.ScheduleOverride) error { return f.err }
func (f *fakeOverrides) Delete(_ context.Context, _ uuid.UUID, _ time.Time) error   { return f.err }

type fakeGuests struct{ err error }

func (f *fakeGuests) ListByRestaurant(_ context.Context, _ uuid.UUID) ([]domain.RestaurantGuest, error) {
	return nil, f.err
}

type fakeBookingList struct{ err error }

func (f *fakeBookingList) ListByRestaurant(_ context.Context, _ bookings.Actor, _ uuid.UUID, _ domain.BookingFilter) ([]domain.Booking, int, error) {
	return nil, 0, f.err
}

type fakeBookingTx struct{ err error }

func (f *fakeBookingTx) result(id uuid.UUID) (*domain.Booking, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.Booking{ID: id}, nil
}
func (f *fakeBookingTx) Confirm(_ context.Context, _ bookings.Actor, id uuid.UUID, _ *string) (*domain.Booking, error) {
	return f.result(id)
}
func (f *fakeBookingTx) Reject(_ context.Context, _ bookings.Actor, id uuid.UUID, _ *string) (*domain.Booking, error) {
	return f.result(id)
}
func (f *fakeBookingTx) Cancel(_ context.Context, _ bookings.Actor, id uuid.UUID, _ bookings.CancelInput) (*domain.Booking, error) {
	return f.result(id)
}
func (f *fakeBookingTx) NoShow(_ context.Context, _ bookings.Actor, id uuid.UUID, _ *string) (*domain.Booking, error) {
	return f.result(id)
}

// harness bundles the fakes with a built router.
type harness struct {
	rest      *fakeRest
	menu      *fakeMenu
	wh        *fakeWH
	overrides *fakeOverrides
	guests    *fakeGuests
	bookList  *fakeBookingList
	bookTx    *fakeBookingTx
	router    *gin.Engine
}

// newHarness builds the admin handler behind the same middleware stack as
// bootstrap: Auth + RequireRestaurantManager. allow drives the usecase RBAC
// gate (perms.HasPermission); manages drives the middleware gate.
func newHarness(allow, manages bool) *harness {
	gin.SetMode(gin.TestMode)
	h := &harness{
		rest: &fakeRest{}, menu: &fakeMenu{}, wh: &fakeWH{},
		overrides: &fakeOverrides{}, guests: &fakeGuests{},
		bookList: &fakeBookingList{}, bookTx: &fakeBookingTx{},
	}
	uc := adminuc.NewUseCase(fakePerms{allow: allow}, h.rest, h.menu, h.wh, h.overrides, h.guests, h.bookList, h.bookTx, fakePaySettings{}, fakeTelegramSettings{})

	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: domain.RoleRestaurant}))
	scoped := authed.Group("")
	scoped.Use(middleware.RequireRestaurantManager(fakeManagers{manages: manages}, "id"))
	NewHandler(uc).RegisterRoutes(scoped)
	h.router = r
	return h
}

func do(r *gin.Engine, method, path string, body any, rawBody []byte, uid uuid.UUID) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	switch {
	case rawBody != nil:
		reader = bytes.NewReader(rawBody)
	case body != nil:
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	default:
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+uid.String())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func base(rid uuid.UUID) string { return "/api/v1/admin/restaurants/" + rid.String() }

// TestHappyPaths walks every admin endpoint on its success path and asserts the
// exact 2xx the handler writes (200 for reads/updates/toggles, 201 for a menu
// item create).
func TestHappyPaths(t *testing.T) {
	h := newHarness(true, true)
	rid, iid, bid := uuid.New(), uuid.New(), uuid.New()
	b := base(rid)

	cases := []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"get profile", http.MethodGet, b + "/profile", nil, http.StatusOK},
		{"update profile", http.MethodPut, b + "/profile", gin.H{"name": "Новое"}, http.StatusOK},
		{"list menu", http.MethodGet, b + "/menu", nil, http.StatusOK},
		{"list categories", http.MethodGet, b + "/menu-categories", nil, http.StatusOK},
		{"create menu item", http.MethodPost, b + "/menu-items", gin.H{"name": "Плов", "price": "3500"}, http.StatusCreated},
		{"update menu item", http.MethodPatch, b + "/menu-items/" + iid.String(), gin.H{"name": "Плов"}, http.StatusOK},
		{"delete menu item", http.MethodDelete, b + "/menu-items/" + iid.String(), nil, http.StatusOK},
		{"toggle availability", http.MethodPatch, b + "/menu-items/" + iid.String() + "/availability", gin.H{"is_available": false}, http.StatusOK},
		{"stop list", http.MethodPost, b + "/stop-list", gin.H{"item_ids": []string{iid.String()}, "available": false}, http.StatusOK},
		{"get schedule", http.MethodGet, b + "/schedule", nil, http.StatusOK},
		{"set working hours", http.MethodPut, b + "/working-hours", gin.H{"working_hours": []gin.H{{"day_of_week": 1, "is_open": false}}}, http.StatusOK},
		{"set schedule override", http.MethodPut, b + "/schedule/overrides", gin.H{"date": "2026-08-01", "is_closed": true}, http.StatusOK},
		{"delete schedule override", http.MethodDelete, b + "/schedule/overrides/2026-08-01", nil, http.StatusOK},
		{"list bookings", http.MethodGet, b + "/bookings", nil, http.StatusOK},
		{"confirm booking", http.MethodPost, b + "/bookings/" + bid.String() + "/confirm", gin.H{}, http.StatusOK},
		{"reject booking", http.MethodPost, b + "/bookings/" + bid.String() + "/reject", gin.H{}, http.StatusOK},
		{"cancel booking", http.MethodPost, b + "/bookings/" + bid.String() + "/cancel", gin.H{}, http.StatusOK},
		{"no-show booking", http.MethodPost, b + "/bookings/" + bid.String() + "/no-show", gin.H{}, http.StatusOK},
		{"list guests", http.MethodGet, b + "/guests", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(h.router, tc.method, tc.path, tc.body, nil, uuid.New())
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
	}
}

// TestBadPathUUID: a malformed :id / :itemId / :bookingId is rejected with 422.
// For :id the RequireRestaurantManager middleware rejects it (restaurant-role
// caller); for :itemId / :bookingId the handler's pathUUID does.
func TestBadPathUUID(t *testing.T) {
	h := newHarness(true, true)
	goodRID := uuid.New()
	gb := base(goodRID)

	cases := []struct{ name, method, path string }{
		{"bad restaurant id (profile)", http.MethodGet, "/api/v1/admin/restaurants/not-a-uuid/profile"},
		{"bad restaurant id (menu-items)", http.MethodPost, "/api/v1/admin/restaurants/not-a-uuid/menu-items"},
		{"bad item id (update)", http.MethodPatch, gb + "/menu-items/not-a-uuid"},
		{"bad item id (delete)", http.MethodDelete, gb + "/menu-items/not-a-uuid"},
		{"bad item id (availability)", http.MethodPatch, gb + "/menu-items/not-a-uuid/availability"},
		{"bad booking id (confirm)", http.MethodPost, gb + "/bookings/not-a-uuid/confirm"},
		{"bad booking id (cancel)", http.MethodPost, gb + "/bookings/not-a-uuid/cancel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(h.router, tc.method, tc.path, gin.H{}, nil, uuid.New())
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
			}
		})
	}
}

// TestMalformedBody: a syntactically broken JSON body on the binding endpoints
// maps to 422.
func TestMalformedBody(t *testing.T) {
	h := newHarness(true, true)
	rid, iid := uuid.New(), uuid.New()
	b := base(rid)

	cases := []struct{ name, method, path string }{
		{"update profile", http.MethodPut, b + "/profile"},
		{"create menu item", http.MethodPost, b + "/menu-items"},
		{"update menu item", http.MethodPatch, b + "/menu-items/" + iid.String()},
		{"availability", http.MethodPatch, b + "/menu-items/" + iid.String() + "/availability"},
		{"stop list", http.MethodPost, b + "/stop-list"},
		{"working hours", http.MethodPut, b + "/working-hours"},
		{"schedule override", http.MethodPut, b + "/schedule/overrides"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(h.router, tc.method, tc.path, nil, []byte(`{bad json`), uuid.New())
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
			}
		})
	}
}

// TestBadValueParsing covers value-level parse failures that the request layer
// turns into ErrValidation → 422: an unparseable date on the override PUT and
// on the override DELETE path param, and a non-UUID id inside the stop-list.
func TestBadValueParsing(t *testing.T) {
	h := newHarness(true, true)
	rid := uuid.New()
	b := base(rid)

	t.Run("stop-list bad item id", func(t *testing.T) {
		w := do(h.router, http.MethodPost, b+"/stop-list", gin.H{"item_ids": []string{"not-a-uuid"}}, nil, uuid.New())
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
		}
	})
	t.Run("override PUT bad date", func(t *testing.T) {
		w := do(h.router, http.MethodPut, b+"/schedule/overrides", gin.H{"date": "31-08-2026", "is_closed": true}, nil, uuid.New())
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
		}
	})
	t.Run("override DELETE bad date", func(t *testing.T) {
		w := do(h.router, http.MethodDelete, b+"/schedule/overrides/31-08-2026", nil, nil, uuid.New())
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
		}
	})
	t.Run("override PUT open day without times → usecase ErrValidation", func(t *testing.T) {
		w := do(h.router, http.MethodPut, b+"/schedule/overrides", gin.H{"date": "2026-08-01", "is_closed": false}, nil, uuid.New())
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
		}
	})
}

// TestBookingFilterParsing documents the venue calendar's query parsing: a bad
// ?from / ?date is 422 (ErrValidation), but a non-integer ?page / ?per_page is
// silently defaulted (strconv.Atoi error discarded) and still returns 200.
func TestBookingFilterParsing(t *testing.T) {
	h := newHarness(true, true)
	b := base(uuid.New())

	t.Run("bad from", func(t *testing.T) {
		w := do(h.router, http.MethodGet, b+"/bookings?from=nope", nil, nil, uuid.New())
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
		}
	})
	t.Run("bad date", func(t *testing.T) {
		w := do(h.router, http.MethodGet, b+"/bookings?date=2026-13-40", nil, nil, uuid.New())
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
		}
	})
	t.Run("non-integer page is defaulted, still 200", func(t *testing.T) {
		w := do(h.router, http.MethodGet, b+"/bookings?page=abc&per_page=xyz", nil, nil, uuid.New())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
		}
	})
}

// TestErrorMapping asserts the domain error → HTTP status mapping the response
// package implements, exercised through representative admin endpoints (each
// error is injected into the store that endpoint delegates to).
func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", fmt.Errorf("%w: x", domain.ErrNotFound), http.StatusNotFound},
		{"conflict", fmt.Errorf("%w: x", domain.ErrAlreadyExists), http.StatusConflict},
		{"validation", fmt.Errorf("%w: x", domain.ErrValidation), http.StatusUnprocessableEntity},
		{"invalid status", fmt.Errorf("%w: x", domain.ErrInvalidStatus), http.StatusUnprocessableEntity},
		{"internal", fmt.Errorf("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run("getProfile/"+tc.name, func(t *testing.T) {
			h := newHarness(true, true)
			h.rest.err = tc.err
			w := do(h.router, http.MethodGet, base(uuid.New())+"/profile", nil, nil, uuid.New())
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
		t.Run("createMenuItem/"+tc.name, func(t *testing.T) {
			h := newHarness(true, true)
			h.menu.err = tc.err
			w := do(h.router, http.MethodPost, base(uuid.New())+"/menu-items", gin.H{"name": "x", "price": "1"}, nil, uuid.New())
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
		t.Run("confirmBooking/"+tc.name, func(t *testing.T) {
			h := newHarness(true, true)
			h.bookTx.err = tc.err
			w := do(h.router, http.MethodPost, base(uuid.New())+"/bookings/"+uuid.New().String()+"/confirm", gin.H{}, nil, uuid.New())
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
	}
}

// TestUsecaseForbidden: when the usecase RBAC gate denies (perms=false) but the
// coarse middleware admitted the caller (manages=true), the handler maps the
// usecase's ErrForbidden to 403.
func TestUsecaseForbidden(t *testing.T) {
	h := newHarness(false, true)
	w := do(h.router, http.MethodPut, base(uuid.New())+"/profile", gin.H{"name": "x"}, nil, uuid.New())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body)
	}
}

// TestMiddlewareForbidden: a restaurant-role caller who does not manage the
// venue is stopped at the middleware with 403, before any handler runs.
func TestMiddlewareForbidden(t *testing.T) {
	h := newHarness(true, false)
	w := do(h.router, http.MethodGet, base(uuid.New())+"/profile", nil, nil, uuid.New())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body)
	}
}

// TestUnauthenticated: no bearer token → 401 (Auth middleware).
func TestUnauthenticated(t *testing.T) {
	h := newHarness(true, true)
	req := httptest.NewRequest(http.MethodGet, base(uuid.New())+"/profile", nil)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", w.Code, w.Body)
	}
}

// fakePaySettings satisfies the admin usecase's paymentSettingsWriter port.
type fakePaySettings struct{}

func (fakePaySettings) UpdateFreeCancelWindow(_ context.Context, _ uuid.UUID, _ int) error {
	return nil
}

// fakeTelegramSettings satisfies the admin usecase's telegramSettings port.
type fakeTelegramSettings struct{}

func (fakeTelegramSettings) TelegramSettings(_ context.Context, _ uuid.UUID) (domain.TelegramSettings, error) {
	return domain.TelegramSettings{Enabled: true}, nil
}

func (fakeTelegramSettings) SetTelegramChatID(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}

func (fakeTelegramSettings) ClearTelegramChatID(_ context.Context, _ uuid.UUID) error {
	return nil
}
