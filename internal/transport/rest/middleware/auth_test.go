package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/token"
	"backend-core/internal/infrastructure/token/tokentest"
)

// fakeUserRepo is a minimal in-memory domain.UserRepository for middleware tests.
type fakeUserRepo struct {
	users map[uuid.UUID]*domain.User
}

func newFakeUserRepo() *fakeUserRepo {
	return &fakeUserRepo{users: map[uuid.UUID]*domain.User{}}
}

func (f *fakeUserRepo) Create(ctx context.Context, u *domain.User) error {
	f.users[u.ID] = u
	return nil
}

func (f *fakeUserRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	u, ok := f.users[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return u, nil
}

func (f *fakeUserRepo) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}

func (f *fakeUserRepo) GetByPhone(ctx context.Context, phone string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}

func (f *fakeUserRepo) Update(ctx context.Context, u *domain.User) error {
	f.users[u.ID] = u
	return nil
}

func runOptionalAuthMiddleware(t *testing.T, issuer *token.RSAIssuer, users domain.UserRepository, authHeader string) (*httptest.ResponseRecorder, bool, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	nextRan := false
	var sawAuthUser bool
	r.Use(OptionalAuth(issuer, users))
	r.GET("/maybe-protected", func(c *gin.Context) {
		nextRan = true
		_, sawAuthUser = GetAuthUser(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/maybe-protected", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w, nextRan, sawAuthUser
}

func TestOptionalAuth_ValidTokenAttachesUser(t *testing.T) {
	issuer := newTestIssuer(t)
	users := newFakeUserRepo()
	u := &domain.User{ID: uuid.New(), Role: domain.RoleUser, IsActive: true}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	access, _, err := issuer.IssueAccess(u.ID, string(u.Role))
	if err != nil {
		t.Fatalf("issue access: %v", err)
	}

	w, nextRan, sawAuthUser := runOptionalAuthMiddleware(t, issuer, users, "Bearer "+access)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !nextRan {
		t.Fatal("expected handler to run")
	}
	if !sawAuthUser {
		t.Error("expected AuthUser to be attached for a valid token")
	}
}

func TestOptionalAuth_NoTokenProceedsAnonymously(t *testing.T) {
	issuer := newTestIssuer(t)
	users := newFakeUserRepo()

	cases := []struct {
		name   string
		header string
	}{
		{"missing header", ""},
		{"garbage header", "not-a-bearer-token"},
		{"garbage bearer token", "Bearer garbage.token.value"},
		{"unknown user", ""}, // filled below
	}
	for _, tc := range cases[:3] {
		t.Run(tc.name, func(t *testing.T) {
			w, nextRan, sawAuthUser := runOptionalAuthMiddleware(t, issuer, users, tc.header)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want %d (must never reject an anonymous caller)", w.Code, http.StatusOK)
			}
			if !nextRan {
				t.Fatal("expected handler to run even with no/invalid token")
			}
			if sawAuthUser {
				t.Error("expected no AuthUser attached")
			}
		})
	}

	t.Run("unknown user", func(t *testing.T) {
		unknownID := uuid.New()
		access, _, err := issuer.IssueAccess(unknownID, string(domain.RoleUser))
		if err != nil {
			t.Fatalf("issue access: %v", err)
		}
		w, nextRan, sawAuthUser := runOptionalAuthMiddleware(t, issuer, users, "Bearer "+access)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if !nextRan || sawAuthUser {
			t.Errorf("nextRan=%v sawAuthUser=%v, want true/false", nextRan, sawAuthUser)
		}
	})

	t.Run("inactive user", func(t *testing.T) {
		u := &domain.User{ID: uuid.New(), Role: domain.RoleUser, IsActive: false}
		if err := users.Create(context.Background(), u); err != nil {
			t.Fatalf("create user: %v", err)
		}
		access, _, err := issuer.IssueAccess(u.ID, string(u.Role))
		if err != nil {
			t.Fatalf("issue access: %v", err)
		}
		w, nextRan, sawAuthUser := runOptionalAuthMiddleware(t, issuer, users, "Bearer "+access)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if !nextRan || sawAuthUser {
			t.Errorf("nextRan=%v sawAuthUser=%v, want true/false for an inactive account", nextRan, sawAuthUser)
		}
	})
}

func newTestIssuer(t *testing.T) *token.RSAIssuer {
	t.Helper()
	issuer, err := token.NewRSAIssuer(tokentest.GenerateKeyPEM(t), "test-kid", time.Hour)
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	return issuer
}

func runAuthMiddleware(t *testing.T, issuer *token.RSAIssuer, users domain.UserRepository, authHeader string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	nextRan := false
	r.Use(Auth(issuer, users))
	r.GET("/protected", func(c *gin.Context) {
		nextRan = true
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w, nextRan
}

func TestAuth_ValidTokenActiveUser(t *testing.T) {
	issuer := newTestIssuer(t)
	users := newFakeUserRepo()
	u := &domain.User{ID: uuid.New(), Role: domain.RoleUser, IsActive: true}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	access, _, err := issuer.IssueAccess(u.ID, string(u.Role))
	if err != nil {
		t.Fatalf("issue access: %v", err)
	}

	w, nextRan := runAuthMiddleware(t, issuer, users, "Bearer "+access)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !nextRan {
		t.Error("expected protected handler to run")
	}
}

func TestAuth_ValidTokenInactiveUser(t *testing.T) {
	issuer := newTestIssuer(t)
	users := newFakeUserRepo()
	u := &domain.User{ID: uuid.New(), Role: domain.RoleUser, IsActive: false}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	access, _, err := issuer.IssueAccess(u.ID, string(u.Role))
	if err != nil {
		t.Fatalf("issue access: %v", err)
	}

	w, nextRan := runAuthMiddleware(t, issuer, users, "Bearer "+access)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if nextRan {
		t.Error("expected protected handler NOT to run for inactive user")
	}
}

func TestAuth_ValidTokenUnknownUser(t *testing.T) {
	issuer := newTestIssuer(t)
	users := newFakeUserRepo()
	unknownID := uuid.New()
	access, _, err := issuer.IssueAccess(unknownID, string(domain.RoleUser))
	if err != nil {
		t.Fatalf("issue access: %v", err)
	}

	w, nextRan := runAuthMiddleware(t, issuer, users, "Bearer "+access)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if nextRan {
		t.Error("expected protected handler NOT to run for unknown user")
	}
}

func TestAuth_MissingOrGarbageHeader(t *testing.T) {
	issuer := newTestIssuer(t)
	users := newFakeUserRepo()

	cases := []struct {
		name   string
		header string
	}{
		{"missing", ""},
		{"garbage", "not-a-bearer-token"},
		{"garbage bearer", "Bearer garbage.token.value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, nextRan := runAuthMiddleware(t, issuer, users, tc.header)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
			}
			if nextRan {
				t.Error("expected protected handler NOT to run")
			}
		})
	}
}
