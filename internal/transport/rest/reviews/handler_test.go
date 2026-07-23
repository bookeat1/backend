package reviews

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
	uc "backend-core/internal/usecase/reviews"
)

// --- auth plumbing: the test router runs the real middleware.Auth, so these
// tests exercise the same AuthUser path as production. The access token is the
// user id (mirrors the bookings/payments handler tests).

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

// --- fake reviews.Facade: one settable err drives the error→HTTP mapping, and
// canned return values drive the happy paths.
type fakeFacade struct {
	err error
	rv  *domain.Review
}

func (f *fakeFacade) review() *domain.Review {
	if f.rv != nil {
		return f.rv
	}
	return &domain.Review{
		ID: uuid.New(), RestaurantID: uuid.New(), UserID: uuid.New(),
		Rating: 5, Body: "хорошо", Status: domain.ReviewPublished,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

func (f *fakeFacade) Submit(context.Context, uuid.UUID, uc.SubmitInput) (*domain.Review, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.review(), nil
}

func (f *fakeFacade) DeleteOwn(context.Context, uuid.UUID, uuid.UUID) error { return f.err }

func (f *fakeFacade) GetOwn(context.Context, uuid.UUID, uuid.UUID) (*domain.Review, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.review(), nil
}

func (f *fakeFacade) ListPublished(context.Context, uuid.UUID, int, int) ([]domain.ReviewListItem, int, error) {
	if f.err != nil {
		return nil, 0, f.err
	}
	return []domain.ReviewListItem{{Review: *f.review(), AuthorName: "Дамир"}}, 1, nil
}

func (f *fakeFacade) Rating(_ context.Context, rid uuid.UUID) (domain.RatingAggregate, error) {
	if f.err != nil {
		return domain.RatingAggregate{}, f.err
	}
	return domain.RatingAggregate{RestaurantID: rid, Average: 4.5, Count: 10}, nil
}

func (f *fakeFacade) Reply(context.Context, uc.Actor, uuid.UUID, string) (*domain.Review, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.review(), nil
}

func (f *fakeFacade) Moderate(context.Context, uc.Actor, uuid.UUID, domain.ReviewStatus) (*domain.Review, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.review(), nil
}

func newRouter(f *fakeFacade) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(f)
	api := r.Group("/api/v1")
	h.RegisterPublic(api)
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: domain.RoleUser}))
	h.RegisterGuestRoutes(authed)
	h.RegisterStaffRoutes(authed)
	return r
}

// do sends a request. rawBody, when non-nil, is used verbatim (for malformed
// JSON); otherwise body is JSON-encoded.
func do(r *gin.Engine, method, path string, body any, rawBody []byte, headers map[string]string) *httptest.ResponseRecorder {
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
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func authHeader(userID uuid.UUID) map[string]string {
	return map[string]string{"Authorization": "Bearer " + userID.String()}
}

const badUUID = "not-a-uuid"

// TestPublicRoutesHappyAndBadID covers the two unauthenticated read routes:
// a good restaurant id returns 200, a malformed one returns 422 (the handler
// parses :id itself and writes 422 on a parse failure).
func TestPublicRoutesHappyAndBadID(t *testing.T) {
	rid := uuid.New().String()
	r := newRouter(&fakeFacade{})

	cases := []struct {
		name, path string
		want       int
	}{
		{"list happy", "/api/v1/restaurants/" + rid + "/reviews", http.StatusOK},
		{"list bad id", "/api/v1/restaurants/" + badUUID + "/reviews", http.StatusUnprocessableEntity},
		{"summary happy", "/api/v1/restaurants/" + rid + "/reviews/summary", http.StatusOK},
		{"summary bad id", "/api/v1/restaurants/" + badUUID + "/reviews/summary", http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(r, http.MethodGet, tc.path, nil, nil, nil)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
	}
}

// TestListPaginationNonInteger documents the ACTUAL behavior for a non-integer
// ?page / ?per_page: strconv.Atoi discards the error, so the handler falls back
// to page=1 / per_page=default and still answers 200 rather than 400.
func TestListPaginationNonInteger(t *testing.T) {
	r := newRouter(&fakeFacade{})
	path := "/api/v1/restaurants/" + uuid.New().String() + "/reviews?page=abc&per_page=xyz"
	w := do(r, http.MethodGet, path, nil, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (non-integer pagination is silently defaulted) (body %s)", w.Code, w.Body)
	}
	var env struct {
		Data struct {
			Page    int `json:"page"`
			PerPage int `json:"per_page"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Page != 1 || env.Data.PerPage != defaultPerPage {
		t.Errorf("defaulted page/per_page = %d/%d, want 1/%d", env.Data.Page, env.Data.PerPage, defaultPerPage)
	}
}

func TestGuestRoutesUnauthenticated(t *testing.T) {
	r := newRouter(&fakeFacade{})
	base := "/api/v1/restaurants/" + uuid.New().String() + "/reviews/me"
	cases := []struct{ method string }{{http.MethodGet}, {http.MethodPut}, {http.MethodDelete}}
	for _, tc := range cases {
		w := do(r, tc.method, base, gin.H{}, nil, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401 (body %s)", tc.method, base, w.Code, w.Body)
		}
	}
}

func TestGuestRoutesBadRestaurantID(t *testing.T) {
	r := newRouter(&fakeFacade{})
	base := "/api/v1/restaurants/" + badUUID + "/reviews/me"
	uid := uuid.New()
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		w := do(r, m, base, gin.H{"rating": 5}, nil, authHeader(uid))
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422 (body %s)", m, w.Code, w.Body)
		}
	}
}

// TestSubmitMalformedRating: a non-integer rating fails JSON binding and maps to
// 422 (the handler's ShouldBindJSON path).
func TestSubmitMalformedRating(t *testing.T) {
	r := newRouter(&fakeFacade{})
	path := "/api/v1/restaurants/" + uuid.New().String() + "/reviews/me"
	w := do(r, http.MethodPut, path, nil, []byte(`{"rating":"five","body":"x"}`), authHeader(uuid.New()))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
	}
}

func TestSubmitMalformedJSON(t *testing.T) {
	r := newRouter(&fakeFacade{})
	path := "/api/v1/restaurants/" + uuid.New().String() + "/reviews/me"
	w := do(r, http.MethodPut, path, nil, []byte(`{not json`), authHeader(uuid.New()))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
	}
}

// TestGuestErrorMapping asserts the domain error → HTTP status mapping the
// response package actually implements, exercised through the guest routes.
func TestGuestErrorMapping(t *testing.T) {
	uid := uuid.New()
	path := "/api/v1/restaurants/" + uuid.New().String() + "/reviews/me"
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"forbidden", fmt.Errorf("%w: no completed booking", domain.ErrForbidden), http.StatusForbidden},
		{"not found", fmt.Errorf("%w: review", domain.ErrNotFound), http.StatusNotFound},
		{"validation", fmt.Errorf("%w: rating range", domain.ErrValidation), http.StatusUnprocessableEntity},
		{"conflict", fmt.Errorf("%w: review", domain.ErrAlreadyExists), http.StatusConflict},
		{"internal", fmt.Errorf("db exploded"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run("submit "+tc.name, func(t *testing.T) {
			r := newRouter(&fakeFacade{err: tc.err})
			w := do(r, http.MethodPut, path, gin.H{"rating": 5, "body": "x"}, nil, authHeader(uid))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
		t.Run("getOwn "+tc.name, func(t *testing.T) {
			r := newRouter(&fakeFacade{err: tc.err})
			w := do(r, http.MethodGet, path, nil, nil, authHeader(uid))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
	}
}

func TestGuestHappyPaths(t *testing.T) {
	uid := uuid.New()
	path := "/api/v1/restaurants/" + uuid.New().String() + "/reviews/me"
	r := newRouter(&fakeFacade{})

	if w := do(r, http.MethodPut, path, gin.H{"rating": 5, "body": "отлично"}, nil, authHeader(uid)); w.Code != http.StatusOK {
		t.Fatalf("submit: status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	if w := do(r, http.MethodGet, path, nil, nil, authHeader(uid)); w.Code != http.StatusOK {
		t.Fatalf("getOwn: status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	if w := do(r, http.MethodDelete, path, nil, nil, authHeader(uid)); w.Code != http.StatusOK {
		t.Fatalf("deleteOwn: status = %d, want 200 (body %s)", w.Code, w.Body)
	}
}

// TestReplyBadReviewID / missing body.
func TestReplyValidation(t *testing.T) {
	uid := uuid.New()
	r := newRouter(&fakeFacade{})

	t.Run("bad review id", func(t *testing.T) {
		w := do(r, http.MethodPost, "/api/v1/reviews/"+badUUID+"/reply", gin.H{"reply": "спасибо"}, nil, authHeader(uid))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
		}
	})
	t.Run("missing reply (binding required)", func(t *testing.T) {
		w := do(r, http.MethodPost, "/api/v1/reviews/"+uuid.New().String()+"/reply", gin.H{}, nil, authHeader(uid))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
		}
	})
	t.Run("happy", func(t *testing.T) {
		w := do(r, http.MethodPost, "/api/v1/reviews/"+uuid.New().String()+"/reply", gin.H{"reply": "спасибо"}, nil, authHeader(uid))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
		}
	})
}

// TestModerateStatusValidation: the handler validates the status enum itself
// (before the usecase), returning 422 for an unknown value and for a missing
// status (binding:"required").
func TestModerateStatusValidation(t *testing.T) {
	uid := uuid.New()
	r := newRouter(&fakeFacade{})
	base := "/api/v1/reviews/" + uuid.New().String() + "/status"

	t.Run("unknown status", func(t *testing.T) {
		w := do(r, http.MethodPatch, base, gin.H{"status": "banana"}, nil, authHeader(uid))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
		}
	})
	t.Run("missing status", func(t *testing.T) {
		w := do(r, http.MethodPatch, base, gin.H{}, nil, authHeader(uid))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
		}
	})
	t.Run("bad review id", func(t *testing.T) {
		w := do(r, http.MethodPatch, "/api/v1/reviews/"+badUUID+"/status", gin.H{"status": "hidden"}, nil, authHeader(uid))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
		}
	})
	t.Run("happy published", func(t *testing.T) {
		w := do(r, http.MethodPatch, base, gin.H{"status": "published"}, nil, authHeader(uid))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
		}
	})
}

// TestStaffErrorMapping exercises the error→status mapping on the staff routes.
func TestStaffErrorMapping(t *testing.T) {
	uid := uuid.New()
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"forbidden", fmt.Errorf("%w: not staff here", domain.ErrForbidden), http.StatusForbidden},
		{"not found", fmt.Errorf("%w: review", domain.ErrNotFound), http.StatusNotFound},
		{"validation", fmt.Errorf("%w: bad", domain.ErrValidation), http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run("reply "+tc.name, func(t *testing.T) {
			r := newRouter(&fakeFacade{err: tc.err})
			w := do(r, http.MethodPost, "/api/v1/reviews/"+uuid.New().String()+"/reply", gin.H{"reply": "x"}, nil, authHeader(uid))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
		t.Run("moderate "+tc.name, func(t *testing.T) {
			r := newRouter(&fakeFacade{err: tc.err})
			w := do(r, http.MethodPatch, "/api/v1/reviews/"+uuid.New().String()+"/status", gin.H{"status": "hidden"}, nil, authHeader(uid))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
	}
}
