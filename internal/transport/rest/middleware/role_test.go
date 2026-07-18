package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func TestRequireRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name       string
		set        bool
		role       domain.Role
		allow      []domain.Role
		wantStatus int
	}{
		{"allowed", true, domain.RoleAdmin, []domain.Role{domain.RoleAdmin}, http.StatusOK},
		{"forbidden", true, domain.RoleUser, []domain.Role{domain.RoleAdmin}, http.StatusForbidden},
		{"one-of", true, domain.RoleRestaurant, []domain.Role{domain.RoleAdmin, domain.RoleRestaurant}, http.StatusOK},
		{"no-auth-user", false, "", []domain.Role{domain.RoleAdmin}, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.set {
				ctx := context.WithValue(req.Context(), authUserKey{}, AuthUser{ID: uuid.New(), Role: string(tc.role)})
				req = req.WithContext(ctx)
			}
			c.Request = req
			handler := RequireRole(tc.allow...)
			handler(c)
			if !c.IsAborted() {
				c.Status(http.StatusOK)
			}
			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
		})
	}
}
