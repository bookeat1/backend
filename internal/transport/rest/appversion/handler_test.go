package appversion

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
	uc "backend-core/internal/usecase/appversion"
)

// --- fake usecase -----------------------------------------------------------

type fakeUC struct {
	decision   uc.Decision
	checkErr   error
	gotVersion string
	gotPlat    domain.DevicePlatform
	gotActor   uc.Actor
	gotInput   uc.SaveInput
	saved      int
	err        error
}

func (f *fakeUC) Check(_ context.Context, p domain.DevicePlatform, v string) (uc.Decision, error) {
	f.gotPlat, f.gotVersion = p, v
	if f.checkErr != nil {
		return uc.Decision{}, f.checkErr
	}
	d := f.decision
	d.Platform = p
	return d, nil
}

func (f *fakeUC) List(_ context.Context, a uc.Actor) ([]domain.MobileAppPolicy, error) {
	f.gotActor = a
	if f.err != nil {
		return nil, f.err
	}
	return []domain.MobileAppPolicy{
		{Platform: domain.PlatformAndroid, MinSupportedVersion: "1.2", StoreURL: "https://play.google.com/x"},
		{Platform: domain.PlatformIOS, MinSupportedVersion: "1.5", StoreURL: "https://apps.apple.com/x",
			RequiredTitleI18n: domain.I18n{"ru": "Нужно обновить", "en": "Update required"},
			UpdatedAt:         time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)},
	}, nil
}

func (f *fakeUC) Save(_ context.Context, a uc.Actor, p domain.DevicePlatform, in uc.SaveInput) (*domain.MobileAppPolicy, error) {
	f.gotActor, f.gotPlat, f.gotInput = a, p, in
	f.saved++
	if f.err != nil {
		return nil, f.err
	}
	out := domain.MobileAppPolicy{Platform: p, StoreURL: "https://apps.apple.com/x"}
	if in.MinSupportedVersion != nil {
		out.MinSupportedVersion = *in.MinSupportedVersion
	}
	return &out, nil
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

func decodeData(t *testing.T, w *httptest.ResponseRecorder, into any) {
	t.Helper()
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
		Code  string          `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", w.Body.String(), err)
	}
	if err := json.Unmarshal(env.Data, into); err != nil {
		t.Fatalf("decode data %q: %v", string(env.Data), err)
	}
}

// --- public check -----------------------------------------------------------

// TestCheckIsPublicAndCarriesTheWholeAnswer. No token, one round trip, and
// everything the app needs to draw the screen: the mode, where to send the
// guest, and the wording in EVERY supported language. The loop below reads
// domain.SupportedLocales rather than a fixed list, so adding a locale to the
// domain makes this test demand wording for it — which is how ko and zh got
// here in the first place.
func TestCheckIsPublicAndCarriesTheWholeAnswer(t *testing.T) {
	f := &fakeUC{decision: uc.Decision{
		Action:              domain.AppUpdateRequired,
		StoreURL:            "https://apps.apple.com/app/id6757542577",
		MinSupportedVersion: "1.6",
		Title: domain.I18n{"ru": "Нужно обновить", "kk": "Жаңарту қажет", "en": "Update required",
			"ko": "업데이트가 필요합니다", "zh": "需要更新"},
		Message: domain.I18n{"ru": "Обновите", "kk": "Жаңартыңыз", "en": "Please update",
			"ko": "앱을 업데이트해 주세요", "zh": "请更新应用"},
	}}
	w := send(t, router(f, domain.RoleUser), http.MethodGet, "/api/v1/app/version-check?platform=ios&version=1.5.1", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (this route must work with no token at all): %s", w.Code, w.Body)
	}
	if f.gotPlat != domain.PlatformIOS || f.gotVersion != "1.5.1" {
		t.Errorf("handler passed platform=%q version=%q", f.gotPlat, f.gotVersion)
	}

	var got checkResponse
	decodeData(t, w, &got)
	if got.Action != string(domain.AppUpdateRequired) {
		t.Errorf("action = %q", got.Action)
	}
	if got.Platform != "ios" {
		t.Errorf("platform = %q", got.Platform)
	}
	if got.StoreURL == "" {
		t.Error("no store_url: the Update button would have nowhere to go")
	}
	for _, l := range domain.SupportedLocales {
		if got.Title[l] == "" || got.Message[l] == "" {
			t.Errorf("no %s wording: title=%v message=%v", l, got.Title, got.Message)
		}
	}
}

// TestCheckAcceptsPlatformCaseAndTrailingSpace: a client that sends "iOS" or
// "Android" (Platform.OS spelled by hand) must not be refused.
func TestCheckAcceptsPlatformCase(t *testing.T) {
	for _, q := range []string{"iOS", "ANDROID", "Android", "ios"} {
		f := &fakeUC{}
		w := send(t, router(f, domain.RoleUser), http.MethodGet,
			"/api/v1/app/version-check?platform="+q+"&version=1.5", nil, false)
		if w.Code != http.StatusOK {
			t.Errorf("platform=%q → status %d, want 200", q, w.Code)
		}
	}
}

// TestCheckRefusesAnUnknownPlatform. The ONE refusal on this route: a missing
// or unknown platform is a client bug, and answering "none" would hide it
// forever. Everything else doubtful answers 200/none instead.
func TestCheckRefusesAnUnknownPlatform(t *testing.T) {
	for _, q := range []string{"", "platform=", "platform=web", "platform=windows", "platform=ios,android"} {
		f := &fakeUC{}
		w := send(t, router(f, domain.RoleUser), http.MethodGet, "/api/v1/app/version-check?"+q, nil, false)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("query %q → status %d, want 422", q, w.Code)
			continue
		}
		var env struct{ Code string }
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if env.Code != string(domain.CodeAppPlatformUnknown) {
			t.Errorf("query %q → code %q, want %q", q, env.Code, domain.CodeAppPlatformUnknown)
		}
	}
}

// TestCheckWithoutAVersionAnswersNone. The app may not know its own version
// (a bad build config, a web preview): that is not a reason to refuse, and not
// a reason to force an update either.
func TestCheckWithoutAVersionAnswersNone(t *testing.T) {
	f := &fakeUC{decision: uc.Decision{Action: domain.AppUpdateNone, StoreURL: "https://apps.apple.com/x"}}
	w := send(t, router(f, domain.RoleUser), http.MethodGet, "/api/v1/app/version-check?platform=ios", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if f.gotVersion != "" {
		t.Errorf("handler invented a version %q", f.gotVersion)
	}
	var got checkResponse
	decodeData(t, w, &got)
	if got.Action != string(domain.AppUpdateNone) {
		t.Errorf("action = %q, want none", got.Action)
	}
	if got.Title != nil || got.Message != nil {
		t.Errorf("action=none must carry no wording, got %v / %v", got.Title, got.Message)
	}
}

// TestCheckTruncatesAnAbsurdVersion rather than passing an unbounded string
// from an unauthenticated caller further down.
func TestCheckTruncatesAnAbsurdVersion(t *testing.T) {
	long := ""
	for i := 0; i < 5000; i++ {
		long += "9"
	}
	f := &fakeUC{}
	w := send(t, router(f, domain.RoleUser), http.MethodGet,
		"/api/v1/app/version-check?platform=android&version="+long, nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(f.gotVersion) > versionQueryMaxLen {
		t.Errorf("handler passed %d characters down, want at most %d", len(f.gotVersion), versionQueryMaxLen)
	}
}

// TestCheckIsCacheable. The answer is a pure function of the query string, so
// it may be cached by URL — that is what keeps a cold-start stampede off the
// database. A Vary-free, header-independent answer is a REQUIREMENT here: if
// the wording were resolved from Accept-Language instead of shipped in all
// three languages, this header would be a cache poisoning bug.
func TestCheckIsCacheable(t *testing.T) {
	f := &fakeUC{decision: uc.Decision{Action: domain.AppUpdateNone}}
	w := send(t, router(f, domain.RoleUser), http.MethodGet, "/api/v1/app/version-check?platform=ios&version=2.0", nil, false)
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Errorf("Cache-Control = %q, want public, max-age=300", got)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("no ETag: a client cannot revalidate cheaply")
	}
	if v := w.Header().Get("Vary"); v != "" {
		t.Errorf("Vary = %q: the answer must not depend on any request header", v)
	}
}

// TestCheckRevalidatesWithIfNoneMatch: the repeat call every launch costs 304
// and no body.
func TestCheckRevalidatesWithIfNoneMatch(t *testing.T) {
	f := &fakeUC{decision: uc.Decision{Action: domain.AppUpdateRecommended,
		StoreURL: "https://apps.apple.com/x", Title: domain.I18n{"ru": "Обновление"}}}
	r := router(f, domain.RoleUser)
	first := send(t, r, http.MethodGet, "/api/v1/app/version-check?platform=ios&version=1.5", nil, false)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the first answer")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/app/version-check?platform=ios&version=1.5", nil)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	r.ServeHTTP(second, req)
	if second.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Errorf("304 carried a body: %s", second.Body)
	}
}

// TestTwoBuildsDoNotShareAnETag. The answer depends on the CALLER's version, so
// two clients on different builds must get different validators — otherwise a
// shared cache would hand one build the other's verdict.
func TestTwoBuildsDoNotShareAnETag(t *testing.T) {
	r := router(&fakeUC{decision: uc.Decision{Action: domain.AppUpdateRequired,
		StoreURL: "https://apps.apple.com/x", Title: domain.I18n{"ru": "Обновите"}}}, domain.RoleUser)
	old := send(t, r, http.MethodGet, "/api/v1/app/version-check?platform=ios&version=1.0", nil, false)

	quiet := router(&fakeUC{decision: uc.Decision{Action: domain.AppUpdateNone}}, domain.RoleUser)
	fresh := send(t, quiet, http.MethodGet, "/api/v1/app/version-check?platform=ios&version=2.0", nil, false)

	if old.Header().Get("ETag") == fresh.Header().Get("ETag") {
		t.Error("two different verdicts share one ETag")
	}
}

// TestCheckSurfacesADatabaseFailure rather than inventing a policy it could not
// read. The app treats any non-200 as "do nothing".
func TestCheckSurfacesADatabaseFailure(t *testing.T) {
	f := &fakeUC{checkErr: fmt.Errorf("connection refused")}
	w := send(t, router(f, domain.RoleUser), http.MethodGet, "/api/v1/app/version-check?platform=ios&version=1.0", nil, false)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

// --- admin routes -----------------------------------------------------------

// TestAdminRoutesNeedSuperadmin: the router gate, not only the usecase.
func TestAdminRoutesNeedSuperadmin(t *testing.T) {
	cases := []struct {
		name   string
		role   domain.Role
		authed bool
		want   int
	}{
		{"anonymous", domain.RoleUser, false, http.StatusUnauthorized},
		{"guest", domain.RoleUser, true, http.StatusForbidden},
		{"venue staff", domain.RoleRestaurant, true, http.StatusForbidden},
		{"superadmin", domain.RoleAdmin, true, http.StatusOK},
	}
	for _, tc := range cases {
		f := &fakeUC{}
		r := router(f, tc.role)
		if w := send(t, r, http.MethodGet, "/api/v1/admin/app-update-policies", nil, tc.authed); w.Code != tc.want {
			t.Errorf("%s GET → %d, want %d: %s", tc.name, w.Code, tc.want, w.Body)
		}
		w := send(t, r, http.MethodPut, "/api/v1/admin/app-update-policies/ios",
			map[string]any{"min_supported_version": "1.6"}, tc.authed)
		if w.Code != tc.want {
			t.Errorf("%s PUT → %d, want %d: %s", tc.name, w.Code, tc.want, w.Body)
		}
		if tc.want != http.StatusOK && f.saved != 0 {
			t.Errorf("%s reached the usecase %d times", tc.name, f.saved)
		}
	}
}

// TestAdminListShowsWhatIsStored, untranslated and unresolved: an editor has to
// see the raw row, both thresholds and every language.
func TestAdminListShowsWhatIsStored(t *testing.T) {
	f := &fakeUC{}
	w := send(t, router(f, domain.RoleAdmin), http.MethodGet, "/api/v1/admin/app-update-policies", nil, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var got []policyResponse
	decodeData(t, w, &got)
	if len(got) != 2 {
		t.Fatalf("got %d policies, want both platforms", len(got))
	}
	if got[0].Platform != "android" || got[1].Platform != "ios" {
		t.Errorf("platforms = %q, %q", got[0].Platform, got[1].Platform)
	}
	if got[1].RequiredTitleI18n["en"] != "Update required" {
		t.Errorf("translations are not shown to the editor: %v", got[1].RequiredTitleI18n)
	}
	if got[1].UpdatedAt == "" {
		t.Error("no updated_at: an editor cannot tell when the policy last changed")
	}
	if f.gotActor.Role != domain.RoleAdmin {
		t.Errorf("actor role reaching the usecase = %q", f.gotActor.Role)
	}
}

// TestAdminSaveIsAPatch: a field the panel did not send arrives as nil, so the
// usecase can preserve it. A body that turned absent fields into "" would be
// the full-replace wipe this codebase has been bitten by before.
func TestAdminSaveIsAPatch(t *testing.T) {
	f := &fakeUC{}
	w := send(t, router(f, domain.RoleAdmin), http.MethodPut, "/api/v1/admin/app-update-policies/android",
		map[string]any{"min_supported_version": "1.6"}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if f.gotPlat != domain.PlatformAndroid {
		t.Errorf("platform = %q", f.gotPlat)
	}
	in := f.gotInput
	if in.MinSupportedVersion == nil || *in.MinSupportedVersion != "1.6" {
		t.Errorf("min_supported_version did not arrive: %v", in.MinSupportedVersion)
	}
	for name, ptr := range map[string]*string{
		"min_recommended_version": in.MinRecommendedVersion,
		"store_url":               in.StoreURL,
		"required_title":          in.RequiredTitle,
		"required_message":        in.RequiredMessage,
		"recommended_title":       in.RecommendedTitle,
		"recommended_message":     in.RecommendedMessage,
	} {
		if ptr != nil {
			t.Errorf("%s was absent from the body but arrived as %q — that is a wipe", name, *ptr)
		}
	}
}

// TestAdminSaveCarriesTheOffSwitch: an EXPLICIT empty string must be
// distinguishable from an absent field, or a forced update could never be
// turned off through this endpoint.
func TestAdminSaveCarriesTheOffSwitch(t *testing.T) {
	f := &fakeUC{}
	w := send(t, router(f, domain.RoleAdmin), http.MethodPut, "/api/v1/admin/app-update-policies/ios",
		map[string]any{"min_supported_version": ""}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if f.gotInput.MinSupportedVersion == nil {
		t.Fatal(`"min_supported_version": "" arrived as absent; the off switch is unreachable`)
	}
	if *f.gotInput.MinSupportedVersion != "" {
		t.Errorf("min_supported_version = %q, want empty", *f.gotInput.MinSupportedVersion)
	}
}

// TestAdminSaveCarriesATranslationDeletion: null in an *_i18n object means
// "remove this language", which the pointer map preserves.
func TestAdminSaveCarriesATranslationDeletion(t *testing.T) {
	f := &fakeUC{}
	body := map[string]any{"required_title_i18n": map[string]any{"kk": nil, "en": "Update required"}}
	w := send(t, router(f, domain.RoleAdmin), http.MethodPut, "/api/v1/admin/app-update-policies/ios", body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	patch := f.gotInput.RequiredTitleI18n
	kk, ok := patch["kk"]
	if !ok {
		t.Fatal("the kk key was dropped; a deletion cannot be expressed")
	}
	if kk != nil {
		t.Errorf("kk = %q, want a null (deletion)", *kk)
	}
	if patch["en"] == nil || *patch["en"] != "Update required" {
		t.Errorf("en did not survive: %v", patch["en"])
	}
}

// TestAdminSaveRefusesAnUnknownPlatformInThePath.
func TestAdminSaveRefusesAnUnknownPlatformInThePath(t *testing.T) {
	f := &fakeUC{}
	w := send(t, router(f, domain.RoleAdmin), http.MethodPut, "/api/v1/admin/app-update-policies/web",
		map[string]any{"min_supported_version": "1.6"}, true)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", w.Code, w.Body)
	}
	if f.saved != 0 {
		t.Error("an unknown platform reached the usecase")
	}
}

// TestAdminSaveRejectsAMalformedBody with 400, before any usecase call.
func TestAdminSaveRejectsAMalformedBody(t *testing.T) {
	f := &fakeUC{}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/app-update-policies/ios",
		bytes.NewReader([]byte(`{"min_supported_version": 1.6}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+uuid.NewString())
	w := httptest.NewRecorder()
	router(f, domain.RoleAdmin).ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
	if f.saved != 0 {
		t.Error("a malformed body reached the usecase")
	}
}
