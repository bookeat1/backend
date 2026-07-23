package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

func TestLogUserContextAttachesUserIDAndRole(t *testing.T) {
	buf := withCapturedDefaultLogger(t)

	issuer := newTestIssuer(t)
	users := newFakeUserRepo()
	u := &domain.User{ID: uuid.New(), Role: domain.RoleAdmin, IsActive: true}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	access, _, err := issuer.IssueAccess(u.ID, string(u.Role))
	if err != nil {
		t.Fatalf("issue access: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.Use(Auth(issuer, users))
	r.Use(LogUserContext())
	r.GET("/x", func(c *gin.Context) {
		logging.FromContext(c.Request.Context()).Info("handler.reached")
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	line := lastLogLine(t, buf)
	if line["user_id"] != u.ID.String() {
		t.Errorf("user_id = %v, want %s", line["user_id"], u.ID.String())
	}
	if line["role"] != string(domain.RoleAdmin) {
		t.Errorf("role = %v, want %s", line["role"], domain.RoleAdmin)
	}
	if _, ok := line["request_id"]; !ok {
		t.Error("expected request_id to still be present after LogUserContext")
	}
}

func TestLogUserContextNoopWithoutAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.Use(LogUserContext())
	var ranHandler bool
	r.GET("/x", func(c *gin.Context) {
		ranHandler = true
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !ranHandler {
		t.Fatalf("expected the request to proceed normally with no AuthUser on context")
	}
}
