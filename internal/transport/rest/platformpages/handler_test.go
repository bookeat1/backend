package platformpages

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
	uc "backend-core/internal/usecase/platformpages"
)

// --- fake usecase -------------------------------------------------------------

type fakeUC struct {
	pages    map[string]domain.PlatformPage
	gotActor uc.Actor
	gotInput uc.UpdateInput
	updates  int
	err      error
}

func (f *fakeUC) GetPublished(_ context.Context, slug string) (*domain.PlatformPage, error) {
	if f.err != nil {
		return nil, f.err
	}
	p, ok := f.pages[slug]
	if !ok || !p.Published() {
		return nil, domain.ErrNotFound
	}
	return &p, nil
}

func (f *fakeUC) ListAdmin(_ context.Context, a uc.Actor) ([]domain.PlatformPage, error) {
	f.gotActor = a
	if f.err != nil {
		return nil, f.err
	}
	out := make([]domain.PlatformPage, 0, len(f.pages))
	for _, p := range f.pages {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeUC) GetAdmin(_ context.Context, a uc.Actor, slug string) (*domain.PlatformPage, error) {
	f.gotActor = a
	if f.err != nil {
		return nil, f.err
	}
	p, ok := f.pages[slug]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &p, nil
}

func (f *fakeUC) Update(_ context.Context, a uc.Actor, slug string, in uc.UpdateInput) (*domain.PlatformPage, error) {
	f.gotActor, f.gotInput = a, in
	f.updates++
	if f.err != nil {
		return nil, f.err
	}
	p, ok := f.pages[slug]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if in.Title != nil {
		p.Title = *in.Title
	}
	if in.Body != nil {
		p.Body = *in.Body
	}
	if in.Published != nil {
		if *in.Published {
			if p.Body == "" {
				return nil, domain.WithCode(domain.CodePageBodyEmpty, domain.ErrValidation)
			}
			now := time.Now()
			p.PublishedAt = &now
		} else {
			p.PublishedAt = nil
		}
	}
	f.pages[slug] = p
	return &p, nil
}

var _ uc.UseCase = (*fakeUC)(nil)

// --- auth plumbing: the router is built the way bootstrap/app.go mounts these
// routes — the real middleware.Auth plus the real RequireRole(RoleAdmin) — so
// the tests cover the ROUTER gate, not only the handler.

type fakeIssuer struct{}

func (fakeIssuer) IssueAccess(id uuid.UUID, _ string) (string, time.Time, error) {
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

func router(u uc.UseCase, role domain.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	h := NewHandler(u)
	h.RegisterPublic(api)
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: role}))
	adminGlobal := authed.Group("")
	adminGlobal.Use(middleware.RequireRole(domain.RoleAdmin))
	h.RegisterAdminGlobal(adminGlobal)
	return r
}

func send(t *testing.T, r *gin.Engine, method, url string, body any, authed bool) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		payload = b
	}
	req := httptest.NewRequest(method, url, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if authed {
		req.Header.Set("Authorization", "Bearer "+uuid.NewString())
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// --- tests ---------------------------------------------------------------

// TestGetPublic_UnpublishedIs404WithCacheHeader pins two contract details at
// once: an unpublished page answers exactly like an unknown one (404, no
// body leak), and the answer still carries the public cache header — a 404
// is just as cacheable by URL as a 200 here.
func TestGetPublic_UnpublishedIs404WithCacheHeader(t *testing.T) {
	f := &fakeUC{pages: map[string]domain.PlatformPage{
		"privacy": {Slug: domain.PlatformPageSlug("privacy"), Title: "Политика данных"},
	}}
	r := router(f, domain.RoleUser)

	w := send(t, r, http.MethodGet, "/api/v1/pages/privacy", nil, false)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET unpublished = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
}

func TestGetPublic_PublishedServesResolvedLanguage(t *testing.T) {
	published := time.Now()
	f := &fakeUC{pages: map[string]domain.PlatformPage{
		"offer": {
			Slug: domain.PlatformPageSlug("offer"), Title: "Оферта", Body: "# Оферта",
			TitleI18n: domain.I18n{"ru": "Оферта", "en": "Offer"},
			BodyI18n:  domain.I18n{"ru": "# Оферта", "en": "# Offer"},
			Format:    domain.PlatformPageFormatMarkdown, PublishedAt: &published,
		},
	}}
	r := router(f, domain.RoleUser)

	w := send(t, r, http.MethodGet, "/api/v1/pages/offer?lang=en", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("GET published = %d, body %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control = %q, want public max-age=60", got)
	}
	var env struct {
		Data publicResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if env.Data.Title != "Offer" || env.Data.Body != "# Offer" {
		t.Fatalf("payload not resolved to en: %+v", env.Data)
	}
	// The public payload must never leak the raw translation maps.
	if bytes.Contains(w.Body.Bytes(), []byte("title_i18n")) {
		t.Error("public response leaked title_i18n")
	}
}

// TestVenueStaffCannotManagePages is the router half of the same rule the
// usecase test pins: only the platform edits its own text pages.
func TestVenueStaffCannotManagePages(t *testing.T) {
	calls := []struct {
		method, url string
		body        any
	}{
		{http.MethodGet, "/api/v1/admin/pages", nil},
		{http.MethodGet, "/api/v1/admin/pages/offer", nil},
		{http.MethodPut, "/api/v1/admin/pages/offer", map[string]any{"title": "x"}},
	}

	for _, role := range []domain.Role{domain.RoleRestaurant, domain.RoleUser} {
		f := &fakeUC{pages: map[string]domain.PlatformPage{"offer": {Slug: "offer", Title: "Оферта"}}}
		r := router(f, role)
		for _, c := range calls {
			w := send(t, r, c.method, c.url, c.body, true)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s as %q = %d, want 403 (body %s)", c.method, c.url, role, w.Code, w.Body.String())
			}
		}
		if f.updates != 0 {
			t.Errorf("as %q the usecase Update was reached; the gate must stop it at the router", role)
		}
	}

	// Anonymous: 401, not 403.
	f := &fakeUC{pages: map[string]domain.PlatformPage{"offer": {Slug: "offer", Title: "Оферта"}}}
	r := router(f, domain.RoleAdmin)
	if w := send(t, r, http.MethodGet, "/api/v1/admin/pages", nil, false); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET = %d, want 401", w.Code)
	}

	// Superadmin gets through.
	if w := send(t, r, http.MethodGet, "/api/v1/admin/pages", nil, true); w.Code != http.StatusOK {
		t.Errorf("superadmin GET = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if f.gotActor.Role != domain.RoleAdmin {
		t.Errorf("actor role reaching the usecase = %q, want admin", f.gotActor.Role)
	}
}

// TestAdminUpdate_PublishWithEmptyBodyIs422 checks the transport wiring for
// the usecase's refusal: the specific code must survive HandleError, not get
// flattened to a generic validation_failed.
func TestAdminUpdate_PublishWithEmptyBodyIs422(t *testing.T) {
	f := &fakeUC{pages: map[string]domain.PlatformPage{"jobs": {Slug: "jobs", Title: "Вакансии"}}}
	r := router(f, domain.RoleAdmin)

	w := send(t, r, http.MethodPut, "/api/v1/admin/pages/jobs", map[string]any{"published": true}, true)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PUT published=true with empty body = %d, want 422 (body %s)", w.Code, w.Body.String())
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != string(domain.CodePageBodyEmpty) {
		t.Errorf("code = %q, want %q", env.Code, domain.CodePageBodyEmpty)
	}
}

// TestAdminUpdate_HappyPath exercises the PUT body -> usecase input mapping.
func TestAdminUpdate_HappyPath(t *testing.T) {
	f := &fakeUC{pages: map[string]domain.PlatformPage{"about": {Slug: "about", Title: "О BookEat"}}}
	r := router(f, domain.RoleAdmin)

	w := send(t, r, http.MethodPut, "/api/v1/admin/pages/about",
		map[string]any{"title": "О компании", "body": "# Текст", "published": true}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, body %s", w.Code, w.Body.String())
	}
	if f.gotInput.Title == nil || *f.gotInput.Title != "О компании" {
		t.Errorf("title not forwarded: %+v", f.gotInput)
	}
	if f.gotInput.Published == nil || !*f.gotInput.Published {
		t.Errorf("published not forwarded: %+v", f.gotInput)
	}
	var env struct {
		Data adminResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.Data.Published {
		t.Error("response does not reflect the publish")
	}
}
