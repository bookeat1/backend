package users

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
)

func newRouter(f *fakeFacade) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(f)

	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{}))
	h.RegisterRoutes(authed)
	return r
}

func do(r *gin.Engine, method, path string, body any, bearer string) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestMeReturnsOwnProfileAndCuisinePreferences(t *testing.T) {
	id := uuid.New()
	catID := uuid.New()
	f := &fakeFacade{
		user:       &domain.User{ID: id, FullName: "Alice", Role: domain.RoleUser, PreferredLanguage: "ru"},
		cuisineIDs: []uuid.UUID{catID},
	}
	w := do(newRouter(f), http.MethodGet, "/api/v1/users/me", nil, id.String())

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if f.lastMeID != id {
		t.Errorf("Me called with %v, want the caller's own id %v", f.lastMeID, id)
	}
	var env response.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	body, _ := json.Marshal(env.Data)
	var got userResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if got.FullName != "Alice" {
		t.Errorf("full_name = %q, want Alice", got.FullName)
	}
	if len(got.CuisineCategoryIDs) != 1 || got.CuisineCategoryIDs[0] != catID.String() {
		t.Errorf("cuisine_category_ids = %v, want [%s]", got.CuisineCategoryIDs, catID)
	}
}

// No route exists to read another user's id — /me always resolves the caller's
// own id from the auth token, never a path/body-supplied id. This test pins
// that: two different bearer tokens each only ever see their own id passed to
// the facade.
func TestMeNeverLeaksAnotherUsersID(t *testing.T) {
	self := uuid.New()
	other := uuid.New()
	f := &fakeFacade{user: &domain.User{ID: self, Role: domain.RoleUser}}

	w := do(newRouter(f), http.MethodGet, "/api/v1/users/me", nil, self.String())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if f.lastMeID != self {
		t.Fatalf("Me called with %v, want %v", f.lastMeID, self)
	}
	if f.lastMeID == other {
		t.Fatal("handler must never resolve another user's id")
	}
}

func TestMeRequiresAuth(t *testing.T) {
	f := &fakeFacade{}
	w := do(newRouter(f), http.MethodGet, "/api/v1/users/me", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestUpdateMePatchesOwnProfile(t *testing.T) {
	id := uuid.New()
	catID := uuid.New()
	f := &fakeFacade{
		user: &domain.User{ID: id, FullName: "New Name", Role: domain.RoleUser},
	}
	body := map[string]any{
		"full_name":            "New Name",
		"country_code":         "KZ",
		"birth_date":           "1998-05-04",
		"cuisine_category_ids": []string{catID.String()},
	}
	w := do(newRouter(f), http.MethodPatch, "/api/v1/users/me", body, id.String())

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if f.lastUpdateID != id {
		t.Errorf("UpdateMe called with %v, want %v", f.lastUpdateID, id)
	}
	if f.lastUpdateIn.CountryCode == nil || *f.lastUpdateIn.CountryCode != "KZ" {
		t.Errorf("country_code not forwarded: %+v", f.lastUpdateIn)
	}
	if f.lastUpdateIn.BirthDate == nil {
		t.Fatalf("birth_date not forwarded: %+v", f.lastUpdateIn)
	}
	if f.lastUpdateIn.CuisineCategoryIDs == nil || len(*f.lastUpdateIn.CuisineCategoryIDs) != 1 {
		t.Errorf("cuisine_category_ids not forwarded: %+v", f.lastUpdateIn)
	}
}

func TestUpdateMeRejectsMalformedBirthDate(t *testing.T) {
	id := uuid.New()
	f := &fakeFacade{user: &domain.User{ID: id}}
	body := map[string]any{"birth_date": "not-a-date"}
	w := do(newRouter(f), http.MethodPatch, "/api/v1/users/me", body, id.String())
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %s", w.Code, w.Body.String())
	}
}

func TestDeleteMeIsIdempotent(t *testing.T) {
	id := uuid.New()
	f := &fakeFacade{}
	r := newRouter(f)

	w1 := do(r, http.MethodDelete, "/api/v1/users/me", nil, id.String())
	if w1.Code != http.StatusOK {
		t.Fatalf("first delete status = %d, body = %s", w1.Code, w1.Body.String())
	}
	w2 := do(r, http.MethodDelete, "/api/v1/users/me", nil, id.String())
	if w2.Code != http.StatusOK {
		t.Fatalf("second delete status = %d, body = %s", w2.Code, w2.Body.String())
	}
	if f.deleteCalled != 2 {
		t.Fatalf("expected the facade to be called twice, got %d", f.deleteCalled)
	}
	if f.lastDeleteID != id {
		t.Errorf("DeleteMe called with %v, want %v", f.lastDeleteID, id)
	}
}
