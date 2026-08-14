package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
)

// The guest avatar route breaks the rule the admin route next door enforces —
// a plain guest MAY upload here. What makes that safe is one property: the
// stored URL lands on the caller's own profile in the same request, and the
// owner comes from the token. These tests pin exactly that, plus the failure
// the guest would otherwise never notice: a stored image that never reached
// the profile, shown as a new avatar that vanishes on the next screen open.

const avatarPath = "/api/v1/users/me/avatar"

type fakeAvatars struct {
	err    error
	gotID  uuid.UUID
	gotURL string
	calls  int
}

func (f *fakeAvatars) SetAvatarURL(_ context.Context, userID uuid.UUID, url string) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.gotID, f.gotURL = userID, url
	return nil
}

func newAvatarRouter(store Store, avatars AvatarSetter, role domain.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: role}))
	NewHandler(store, WithAvatarSetter(avatars)).RegisterUserRoutes(authed)
	return r
}

func postAvatar(r *gin.Engine, token string, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, avatarPath, body)
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAvatarUploadStoresAndAttachesToTheCaller(t *testing.T) {
	store := &fakeStore{}
	avatars := &fakeAvatars{}
	// A plain guest — the role the admin upload refuses.
	r := newAvatarRouter(store, avatars, domain.RoleUser)
	caller := uuid.New()

	body, ct := multipartBody("file", "me.jpg", jpegBytes(2048))
	w := postAvatar(r, caller.String(), body, ct)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var env struct {
		Data uploadResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !store.putCalled || store.gotType != "image/jpeg" {
		t.Fatalf("store got type %q, put=%v", store.gotType, store.putCalled)
	}
	if avatars.gotID != caller {
		t.Fatalf("avatar written for %s, want the caller %s", avatars.gotID, caller)
	}
	if avatars.gotURL != env.Data.URL || !strings.Contains(env.Data.URL, store.gotKey) {
		t.Fatalf("profile got %q, response %q, key %q — the three must agree",
			avatars.gotURL, env.Data.URL, store.gotKey)
	}
}

func TestAvatarUploadRequiresAuth(t *testing.T) {
	avatars := &fakeAvatars{}
	r := newAvatarRouter(&fakeStore{}, avatars, domain.RoleUser)

	body, ct := multipartBody("file", "me.jpg", jpegBytes(1024))
	if w := postAvatar(r, "", body, ct); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if avatars.calls != 0 {
		t.Fatal("an anonymous request must not touch any profile")
	}
}

func TestAvatarUploadFailsWhenTheProfileWriteFails(t *testing.T) {
	// The image is in the bucket, the profile is not updated. Answering 200
	// would show the guest a new avatar that disappears on the next open — the
	// request has to fail instead.
	store := &fakeStore{}
	avatars := &fakeAvatars{err: errors.New("database is down")}
	r := newAvatarRouter(store, avatars, domain.RoleUser)

	body, ct := multipartBody("file", "me.jpg", jpegBytes(1024))
	w := postAvatar(r, uuid.New().String(), body, ct)

	if w.Code < 500 {
		t.Fatalf("status = %d, want a server error", w.Code)
	}
}

func TestAvatarUploadRejectsNonImage(t *testing.T) {
	store := &fakeStore{}
	avatars := &fakeAvatars{}
	r := newAvatarRouter(store, avatars, domain.RoleUser)

	body, ct := multipartBody("file", "note.txt", []byte(strings.Repeat("hello", 200)))
	w := postAvatar(r, uuid.New().String(), body, ct)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	if store.putCalled || avatars.calls != 0 {
		t.Fatal("a rejected file must reach neither the bucket nor the profile")
	}
}

func TestAvatarUploadRejectsOversize(t *testing.T) {
	store := &fakeStore{}
	avatars := &fakeAvatars{}
	r := newAvatarRouter(store, avatars, domain.RoleUser)

	body, ct := multipartBody("file", "huge.jpg", jpegBytes(avatarMaxBytes+1))
	w := postAvatar(r, uuid.New().String(), body, ct)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
	if store.putCalled {
		t.Fatal("an oversize image must not be stored")
	}
}

func TestAvatarUploadNotConfigured(t *testing.T) {
	// No R2 on this deployment: the route stays mounted and says so, rather
	// than panicking on a nil store.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: domain.RoleUser}))
	NewHandler(nil, WithAvatarSetter(&fakeAvatars{})).RegisterUserRoutes(authed)

	body, ct := multipartBody("file", "me.jpg", jpegBytes(1024))
	if w := postAvatar(r, uuid.New().String(), body, ct); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}
