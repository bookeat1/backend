package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/auth/initdata"
	"backend-core/internal/auth/password"
	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/token"
	"backend-core/internal/infrastructure/token/tokentest"
	uc "backend-core/internal/usecase/auth"
)

// The usecase tests fix WHICH refusal each situation produces. This file fixes
// what a client on the wire actually receives — the status and the
// machine-readable code the mini app branches on to choose a screen.

const (
	miniBot        = "7654321:AAH-our-restaurants-bot"
	otherBot       = "1234567:AAF-somebody-elses-bot"
	tgUser   int64 = 4242
)

// --- minimal in-memory ports ------------------------------------------------

type memUsers struct{ byID map[uuid.UUID]*domain.User }

func (m *memUsers) Create(_ context.Context, u *domain.User) error { m.byID[u.ID] = u; return nil }
func (m *memUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if u, ok := m.byID[id]; ok {
		return u, nil
	}
	return nil, domain.ErrNotFound
}
func (m *memUsers) GetByEmail(_ context.Context, email string) (*domain.User, error) {
	for _, u := range m.byID {
		if u.Email != nil && *u.Email == email {
			return u, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (m *memUsers) GetByPhone(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (m *memUsers) Update(_ context.Context, u *domain.User) error { m.byID[u.ID] = u; return nil }
func (m *memUsers) Delete(context.Context, uuid.UUID) error        { return nil }

type memCreds struct{ byUser map[uuid.UUID]string }

func (m *memCreds) Upsert(_ context.Context, c *domain.UserCredential) error {
	m.byUser[c.UserID] = c.PasswordHash
	return nil
}
func (m *memCreds) GetByUserID(_ context.Context, id uuid.UUID) (*domain.UserCredential, error) {
	if h, ok := m.byUser[id]; ok {
		return &domain.UserCredential{UserID: id, PasswordHash: h}, nil
	}
	return nil, domain.ErrNotFound
}

type memRefresh struct{ rows []*domain.RefreshToken }

func (m *memRefresh) Create(_ context.Context, t *domain.RefreshToken) error {
	m.rows = append(m.rows, t)
	return nil
}
func (m *memRefresh) GetByHash(context.Context, string) (*domain.RefreshToken, error) {
	return nil, domain.ErrNotFound
}
func (m *memRefresh) Revoke(context.Context, uuid.UUID) error          { return nil }
func (m *memRefresh) RevokeAllByUser(context.Context, uuid.UUID) error { return nil }

type memLinks struct {
	rows map[int64]*domain.TelegramStaffLink
}

func (m *memLinks) GetByTelegramUserID(_ context.Context, id int64) (*domain.TelegramStaffLink, error) {
	if l, ok := m.rows[id]; ok {
		return l, nil
	}
	return nil, domain.ErrNotFound
}
func (m *memLinks) Upsert(_ context.Context, l *domain.TelegramStaffLink) error {
	l.RevokedAt = nil
	m.rows[l.TelegramUserID] = l
	return nil
}
func (m *memLinks) Revoke(_ context.Context, id int64) error {
	if l, ok := m.rows[id]; ok {
		now := time.Now()
		l.RevokedAt = &now
	}
	return nil
}
func (m *memLinks) RevokeByUser(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (m *memLinks) TouchLastSeen(context.Context, int64) error           { return nil }

type memVenues struct{ byUser map[uuid.UUID][]uc.StaffVenue }

func (m *memVenues) ListForStaff(_ context.Context, id uuid.UUID, _ domain.Role) ([]uc.StaffVenue, error) {
	return m.byUser[id], nil
}

type memTx struct{}

func (memTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
func (memTx) Detach(ctx context.Context) context.Context                         { return ctx }

// --- harness ----------------------------------------------------------------

type miniRig struct {
	router *gin.Engine
	links  *memLinks
	user   *domain.User
}

func newMiniRig(t *testing.T, botToken string) *miniRig {
	t.Helper()
	gin.SetMode(gin.TestMode)

	users := &memUsers{byID: map[uuid.UUID]*domain.User{}}
	creds := &memCreds{byUser: map[uuid.UUID]string{}}
	links := &memLinks{rows: map[int64]*domain.TelegramStaffLink{}}
	venues := &memVenues{byUser: map[uuid.UUID][]uc.StaffVenue{}}

	email := "hostess@delpapa.kz"
	u := &domain.User{ID: uuid.New(), Email: &email, FullName: "Аня", Role: domain.RoleUser, IsActive: true}
	users.byID[u.ID] = u
	hash, err := password.Hash("s3cret123")
	if err != nil {
		t.Fatal(err)
	}
	creds.byUser[u.ID] = hash
	venues.byUser[u.ID] = []uc.StaffVenue{{
		RestaurantID: uuid.New(),
		Name:         "Del Papa",
		NameI18n:     domain.I18n{"ru": "Дель Папа"},
		Role:         "hostess",
	}}

	iss, err := token.NewRSAIssuer(tokentest.GenerateKeyPEM(t), "kid", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mini := uc.NewMiniAppUseCase(
		links, users, creds, &memRefresh{}, venues, memTx{}, iss,
		uc.Config{RefreshTTL: time.Hour},
		uc.MiniAppConfig{BotToken: botToken, InitDataTTL: time.Hour},
		nil,
	)

	r := gin.New()
	NewTelegramHandler(mini).RegisterRoutes(r.Group("/api/v1"))
	return &miniRig{router: r, links: links, user: u}
}

func signedBlob(tgID int64, botToken string, authDate time.Time) string {
	v := url.Values{}
	v.Set("user", `{"id":`+strconv.FormatInt(tgID, 10)+`,"first_name":"Аня"}`)
	v.Set("auth_date", strconv.FormatInt(authDate.Unix(), 10))
	v.Set("hash", initdata.Sign(v, botToken))
	return v.Encode()
}

// jsonQuote encodes s as a JSON string so the initData blob's & and = survive
// being embedded in a request body.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (rg *miniRig) post(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rg.router.ServeHTTP(w, req)
	return w
}

func assertWire(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d — body: %s", w.Code, status, w.Body.String())
	}
	if code == "" {
		return
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not an envelope: %s", w.Body.String())
	}
	if env.Code != code {
		t.Fatalf("code = %q, want %q — body: %s", env.Code, code, w.Body.String())
	}
}

// --- the wire ---------------------------------------------------------------

// Criterion 5: no bot token → all three routes report themselves absent.
func TestMiniAppRoutesAre404WithoutABotToken(t *testing.T) {
	rig := newMiniRig(t, "")
	body := `{"init_data":` + jsonQuote(signedBlob(tgUser, miniBot, time.Now())) + `}`
	for _, path := range []string{"/api/v1/auth/telegram/miniapp", "/api/v1/auth/telegram/link", "/api/v1/auth/telegram/unlink"} {
		assertWire(t, rig.post(t, path, body), http.StatusNotFound, "")
	}
}

// Criterion 1: an edited blob is a 401 that names the signature.
func TestSignInWithAForgedBlobIs401(t *testing.T) {
	rig := newMiniRig(t, miniBot)
	v, _ := url.ParseQuery(signedBlob(tgUser, miniBot, time.Now()))
	v.Set("user", `{"id":9999,"first_name":"Чужой"}`)
	body := `{"init_data":` + jsonQuote(v.Encode()) + `}`
	assertWire(t, rig.post(t, "/api/v1/auth/telegram/miniapp", body), http.StatusUnauthorized, "init_data_invalid")
}

// Criterion 2: another bot's genuine signature is a 401 too.
func TestSignInWithAnotherBotsBlobIs401(t *testing.T) {
	rig := newMiniRig(t, miniBot)
	body := `{"init_data":` + jsonQuote(signedBlob(tgUser, otherBot, time.Now())) + `}`
	assertWire(t, rig.post(t, "/api/v1/auth/telegram/miniapp", body), http.StatusUnauthorized, "init_data_invalid")
}

// Criterion 3: a stale blob gets its own code, so the app says "reopen from the
// bot" instead of showing a password form that cannot help.
func TestSignInWithAStaleBlobIs401Expired(t *testing.T) {
	rig := newMiniRig(t, miniBot)
	body := `{"init_data":` + jsonQuote(signedBlob(tgUser, miniBot, time.Now().Add(-4*time.Hour))) + `}`
	assertWire(t, rig.post(t, "/api/v1/auth/telegram/miniapp", body), http.StatusUnauthorized, "init_data_expired")
}

// Criterion 6: first open → 403 link_required, the only code that draws the form.
func TestSignInWithoutALinkIs403LinkRequired(t *testing.T) {
	rig := newMiniRig(t, miniBot)
	body := `{"init_data":` + jsonQuote(signedBlob(tgUser, miniBot, time.Now())) + `}`
	assertWire(t, rig.post(t, "/api/v1/auth/telegram/miniapp", body), http.StatusForbidden, "link_required")
}

// Criterion 7 + 8 on the wire: the password sign-in returns the session shape,
// and the next open returns the same shape with no password at all.
func TestLinkThenSignInReturnTheSameSessionShape(t *testing.T) {
	rig := newMiniRig(t, miniBot)
	blob := signedBlob(tgUser, miniBot, time.Now())

	w := rig.post(t, "/api/v1/auth/telegram/link",
		`{"init_data":`+jsonQuote(blob)+`,"email":"hostess@delpapa.kz","password":"s3cret123"}`)
	assertWire(t, w, http.StatusOK, "")

	var out struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			User         struct {
				ID       string `json:"id"`
				FullName string `json:"full_name"`
			} `json:"user"`
			Restaurants []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Role string `json:"role"`
			} `json:"restaurants"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unparsable body: %s", w.Body.String())
	}
	if out.Data.AccessToken == "" || out.Data.RefreshToken == "" {
		t.Fatalf("no token pair in the body: %s", w.Body.String())
	}
	if out.Data.User.ID != rig.user.ID.String() {
		t.Fatalf("user id = %q, want %q", out.Data.User.ID, rig.user.ID)
	}
	if len(out.Data.Restaurants) != 1 || out.Data.Restaurants[0].Role != "hostess" {
		t.Fatalf("restaurants = %+v", out.Data.Restaurants)
	}
	// No Accept-Language was sent, so the base name comes back untouched —
	// the same rule every other i18n endpoint follows.
	if out.Data.Restaurants[0].Name != "Del Papa" {
		t.Fatalf("name = %q, want the base name when no language was asked for", out.Data.Restaurants[0].Name)
	}

	assertWire(t, rig.post(t, "/api/v1/auth/telegram/miniapp",
		`{"init_data":`+jsonQuote(blob)+`}`), http.StatusOK, "")
}

func TestLinkWithABadPasswordIs401InvalidCredentials(t *testing.T) {
	rig := newMiniRig(t, miniBot)
	body := `{"init_data":` + jsonQuote(signedBlob(tgUser, miniBot, time.Now())) + `,"email":"hostess@delpapa.kz","password":"nope"}`
	assertWire(t, rig.post(t, "/api/v1/auth/telegram/link", body), http.StatusUnauthorized, "invalid_credentials")
	if len(rig.links.rows) != 0 {
		t.Fatal("a failed sign-in created a link")
	}
}

// A malformed body must not echo the password back in a validation message.
func TestLinkWithAMissingFieldIs422AndSaysNothingAboutTheValues(t *testing.T) {
	rig := newMiniRig(t, miniBot)
	body := `{"init_data":` + jsonQuote(signedBlob(tgUser, miniBot, time.Now())) + `,"email":"hostess@delpapa.kz"}`
	w := rig.post(t, "/api/v1/auth/telegram/link", body)
	assertWire(t, w, http.StatusUnprocessableEntity, "validation_failed")
	if got := w.Body.String(); strings.Contains(got, "s3cret123") || strings.Contains(got, "hostess@delpapa.kz") {
		t.Fatalf("the 422 body quotes the submitted values: %s", got)
	}
}

// Criterion 11 on the wire: sign-out is a 204 and the link stops working.
func TestUnlinkIs204AndTheNextOpenAsksForTheForm(t *testing.T) {
	rig := newMiniRig(t, miniBot)
	blob := signedBlob(tgUser, miniBot, time.Now())
	if w := rig.post(t, "/api/v1/auth/telegram/link",
		`{"init_data":`+jsonQuote(blob)+`,"email":"hostess@delpapa.kz","password":"s3cret123"}`); w.Code != http.StatusOK {
		t.Fatalf("setup link failed: %s", w.Body.String())
	}

	assertWire(t, rig.post(t, "/api/v1/auth/telegram/unlink",
		`{"init_data":`+jsonQuote(blob)+`}`), http.StatusNoContent, "")

	assertWire(t, rig.post(t, "/api/v1/auth/telegram/miniapp",
		`{"init_data":`+jsonQuote(blob)+`}`), http.StatusForbidden, "link_required")
}

// No initData and no bearer: there is nothing to identify a session to end.
func TestUnlinkWithNothingIs422(t *testing.T) {
	rig := newMiniRig(t, miniBot)
	assertWire(t, rig.post(t, "/api/v1/auth/telegram/unlink", `{}`), http.StatusUnprocessableEntity, "validation_failed")
}

func TestSignInWithNoInitDataIs422(t *testing.T) {
	rig := newMiniRig(t, miniBot)
	assertWire(t, rig.post(t, "/api/v1/auth/telegram/miniapp", `{}`), http.StatusUnprocessableEntity, "validation_failed")
}

// The venue name is localized per request, like every other i18n endpoint: the
// mini app must not be the one screen that shows an untranslated name.
func TestSignInLocalizesTheVenueName(t *testing.T) {
	rig := newMiniRig(t, miniBot)
	blob := signedBlob(tgUser, miniBot, time.Now())
	if w := rig.post(t, "/api/v1/auth/telegram/link",
		`{"init_data":`+jsonQuote(blob)+`,"email":"hostess@delpapa.kz","password":"s3cret123"}`); w.Code != http.StatusOK {
		t.Fatalf("setup link failed: %s", w.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/telegram/miniapp",
		strings.NewReader(`{"init_data":`+jsonQuote(blob)+`}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Language", "ru")
	w := httptest.NewRecorder()
	rig.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	var out struct {
		Data struct {
			Restaurants []struct {
				Name string `json:"name"`
			} `json:"restaurants"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data.Restaurants) != 1 || out.Data.Restaurants[0].Name != "Дель Папа" {
		t.Fatalf("name = %+v, want the ru i18n value", out.Data.Restaurants)
	}
}
