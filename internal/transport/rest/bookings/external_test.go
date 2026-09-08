package bookings

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// managerDeps returns deps for a restaurant-role user who manages the venue, so
// the RequireRestaurantManager gate in front of the external-reservation routes
// lets the handler run.
func managerDeps() *deps {
	d := newDeps()
	d.role = domain.RoleRestaurant
	d.manages = true
	return d
}

func extPath(rid uuid.UUID) string {
	return "/api/v1/restaurants/" + rid.String() + "/external-reservations"
}

// validHoldBody is a whole-venue block (no table_id) over a valid RFC3339 window.
func validHoldBody() gin.H {
	return gin.H{
		"starts_at": "2026-08-01T18:00:00Z",
		"ends_at":   "2026-08-01T20:00:00Z",
	}
}

func TestCreateExternalHoldHappy(t *testing.T) {
	d := managerDeps()
	r := newRouter(d)
	rid := uuid.New()
	w := do(r, http.MethodPost, extPath(rid), validHoldBody(), authHeader(uuid.New()))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body)
	}
}

// TestCreateExternalHoldBadPathID: a malformed restaurant id in the path is
// rejected with 422 (here by the RequireRestaurantManager middleware, which
// parses the id before the handler runs — documents the actual status).
func TestCreateExternalHoldBadPathID(t *testing.T) {
	d := managerDeps()
	r := newRouter(d)
	w := do(r, http.MethodPost, "/api/v1/restaurants/not-a-uuid/external-reservations", validHoldBody(), authHeader(uuid.New()))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
	}
}

// TestCreateExternalHoldMalformedBody: a syntactically broken JSON body fails
// ShouldBindJSON and maps to 422.
func TestCreateExternalHoldMalformedBody(t *testing.T) {
	d := managerDeps()
	r := newRouter(d)
	rid := uuid.New()
	req := httptest.NewRequest(http.MethodPost, extPath(rid), bytes.NewReader([]byte(`{"starts_at":`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+uuid.New().String())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
	}
}

// TestCreateExternalHoldBadTimestampType: a non-RFC3339 starts_at (wrong JSON
// type) fails time.Time binding → 422.
func TestCreateExternalHoldBadTimestampType(t *testing.T) {
	d := managerDeps()
	r := newRouter(d)
	rid := uuid.New()
	body := gin.H{"starts_at": "yesterday", "ends_at": "2026-08-01T20:00:00Z"}
	w := do(r, http.MethodPost, extPath(rid), body, authHeader(uuid.New()))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
	}
}

// TestCreateExternalHoldBadTableID: a body-level table_id that is not a UUID is
// caught in toInput() as ErrValidation and mapped to 422.
func TestCreateExternalHoldBadTableID(t *testing.T) {
	d := managerDeps()
	r := newRouter(d)
	rid := uuid.New()
	body := validHoldBody()
	body["table_id"] = "not-a-uuid"
	w := do(r, http.MethodPost, extPath(rid), body, authHeader(uuid.New()))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
	}
}

// TestCreateExternalHoldErrorMapping: usecase sentinel errors → HTTP status.
func TestCreateExternalHoldErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"forbidden", fmt.Errorf("%w: booking.manage", domain.ErrForbidden), http.StatusForbidden},
		{"not found", fmt.Errorf("%w: restaurant", domain.ErrNotFound), http.StatusNotFound},
		{"validation", fmt.Errorf("%w: window", domain.ErrValidation), http.StatusUnprocessableEntity},
		{"conflict", fmt.Errorf("%w: overlap", domain.ErrAlreadyExists), http.StatusConflict},
		{"internal", fmt.Errorf("db down"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := managerDeps()
			d.external.err = tc.err
			r := newRouter(d)
			w := do(r, http.MethodPost, extPath(uuid.New()), validHoldBody(), authHeader(uuid.New()))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
	}
}

func TestListExternalHoldsHappy(t *testing.T) {
	d := managerDeps()
	r := newRouter(d)
	rid := uuid.New()
	path := extPath(rid) + "?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z"
	w := do(r, http.MethodGet, path, nil, authHeader(uuid.New()))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
}

// TestListExternalHoldsBadWindow: both from and to are required RFC3339
// timestamps; a missing or unparseable value maps to 422.
func TestListExternalHoldsBadWindow(t *testing.T) {
	d := managerDeps()
	r := newRouter(d)
	rid := uuid.New()
	cases := []struct {
		name, query string
	}{
		{"missing from", "?to=2026-08-02T00:00:00Z"},
		{"missing to", "?from=2026-08-01T00:00:00Z"},
		{"both missing", ""},
		{"bad from", "?from=nope&to=2026-08-02T00:00:00Z"},
		{"bad to", "?from=2026-08-01T00:00:00Z&to=nope"},
		{"date-only from (not RFC3339)", "?from=2026-08-01&to=2026-08-02T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(r, http.MethodGet, extPath(rid)+tc.query, nil, authHeader(uuid.New()))
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
			}
		})
	}
}

func TestListExternalHoldsErrorMapping(t *testing.T) {
	d := managerDeps()
	d.external.err = fmt.Errorf("%w: booking.manage", domain.ErrForbidden)
	r := newRouter(d)
	path := extPath(uuid.New()) + "?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z"
	w := do(r, http.MethodGet, path, nil, authHeader(uuid.New()))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body)
	}
}

func TestRemoveExternalHoldHappy(t *testing.T) {
	d := managerDeps()
	r := newRouter(d)
	path := extPath(uuid.New()) + "/" + uuid.New().String()
	w := do(r, http.MethodDelete, path, nil, authHeader(uuid.New()))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
}

// TestRemoveExternalHoldBadHoldID: a malformed hold id in the path → 422 (parsed
// by the handler's pathID, after the middleware has already accepted the valid
// restaurant id).
func TestRemoveExternalHoldBadHoldID(t *testing.T) {
	d := managerDeps()
	r := newRouter(d)
	path := extPath(uuid.New()) + "/not-a-uuid"
	w := do(r, http.MethodDelete, path, nil, authHeader(uuid.New()))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
	}
}

func TestRemoveExternalHoldErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", fmt.Errorf("%w: hold", domain.ErrNotFound), http.StatusNotFound},
		{"forbidden", fmt.Errorf("%w: booking.manage", domain.ErrForbidden), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := managerDeps()
			d.external.err = tc.err
			r := newRouter(d)
			path := extPath(uuid.New()) + "/" + uuid.New().String()
			w := do(r, http.MethodDelete, path, nil, authHeader(uuid.New()))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
	}
}
