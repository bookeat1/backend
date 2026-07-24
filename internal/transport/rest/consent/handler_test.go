package consent

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
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/consent"
)

// --- auth plumbing: the test router runs the real middleware.Auth, so tests
// exercise the same AuthUser path as production. The access token is the user id.

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

type fakeUsers struct{}

func (fakeUsers) Create(context.Context, *domain.User) error { return nil }
func (fakeUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	return &domain.User{ID: id, Role: domain.RoleUser, IsActive: true}, nil
}
func (fakeUsers) GetByEmail(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (fakeUsers) GetByPhone(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (fakeUsers) Update(context.Context, *domain.User) error { return nil }
func (fakeUsers) Delete(context.Context, uuid.UUID) error    { return nil }

// fakeFacade records the id each method was called with, so tests can assert
// the handler never leaks another user's id, and holds a tiny in-memory store
// so a record-then-read round-trips.
type fakeFacade struct {
	records map[uuid.UUID][]domain.ConsentRecord
	prefs   map[uuid.UUID]domain.NotificationPreference

	lastRecordID uuid.UUID
	lastStateID  uuid.UUID
	lastGetID    uuid.UUID
	lastSetID    uuid.UUID
}

func newFakeFacade() *fakeFacade {
	return &fakeFacade{
		records: map[uuid.UUID][]domain.ConsentRecord{},
		prefs:   map[uuid.UUID]domain.NotificationPreference{},
	}
}

func (f *fakeFacade) Record(_ context.Context, userID uuid.UUID, in uc.RecordInput) (*domain.ConsentRecord, error) {
	f.lastRecordID = userID
	if in.ConsentType == "" {
		return nil, fmt.Errorf("%w: consent_type", domain.ErrValidation)
	}
	rec := domain.ConsentRecord{
		ID: uuid.New(), UserID: userID, ConsentType: in.ConsentType, Version: in.Version,
		Granted: in.Granted, Source: in.Source, CreatedAt: time.Now(),
	}
	f.records[userID] = append(f.records[userID], rec)
	return &rec, nil
}

func (f *fakeFacade) CurrentState(_ context.Context, userID uuid.UUID) ([]domain.ConsentRecord, error) {
	f.lastStateID = userID
	return f.records[userID], nil
}

func (f *fakeFacade) Preferences(_ context.Context, userID uuid.UUID) (domain.NotificationPreference, error) {
	f.lastGetID = userID
	p, ok := f.prefs[userID]
	if !ok {
		return domain.DefaultNotificationPreference(userID), nil
	}
	return p, nil
}

func (f *fakeFacade) SetPreferences(_ context.Context, userID uuid.UUID, in uc.PreferenceInput) (domain.NotificationPreference, error) {
	f.lastSetID = userID
	p := domain.NotificationPreference{
		UserID: userID, NotificationsEnabled: in.NotificationsEnabled,
		PushEnabled: in.PushEnabled, EmailEnabled: in.EmailEnabled, UpdatedAt: time.Now(),
	}
	f.prefs[userID] = p
	return p, nil
}

func newRouter(f *fakeFacade) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{}))
	NewHandler(f).RegisterRoutes(authed)
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

func TestRecordThenReadOwnConsent(t *testing.T) {
	id := uuid.New()
	f := newFakeFacade()
	r := newRouter(f)

	// Grant, then revoke the same type.
	if w := do(r, http.MethodPost, "/api/v1/consents",
		map[string]any{"consent_type": "privacy_policy", "version": "v1", "granted": true, "source": "app"},
		id.String()); w.Code != http.StatusCreated {
		t.Fatalf("grant status = %d, body = %s", w.Code, w.Body.String())
	}
	if w := do(r, http.MethodPost, "/api/v1/consents",
		map[string]any{"consent_type": "privacy_policy", "version": "v1", "granted": false, "source": "web"},
		id.String()); w.Code != http.StatusCreated {
		t.Fatalf("revoke status = %d, body = %s", w.Code, w.Body.String())
	}
	if f.lastRecordID != id {
		t.Errorf("Record called with %v, want caller's own id %v", f.lastRecordID, id)
	}

	// Read state — history preserved in the fake store (2 rows).
	w := do(r, http.MethodGet, "/api/v1/consents", nil, id.String())
	if w.Code != http.StatusOK {
		t.Fatalf("read status = %d", w.Code)
	}
	var env response.Envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	raw, _ := json.Marshal(env.Data)
	var got []consentResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both records back from the fake, got %d", len(got))
	}
	if f.lastStateID != id {
		t.Errorf("CurrentState called with %v, want %v", f.lastStateID, id)
	}
}

func TestRecordValidationRejectsEmptyType(t *testing.T) {
	id := uuid.New()
	w := do(newRouter(newFakeFacade()), http.MethodPost, "/api/v1/consents",
		map[string]any{"consent_type": "", "version": "v1", "source": "app"}, id.String())
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
}

// Consent and preferences are ALWAYS resolved from the auth token, never a
// body/path-supplied id: two different callers each only ever reach their own id.
func TestConsentNeverLeaksAnotherUsersID(t *testing.T) {
	self := uuid.New()
	other := uuid.New()
	f := newFakeFacade()
	r := newRouter(f)

	// Pre-seed the OTHER user's records directly in the fake store.
	f.records[other] = []domain.ConsentRecord{{ID: uuid.New(), UserID: other, ConsentType: "marketing"}}

	w := do(r, http.MethodGet, "/api/v1/consents", nil, self.String())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if f.lastStateID != self {
		t.Fatalf("CurrentState resolved %v, want the caller's own id %v", f.lastStateID, self)
	}
	var env response.Envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	raw, _ := json.Marshal(env.Data)
	var got []consentResponse
	_ = json.Unmarshal(raw, &got)
	if len(got) != 0 {
		t.Fatalf("caller must not see another user's records, got %d", len(got))
	}
}

func TestConsentRequiresAuth(t *testing.T) {
	f := newFakeFacade()
	r := newRouter(f)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/consents"},
		{http.MethodPost, "/api/v1/consents"},
		{http.MethodGet, "/api/v1/notification-preferences"},
		{http.MethodPut, "/api/v1/notification-preferences"},
	} {
		w := do(r, tc.method, tc.path, nil, "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want 401", tc.method, tc.path, w.Code)
		}
	}
}

func TestNotificationOptOutPersistsAndReadsBack(t *testing.T) {
	id := uuid.New()
	f := newFakeFacade()
	r := newRouter(f)

	// Default read: all enabled.
	w := do(r, http.MethodGet, "/api/v1/notification-preferences", nil, id.String())
	if w.Code != http.StatusOK {
		t.Fatalf("get default status = %d", w.Code)
	}
	var env response.Envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	raw, _ := json.Marshal(env.Data)
	var def preferenceResponse
	_ = json.Unmarshal(raw, &def)
	if !def.NotificationsEnabled {
		t.Fatalf("unset preference should default to enabled")
	}

	// Opt out.
	w = do(r, http.MethodPut, "/api/v1/notification-preferences",
		map[string]any{"notifications_enabled": false}, id.String())
	if w.Code != http.StatusOK {
		t.Fatalf("set status = %d, body = %s", w.Code, w.Body.String())
	}
	if f.lastSetID != id {
		t.Errorf("SetPreferences called with %v, want %v", f.lastSetID, id)
	}

	// Read back the opt-out.
	w = do(r, http.MethodGet, "/api/v1/notification-preferences", nil, id.String())
	env = response.Envelope{}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	raw, _ = json.Marshal(env.Data)
	var got preferenceResponse
	_ = json.Unmarshal(raw, &got)
	if got.NotificationsEnabled {
		t.Fatalf("opt-out did not persist through the handler")
	}
}
