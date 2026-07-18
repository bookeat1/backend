package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeChecker struct {
	manages bool
	err     error
}

func (f fakeChecker) Manages(_ context.Context, _ uuid.UUID, _ uuid.UUID) (bool, error) {
	return f.manages, f.err
}

func TestRequireRestaurantManager(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rid := uuid.New()
	cases := []struct {
		name       string
		set        bool
		role       domain.Role
		checker    fakeChecker
		param      string
		wantStatus int
	}{
		{"admin passes", true, domain.RoleAdmin, fakeChecker{}, rid.String(), http.StatusOK},
		{"manager of restaurant passes", true, domain.RoleRestaurant, fakeChecker{manages: true}, rid.String(), http.StatusOK},
		{"manager of other forbidden", true, domain.RoleRestaurant, fakeChecker{manages: false}, rid.String(), http.StatusForbidden},
		{"plain user forbidden", true, domain.RoleUser, fakeChecker{}, rid.String(), http.StatusForbidden},
		{"no auth unauthorized", false, "", fakeChecker{}, rid.String(), http.StatusUnauthorized},
		{"bad restaurant id", true, domain.RoleRestaurant, fakeChecker{manages: true}, "not-a-uuid", http.StatusUnprocessableEntity},
		{"checker error", true, domain.RoleRestaurant, fakeChecker{err: errors.New("db down")}, rid.String(), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.set {
				ctx := context.WithValue(req.Context(), authUserKey{}, AuthUser{ID: uuid.New(), Role: string(tc.role)})
				req = req.WithContext(ctx)
			}
			c.Request = req
			c.Params = gin.Params{{Key: "restaurantId", Value: tc.param}}
			RequireRestaurantManager(tc.checker, "restaurantId")(c)
			if !c.IsAborted() {
				c.Status(http.StatusOK)
			}
			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
		})
	}
}
