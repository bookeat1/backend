package stories

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
)

// --- auth plumbing: the admin router runs the real middleware.Auth, so the
// tests exercise the same AuthUser path as production. The access token is the
// user id (mirrors the reviews/bookings handler tests). ---

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

// adminRouter mounts the admin routes behind the real auth middleware, with the
// authenticated user carrying the given role.
func adminRouter(f *fakeFacade, role domain.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(f)
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: role}))
	h.RegisterAdminRoutes(authed)
	return r
}

func doAdmin(r *gin.Engine, method, path string, body any, rawBody []byte, token *uuid.UUID) *httptest.ResponseRecorder {
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
	if token != nil {
		req.Header.Set("Authorization", "Bearer "+token.String())
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func sampleStory() *domain.Story {
	cap := "Летнее меню"
	return &domain.Story{
		ID: uuid.New(), RestaurantID: uuid.New(),
		ImageURL: "https://cdn.book-eat.com/stories/a.jpg", Caption: &cap,
		SortOrder: 2, IsActive: true, CreatedAt: time.Now(),
	}
}

// TestAdminRoutesRequireAuth: every admin route rejects an unauthenticated
// request with 401 before touching the facade.
func TestAdminRoutesRequireAuth(t *testing.T) {
	rid := uuid.New().String()
	sid := uuid.New().String()
	r := adminRouter(&fakeFacade{story: sampleStory()}, domain.RoleRestaurant)

	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/restaurants/" + rid + "/stories"},
		{http.MethodPost, "/api/v1/admin/restaurants/" + rid + "/stories"},
		{http.MethodPut, "/api/v1/admin/stories/" + sid},
		{http.MethodDelete, "/api/v1/admin/stories/" + sid},
		{http.MethodPost, "/api/v1/admin/restaurants/" + rid + "/stories/reorder"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := doAdmin(r, tc.method, tc.path, gin.H{}, nil, nil) // no token
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", w.Code, w.Body)
			}
		})
	}
}

// TestCreateReturnsTheStory: an authenticated manager gets 200 and the created
// story's JSON, including is_active and created_at (the admin shape).
func TestCreateReturnsTheStory(t *testing.T) {
	story := sampleStory()
	f := &fakeFacade{story: story}
	r := adminRouter(f, domain.RoleRestaurant)
	token := uuid.New()

	body := gin.H{"image_url": story.ImageURL, "caption": "Летнее меню"}
	w := doAdmin(r, http.MethodPost, "/api/v1/admin/restaurants/"+story.RestaurantID.String()+"/stories", body, nil, &token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	// The facade received the image_url and caption from the body.
	if f.created == nil || f.created.ImageURL != story.ImageURL {
		t.Fatalf("facade did not receive the create input: %+v", f.created)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data["id"] != story.ID.String() {
		t.Errorf("id = %v, want %v", env.Data["id"], story.ID)
	}
	if env.Data["is_active"] != true {
		t.Errorf("is_active missing/false in admin response: %v", env.Data)
	}
	if _, ok := env.Data["created_at"]; !ok {
		t.Errorf("created_at missing in admin response: %v", env.Data)
	}
}

// TestCreateBadBody: a malformed JSON body is a 422, and never reaches the facade.
func TestCreateBadBody(t *testing.T) {
	f := &fakeFacade{story: sampleStory()}
	r := adminRouter(f, domain.RoleRestaurant)
	token := uuid.New()

	w := doAdmin(r, http.MethodPost, "/api/v1/admin/restaurants/"+uuid.New().String()+"/stories",
		nil, []byte(`{"image_url": 12345}`), &token) // image_url wrong type
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
	}
	if f.created != nil {
		t.Fatal("a malformed body must not reach the facade")
	}
}

// TestCreateBadRestaurantID: a malformed :id is a 422.
func TestCreateBadRestaurantID(t *testing.T) {
	f := &fakeFacade{story: sampleStory()}
	r := adminRouter(f, domain.RoleRestaurant)
	token := uuid.New()

	w := doAdmin(r, http.MethodPost, "/api/v1/admin/restaurants/"+badUUID+"/stories",
		gin.H{"image_url": "https://cdn/a.jpg"}, nil, &token)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
	}
}

// TestNonManagerDenied: when the usecase reports ErrForbidden (the caller does
// not manage this restaurant), the handler maps it to 403.
func TestNonManagerDenied(t *testing.T) {
	f := &fakeFacade{adminErr: fmt.Errorf("%w: not a manager", domain.ErrForbidden)}
	r := adminRouter(f, domain.RoleRestaurant)
	token := uuid.New()

	w := doAdmin(r, http.MethodPost, "/api/v1/admin/restaurants/"+uuid.New().String()+"/stories",
		gin.H{"image_url": "https://cdn/a.jpg"}, nil, &token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body)
	}
}

// TestReorderParsesIDs: a well-formed reorder reaches the facade with the parsed
// ids in order; a malformed id in the list is a 422 that never reaches it.
func TestReorderParsesIDs(t *testing.T) {
	f := &fakeFacade{}
	r := adminRouter(f, domain.RoleRestaurant)
	token := uuid.New()
	rid := uuid.New()
	id1, id2 := uuid.New(), uuid.New()

	w := doAdmin(r, http.MethodPost, "/api/v1/admin/restaurants/"+rid.String()+"/stories/reorder",
		gin.H{"ordered_ids": []string{id1.String(), id2.String()}}, nil, &token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	if len(f.reordered) != 2 || f.reordered[0] != id1 || f.reordered[1] != id2 || f.reorderedRID != rid {
		t.Fatalf("facade got wrong reorder args: rid=%v ids=%v", f.reorderedRID, f.reordered)
	}

	f.reordered = nil
	w = doAdmin(r, http.MethodPost, "/api/v1/admin/restaurants/"+rid.String()+"/stories/reorder",
		gin.H{"ordered_ids": []string{"not-a-uuid"}}, nil, &token)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
	}
	if f.reordered != nil {
		t.Fatal("a malformed ordered_ids must not reach the facade")
	}
}

// TestDeleteReturnsStatus: an authenticated delete returns 200 with a status body.
func TestDeleteReturnsStatus(t *testing.T) {
	f := &fakeFacade{}
	r := adminRouter(f, domain.RoleRestaurant)
	token := uuid.New()

	w := doAdmin(r, http.MethodDelete, "/api/v1/admin/stories/"+uuid.New().String(), nil, nil, &token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
}

// TestCreatePassesActionURL: the body's action_url reaches the facade as the
// LINK, untouched, and comes back in the admin response — and it is carried
// independently of image_url, which stays the picture's address.
func TestCreatePassesActionURL(t *testing.T) {
	story := sampleStory()
	link := "https://book-eat.com/promo"
	story.ActionURL = &link
	f := &fakeFacade{story: story}
	r := adminRouter(f, domain.RoleRestaurant)
	token := uuid.New()

	body := gin.H{"image_url": story.ImageURL, "action_url": link}
	w := doAdmin(r, http.MethodPost, "/api/v1/admin/restaurants/"+story.RestaurantID.String()+"/stories", body, nil, &token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	if f.created == nil || f.created.ActionURL == nil || *f.created.ActionURL != link {
		t.Fatalf("facade did not receive action_url: %+v", f.created)
	}
	if f.created.ImageURL != story.ImageURL {
		t.Fatalf("image_url must be unaffected, got %q", f.created.ImageURL)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data["action_url"] != link {
		t.Errorf("admin response action_url = %v, want %q", env.Data["action_url"], link)
	}
}

// TestUpdatePassesActionURL: an explicit empty action_url in the PATCH body must
// reach the facade as a set-but-empty pointer — that is how the operator clears
// the link — and must not be confused with an omitted field.
func TestUpdatePassesActionURL(t *testing.T) {
	story := sampleStory()
	f := &fakeFacade{story: story}
	r := adminRouter(f, domain.RoleRestaurant)
	token := uuid.New()

	body := gin.H{"action_url": ""}
	w := doAdmin(r, http.MethodPut, "/api/v1/admin/stories/"+story.ID.String(), body, nil, &token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	if f.updated == nil || f.updated.ActionURL == nil || *f.updated.ActionURL != "" {
		t.Fatalf("an explicit empty action_url must reach the facade: %+v", f.updated)
	}
	if f.updated.ImageURL != nil {
		t.Fatalf("image_url must stay omitted, got %v", *f.updated.ImageURL)
	}
}
