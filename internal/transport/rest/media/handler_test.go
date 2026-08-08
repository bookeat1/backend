package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
)

// --- auth plumbing: the router runs the real middleware.Auth, so these tests
// exercise the same AuthUser + RequireRole path as production. The access token
// is the user id (mirrors the reviews/bookings handler tests).

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

// --- fake Store: records the last write, one settable err drives the store
// error → HTTP mapping. It NEVER touches a real R2 bucket.
type fakeStore struct {
	err error

	putCalled  bool
	gotKey     string
	gotBody    []byte
	gotType    string
	publicBase string
}

func (f *fakeStore) PutOriginal(_ context.Context, key string, body []byte, contentType string) error {
	if f.err != nil {
		return f.err
	}
	f.putCalled = true
	f.gotKey = key
	f.gotBody = body
	f.gotType = contentType
	return nil
}

func (f *fakeStore) PublicURL(key string) string {
	base := f.publicBase
	if base == "" {
		base = "https://public.example"
	}
	return base + "/" + key
}

// newRouter builds a router whose authed group runs the real Auth middleware
// with a user of role `role`. store is passed straight to NewHandler; pass a
// typed *fakeStore for the configured case, or the untyped nil literal via
// newUnconfiguredRouter for the 503 case.
func newRouter(store Store, role domain.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: role}))
	NewHandler(store).RegisterRoutes(authed)
	return r
}

const uploadPath = "/api/v1/admin/media/images"

// jpegBytes returns a buffer of length n whose first bytes are the JPEG magic,
// so http.DetectContentType sniffs it as image/jpeg.
func jpegBytes(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'})
	return b
}

func pngBytes(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	return b
}

// multipartBody builds a multipart/form-data body with one file part under
// `field`. Returns the body and the Content-Type header to send with it.
func multipartBody(field, filename string, content []byte) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile(field, filename)
	_, _ = fw.Write(content)
	_ = w.Close()
	return &buf, w.FormDataContentType()
}

// doUpload sends a multipart POST. When token != "" it is sent as a bearer.
func doUpload(r *gin.Engine, token string, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, uploadPath, body)
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestUploadHappyPath: a manager uploads a JPEG and gets back the public URL
// built from the store's public base + the generated uploads/ key, and the
// store received the sniffed content type and the exact bytes.
func TestUploadHappyPath(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(store, domain.RoleRestaurant)
	content := jpegBytes(1024)
	body, ct := multipartBody("file", "cover.jpg", content)

	w := doUpload(r, uuid.NewString(), body, ct)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	var env struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !store.putCalled {
		t.Fatal("store.PutOriginal was not called")
	}
	if !strings.HasPrefix(store.gotKey, "uploads/") || !strings.HasSuffix(store.gotKey, ".jpg") {
		t.Errorf("stored key = %q, want uploads/<...>.jpg", store.gotKey)
	}
	if store.gotType != "image/jpeg" {
		t.Errorf("stored content type = %q, want image/jpeg (sniffed)", store.gotType)
	}
	if !bytes.Equal(store.gotBody, content) {
		t.Errorf("stored body differs from uploaded bytes (%d vs %d)", len(store.gotBody), len(content))
	}
	wantURL := "https://public.example/" + store.gotKey
	if env.Data.URL != wantURL {
		t.Errorf("url = %q, want %q", env.Data.URL, wantURL)
	}
}

// TestUploadPNGSniffedExtension: a PNG is accepted and keyed with .png, from
// the sniffed type (not the .jpg filename the client sent).
func TestUploadPNGSniffedExtension(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(store, domain.RoleAdmin)
	body, ct := multipartBody("file", "lie.jpg", pngBytes(2048))

	w := doUpload(r, uuid.NewString(), body, ct)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	if store.gotType != "image/png" || !strings.HasSuffix(store.gotKey, ".png") {
		t.Errorf("type=%q key=%q, want image/png + .png", store.gotType, store.gotKey)
	}
}

// TestUploadRejectsNonImage: a text/plain body is refused 422 and never reaches
// the store.
func TestUploadRejectsNonImage(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(store, domain.RoleRestaurant)
	body, ct := multipartBody("file", "notes.txt", []byte("this is plainly not an image, just prose"))

	w := doUpload(r, uuid.NewString(), body, ct)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
	}
	if store.putCalled {
		t.Error("store was written for a non-image upload")
	}
}

// TestUploadRejectsOversize: a JPEG over 8 MiB is refused 413 and never reaches
// the store.
func TestUploadRejectsOversize(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(store, domain.RoleRestaurant)
	body, ct := multipartBody("file", "huge.jpg", jpegBytes(maxUploadBytes+1024))

	w := doUpload(r, uuid.NewString(), body, ct)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", w.Code, w.Body)
	}
	if store.putCalled {
		t.Error("store was written for an oversize upload")
	}
}

// TestUploadRejectsMissingFile: the form carries no "file" field → 422.
func TestUploadRejectsMissingFile(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(store, domain.RoleRestaurant)
	body, ct := multipartBody("attachment", "cover.jpg", jpegBytes(512))

	w := doUpload(r, uuid.NewString(), body, ct)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body)
	}
	if store.putCalled {
		t.Error("store was written when no file field was present")
	}
}

// TestUploadRequiresAuth: no bearer token → 401.
func TestUploadRequiresAuth(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(store, domain.RoleRestaurant)
	body, ct := multipartBody("file", "cover.jpg", jpegBytes(512))

	w := doUpload(r, "", body, ct)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", w.Code, w.Body)
	}
}

// TestUploadForbidsGuest: an authenticated plain user (RoleUser) is not staff
// and must be refused 403 by the RequireRole gate.
func TestUploadForbidsGuest(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(store, domain.RoleUser)
	body, ct := multipartBody("file", "cover.jpg", jpegBytes(512))

	w := doUpload(r, uuid.NewString(), body, ct)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body)
	}
	if store.putCalled {
		t.Error("store was written for a forbidden guest")
	}
}

// TestUploadNotConfigured: when the store is nil (R2 not configured) a staff
// upload gets 503, not a panic. The handler must receive a TRUE nil interface.
func TestUploadNotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: domain.RoleRestaurant}))
	NewHandler(nil).RegisterRoutes(authed) // nil literal → true nil interface

	body, ct := multipartBody("file", "cover.jpg", jpegBytes(512))
	w := doUpload(r, uuid.NewString(), body, ct)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", w.Code, w.Body)
	}
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Code != string(domain.CodeUnavailable) {
		t.Errorf("code = %q, want %q", env.Code, domain.CodeUnavailable)
	}
}

// TestUploadStoreError: a store write failure maps to 500 via HandleError
// (generic internal error, no leak of the store's message).
func TestUploadStoreError(t *testing.T) {
	store := &fakeStore{err: errors.New("r2 exploded")}
	r := newRouter(store, domain.RoleRestaurant)
	body, ct := multipartBody("file", "cover.jpg", jpegBytes(512))

	w := doUpload(r, uuid.NewString(), body, ct)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "r2 exploded") {
		t.Error("store's internal error text leaked to the client")
	}
}

// TestNewObjectKeyShape: the generated key is uploads/<YYYY>/<MM>/<32hex><ext>,
// and two calls never collide.
func TestNewObjectKeyShape(t *testing.T) {
	now := time.Date(2026, time.August, 8, 0, 0, 0, 0, time.UTC)
	k1, err := newObjectKey(now, ".jpg")
	if err != nil {
		t.Fatalf("newObjectKey: %v", err)
	}
	if !strings.HasPrefix(k1, "uploads/2026/08/") || !strings.HasSuffix(k1, ".jpg") {
		t.Errorf("key = %q, want uploads/2026/08/<hex>.jpg", k1)
	}
	// 16 random bytes → 32 hex chars.
	hexPart := strings.TrimSuffix(strings.TrimPrefix(k1, "uploads/2026/08/"), ".jpg")
	if len(hexPart) != 32 {
		t.Errorf("hex part = %q (len %d), want 32", hexPart, len(hexPart))
	}
	k2, _ := newObjectKey(now, ".jpg")
	if k1 == k2 {
		t.Error("two generated keys collided")
	}
}
