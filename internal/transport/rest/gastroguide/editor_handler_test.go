package gastroguide

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
	uc "backend-core/internal/usecase/gastroguide"
)

// fakeEditor records what the handler passed down and answers with whatever the
// test set. The authorization itself is the usecase's (and the router group's);
// what is checked here is that the handler carries the actor's ROLE through
// unchanged and turns a refusal into the right status and code.
type fakeEditor struct {
	err error

	gotActor  uc.EditorActor
	gotOrder  []uuid.UUID
	gotColl   uuid.UUID
	gotAttach uc.AttachVenueInput
	calls     int

	gotCollection uc.CollectionInput
	gotAdminList  uc.AdminListInput
}

func (f *fakeEditor) ListCategories(_ context.Context, a uc.EditorActor) ([]domain.GuideCategory, error) {
	f.calls, f.gotActor = f.calls+1, a
	return nil, f.err
}

func (f *fakeEditor) CreateCategory(_ context.Context, a uc.EditorActor, _ uc.CategoryInput) (*domain.GuideCategory, error) {
	f.calls, f.gotActor = f.calls+1, a
	return &domain.GuideCategory{ID: uuid.New()}, f.err
}

func (f *fakeEditor) UpdateCategory(_ context.Context, a uc.EditorActor, _ uuid.UUID, _ uc.CategoryInput) (*domain.GuideCategory, error) {
	f.calls, f.gotActor = f.calls+1, a
	return &domain.GuideCategory{ID: uuid.New()}, f.err
}

func (f *fakeEditor) ListCollections(_ context.Context, a uc.EditorActor, in uc.AdminListInput) ([]domain.GuideCollection, int, error) {
	f.calls, f.gotActor, f.gotAdminList = f.calls+1, a, in
	return nil, 0, f.err
}

func (f *fakeEditor) GetCollection(_ context.Context, a uc.EditorActor, _ uuid.UUID) (*domain.GuideCollectionAdminDetail, error) {
	f.calls, f.gotActor = f.calls+1, a
	if f.err != nil {
		return nil, f.err
	}
	return &domain.GuideCollectionAdminDetail{}, nil
}

func (f *fakeEditor) CreateCollection(_ context.Context, a uc.EditorActor, in uc.CollectionInput) (*domain.GuideCollection, error) {
	f.calls, f.gotActor, f.gotCollection = f.calls+1, a, in
	return &domain.GuideCollection{ID: uuid.New(), Kind: in.Kind}, f.err
}

func (f *fakeEditor) UpdateCollection(_ context.Context, a uc.EditorActor, _ uuid.UUID, in uc.CollectionInput) (*domain.GuideCollection, error) {
	f.calls, f.gotActor, f.gotCollection = f.calls+1, a, in
	return &domain.GuideCollection{ID: uuid.New(), Kind: in.Kind}, f.err
}

func (f *fakeEditor) Publish(_ context.Context, a uc.EditorActor, _ uuid.UUID, at *time.Time) (*domain.GuideCollection, error) {
	f.calls, f.gotActor = f.calls+1, a
	return &domain.GuideCollection{PublishedAt: at}, f.err
}

func (f *fakeEditor) Unpublish(_ context.Context, a uc.EditorActor, _ uuid.UUID) (*domain.GuideCollection, error) {
	f.calls, f.gotActor = f.calls+1, a
	return &domain.GuideCollection{}, f.err
}

func (f *fakeEditor) Archive(_ context.Context, a uc.EditorActor, _ uuid.UUID) (*domain.GuideCollection, error) {
	f.calls, f.gotActor = f.calls+1, a
	return &domain.GuideCollection{}, f.err
}

func (f *fakeEditor) SetCategories(_ context.Context, a uc.EditorActor, _ uuid.UUID, _ []uuid.UUID) error {
	f.calls, f.gotActor = f.calls+1, a
	return f.err
}

func (f *fakeEditor) AttachVenue(_ context.Context, a uc.EditorActor, col uuid.UUID, in uc.AttachVenueInput) error {
	f.calls, f.gotActor, f.gotColl, f.gotAttach = f.calls+1, a, col, in
	return f.err
}

func (f *fakeEditor) DetachVenue(_ context.Context, a uc.EditorActor, _, _ uuid.UUID) error {
	f.calls, f.gotActor = f.calls+1, a
	return f.err
}

func (f *fakeEditor) SetVenueNote(_ context.Context, a uc.EditorActor, _, _ uuid.UUID, _ string, _ domain.I18n) error {
	f.calls, f.gotActor = f.calls+1, a
	return f.err
}

func (f *fakeEditor) ReorderVenues(_ context.Context, a uc.EditorActor, col uuid.UUID, ids []uuid.UUID) error {
	f.calls, f.gotActor, f.gotColl, f.gotOrder = f.calls+1, a, col, ids
	return f.err
}

var _ uc.Editor = (*fakeEditor)(nil)

// --- auth plumbing: the router is built EXACTLY the way bootstrap/app.go mounts
// the editor — the real middleware.Auth followed by the real
// RequireRole(RoleAdmin) — so these tests cover the router gate itself and not
// only the handler. The fake issuer treats the access token as the user id and
// the fake user repository decides the role (same shape as the dashboard and
// myrestaurants handler tests).

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

func adminRouter(e uc.Editor, role domain.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: role}))
	adminGlobal := authed.Group("")
	adminGlobal.Use(middleware.RequireRole(domain.RoleAdmin))
	NewEditorHandler(e).RegisterAdminRoutes(adminGlobal)
	return r
}

func send(t *testing.T, r *gin.Engine, method, url string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, url, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+uuid.NewString())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// The reorder endpoint takes the intended FINAL sequence and hands it down in
// exactly that order — a handler that sorted or de-duplicated would hide the
// stale-client case the usecase is built to refuse.
func TestEditorHandler_ReorderPassesTheFinalOrderThrough(t *testing.T) {
	f := &fakeEditor{}
	r := adminRouter(f, domain.RoleAdmin)
	col := uuid.New()
	a, b, c := uuid.New(), uuid.New(), uuid.New()

	w := send(t, r, http.MethodPut, "/api/v1/admin/gastroguide/collections/"+col.String()+"/venues/order",
		map[string]any{"restaurant_ids": []string{c.String(), a.String(), b.String()}})
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %s, want 204", w.Code, w.Body.String())
	}
	if f.gotColl != col {
		t.Errorf("collection = %s, want %s", f.gotColl, col)
	}
	want := []uuid.UUID{c, a, b}
	if len(f.gotOrder) != 3 {
		t.Fatalf("order = %v, want 3 ids", f.gotOrder)
	}
	for i := range want {
		if f.gotOrder[i] != want[i] {
			t.Fatalf("order = %v, want %v", f.gotOrder, want)
		}
	}
}

// A refusal from the usecase reaches the client as the right status WITH the
// machine-readable code, so the panel can say "порядок устарел, обновите
// страницу" instead of "что-то пошло не так".
func TestEditorHandler_RefusalsCarryTheirCode(t *testing.T) {
	col := uuid.New()
	cases := []struct {
		name     string
		err      error
		method   string
		url      string
		body     any
		wantCode int
		wantErr  domain.ErrorCode
	}{
		{
			name:     "a non-superadmin is refused",
			err:      domain.ErrForbidden,
			method:   http.MethodPost,
			url:      "/api/v1/admin/gastroguide/collections",
			body:     map[string]any{"slug": "kids", "title": "С детьми"},
			wantCode: http.StatusForbidden,
			wantErr:  domain.CodeForbidden,
		},
		{
			name:     "a stale order",
			err:      domain.WithCode(domain.CodeGuideOrderMismatch, domain.ErrValidation),
			method:   http.MethodPut,
			url:      "/api/v1/admin/gastroguide/collections/" + col.String() + "/venues/order",
			body:     map[string]any{"restaurant_ids": []string{uuid.NewString()}},
			wantCode: http.StatusUnprocessableEntity,
			wantErr:  domain.CodeGuideOrderMismatch,
		},
		{
			name:     "a taken slug",
			err:      domain.WithCode(domain.CodeGuideSlugTaken, domain.ErrAlreadyExists),
			method:   http.MethodPost,
			url:      "/api/v1/admin/gastroguide/collections",
			body:     map[string]any{"slug": "kids", "title": "С детьми"},
			wantCode: http.StatusConflict,
			wantErr:  domain.CodeGuideSlugTaken,
		},
		{
			name:     "a venue already in the collection",
			err:      domain.WithCode(domain.CodeGuideVenueAlreadyAttached, domain.ErrAlreadyExists),
			method:   http.MethodPost,
			url:      "/api/v1/admin/gastroguide/collections/" + col.String() + "/venues",
			body:     map[string]any{"restaurant_id": uuid.NewString()},
			wantCode: http.StatusConflict,
			wantErr:  domain.CodeGuideVenueAlreadyAttached,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := adminRouter(&fakeEditor{err: tc.err}, domain.RoleAdmin)
			w := send(t, r, tc.method, tc.url, tc.body)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, body %s, want %d", w.Code, w.Body.String(), tc.wantCode)
			}
			var body struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %q: %v", w.Body.String(), err)
			}
			if body.Code != string(tc.wantErr) {
				t.Errorf("code = %q, want %q (body %s)", body.Code, tc.wantErr, w.Body.String())
			}
		})
	}
}

// The router gate itself: a caller who is not the global superadmin never
// reaches the usecase at all. This is the guarantee that matters — the guide is
// platform editorial content, and a restaurant owner who could reach these
// routes could put their own venue into "лучшие завтраки".
func TestEditorHandler_NonSuperadminNeverReachesTheUsecase(t *testing.T) {
	routes := []struct {
		method string
		url    string
		body   any
	}{
		{http.MethodGet, "/api/v1/admin/gastroguide/categories", nil},
		{http.MethodPost, "/api/v1/admin/gastroguide/categories", map[string]any{"slug": "b", "title": "Б"}},
		{http.MethodPut, "/api/v1/admin/gastroguide/categories/" + uuid.NewString(), map[string]any{"slug": "b", "title": "Б"}},
		{http.MethodGet, "/api/v1/admin/gastroguide/collections", nil},
		{http.MethodPost, "/api/v1/admin/gastroguide/collections", map[string]any{"slug": "k", "title": "К"}},
		{http.MethodGet, "/api/v1/admin/gastroguide/collections/" + uuid.NewString(), nil},
		{http.MethodPut, "/api/v1/admin/gastroguide/collections/" + uuid.NewString(), map[string]any{"slug": "k", "title": "К"}},
		{http.MethodPost, "/api/v1/admin/gastroguide/collections/" + uuid.NewString() + "/publish", nil},
		{http.MethodPost, "/api/v1/admin/gastroguide/collections/" + uuid.NewString() + "/unpublish", nil},
		{http.MethodPost, "/api/v1/admin/gastroguide/collections/" + uuid.NewString() + "/archive", nil},
		{http.MethodPut, "/api/v1/admin/gastroguide/collections/" + uuid.NewString() + "/categories", map[string]any{"category_ids": []string{}}},
		{http.MethodPost, "/api/v1/admin/gastroguide/collections/" + uuid.NewString() + "/venues", map[string]any{"restaurant_id": uuid.NewString()}},
		{http.MethodPut, "/api/v1/admin/gastroguide/collections/" + uuid.NewString() + "/venues/order", map[string]any{"restaurant_ids": []string{}}},
		{http.MethodPut, "/api/v1/admin/gastroguide/collections/" + uuid.NewString() + "/venues/" + uuid.NewString() + "/note", map[string]any{"note": "x"}},
		{http.MethodDelete, "/api/v1/admin/gastroguide/collections/" + uuid.NewString() + "/venues/" + uuid.NewString(), nil},
	}

	for _, role := range []domain.Role{domain.RoleUser, domain.RoleRestaurant, domain.Role("hostess")} {
		for _, rt := range routes {
			f := &fakeEditor{}
			w := send(t, adminRouter(f, role), rt.method, rt.url, rt.body)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s: status = %d, body %s, want 403", rt.method, rt.url, role, w.Code, w.Body.String())
			}
			if f.calls != 0 {
				t.Errorf("%s %s as %s: the usecase was reached anyway", rt.method, rt.url, role)
			}
		}
	}

	// …and a superadmin does get through, so the test above is not passing for
	// the wrong reason (a mistyped path answering 404, say).
	f := &fakeEditor{}
	if w := send(t, adminRouter(f, domain.RoleAdmin), http.MethodGet, "/api/v1/admin/gastroguide/categories", nil); w.Code != http.StatusOK {
		t.Fatalf("superadmin: status = %d, body %s, want 200", w.Code, w.Body.String())
	}
	if f.gotActor.Role != domain.RoleAdmin {
		t.Errorf("role = %q, want admin", f.gotActor.Role)
	}
}

// Publishing with an empty body means "now"; only a client asking for a
// scheduled publication sends published_at. An empty body must not be a 400.
func TestEditorHandler_PublishAcceptsAnEmptyBody(t *testing.T) {
	f := &fakeEditor{}
	r := adminRouter(f, domain.RoleAdmin)
	url := "/api/v1/admin/gastroguide/collections/" + uuid.NewString() + "/publish"
	if w := send(t, r, http.MethodPost, url, nil); w.Code != http.StatusOK {
		t.Fatalf("empty body: status = %d, body %s, want 200", w.Code, w.Body.String())
	}
	if f.calls != 1 {
		t.Errorf("usecase called %d times, want 1", f.calls)
	}
}

// Подсветка блока в этих тестах не проверяется — фейку нужен метод только
// затем, чтобы удовлетворить интерфейс.
func (f *fakeEditor) SetVenueHighlight(_ context.Context, _ uc.EditorActor, _, _ uuid.UUID, _, _ *uuid.UUID) error {
	return nil
}

// --- articles vs collections (migration 0096) ---

// A panel build older than the split posts no `kind` field at all. The handler
// must pass an EMPTY kind down (the usecase defaults it to "collection"), not
// invent some third value — and the create must still succeed.
func TestEditorHandler_OmittedKindIsPassedThroughEmpty(t *testing.T) {
	f := &fakeEditor{}
	r := adminRouter(f, domain.RoleAdmin)

	w := send(t, r, http.MethodPost, "/api/v1/admin/gastroguide/collections",
		map[string]any{"slug": "kids", "title": "С детьми"})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s, want 201", w.Code, w.Body.String())
	}
	if f.gotCollection.Kind != "" {
		t.Fatalf("kind = %q, want empty (the usecase owns the default)", f.gotCollection.Kind)
	}
}

// An explicit kind reaches the usecase normalized (trimmed, lowercased), so a
// panel that posts "Article" is not refused for a cosmetic difference.
func TestEditorHandler_KindIsNormalizedOnTheWayIn(t *testing.T) {
	for _, raw := range []string{"article", " Article ", "ARTICLE"} {
		f := &fakeEditor{}
		r := adminRouter(f, domain.RoleAdmin)

		w := send(t, r, http.MethodPost, "/api/v1/admin/gastroguide/collections",
			map[string]any{"slug": "chto-proishodit", "title": "Что происходит", "kind": raw})
		if w.Code != http.StatusCreated {
			t.Fatalf("%q: status = %d, body %s", raw, w.Code, w.Body.String())
		}
		if f.gotCollection.Kind != domain.GuideKindArticle {
			t.Fatalf("%q: kind = %q, want %q", raw, f.gotCollection.Kind, domain.GuideKindArticle)
		}
	}
}

// The kind is echoed back on the admin response, so the panel can render the
// row without guessing from the rubric list.
func TestEditorHandler_ResponseCarriesKind(t *testing.T) {
	f := &fakeEditor{}
	r := adminRouter(f, domain.RoleAdmin)

	w := send(t, r, http.MethodPost, "/api/v1/admin/gastroguide/collections",
		map[string]any{"slug": "chto-proishodit", "title": "Что происходит", "kind": "article"})
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	if got := body["data"].(map[string]any)["kind"]; got != "article" {
		t.Fatalf("kind = %v, want article", got)
	}
}

// ?kind= narrows the cabinet listing; absent means both kinds.
func TestEditorHandler_ListKindFilter(t *testing.T) {
	f := &fakeEditor{}
	r := adminRouter(f, domain.RoleAdmin)

	if w := send(t, r, http.MethodGet, "/api/v1/admin/gastroguide/collections?kind=article", nil); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if f.gotAdminList.Kind == nil || *f.gotAdminList.Kind != domain.GuideKindArticle {
		t.Fatalf("kind = %v, want article", f.gotAdminList.Kind)
	}

	f2 := &fakeEditor{}
	r2 := adminRouter(f2, domain.RoleAdmin)
	if w := send(t, r2, http.MethodGet, "/api/v1/admin/gastroguide/collections", nil); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if f2.gotAdminList.Kind != nil {
		t.Fatalf("kind = %v, want nil (no filter)", *f2.gotAdminList.Kind)
	}
}

// The two refusals the split introduces arrive at the client as 422 with their
// own machine-readable codes, not as a bare validation failure the panel cannot
// explain.
func TestEditorHandler_KindRefusalsAre422WithTheirCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code domain.ErrorCode
	}{
		{"unknown kind",
			domain.WithCode(domain.CodeGuideUnknownKind,
				fmt.Errorf("%w: unknown collection kind", domain.ErrValidation)),
			domain.CodeGuideUnknownKind},
		{"article with rubrics",
			domain.WithCode(domain.CodeGuideArticleHasRubrics,
				fmt.Errorf("%w: an article carries no rubrics", domain.ErrValidation)),
			domain.CodeGuideArticleHasRubrics},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEditor{err: tc.err}
			r := adminRouter(f, domain.RoleAdmin)

			w := send(t, r, http.MethodPut,
				"/api/v1/admin/gastroguide/collections/"+uuid.NewString(),
				map[string]any{"slug": "kids", "title": "С детьми", "kind": "article"})
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, body %s, want 422", w.Code, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %q: %v", w.Body.String(), err)
			}
			if body["code"] != string(tc.code) {
				t.Fatalf("code = %v, want %s", body["code"], tc.code)
			}
		})
	}
}
