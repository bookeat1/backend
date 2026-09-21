package pushcampaigns

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
	uc "backend-core/internal/usecase/pushcampaigns"
)

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

type fakeUsers struct{ roles map[uuid.UUID]domain.Role }

func (f fakeUsers) Create(context.Context, *domain.User) error { return nil }
func (f fakeUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	role, ok := f.roles[id]
	if !ok {
		role = domain.RoleUser
	}
	return &domain.User{ID: id, Role: role, IsActive: true}, nil
}
func (f fakeUsers) GetByEmail(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (f fakeUsers) GetByPhone(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (f fakeUsers) Update(context.Context, *domain.User) error { return nil }
func (f fakeUsers) Delete(context.Context, uuid.UUID) error    { return nil }

// fakeFacade is a scripted usecase/pushcampaigns.Facade double.
type fakeFacade struct {
	estimateFn func(ctx context.Context, actor uc.Actor, kind domain.PushCampaignKind, subjectID uuid.UUID) (*uc.EstimateResult, error)
	createFn   func(ctx context.Context, actor uc.Actor, in uc.CreateInput) (*domain.PushCampaign, error)
	listFn     func(ctx context.Context, actor uc.Actor, kind domain.PushCampaignKind, restaurantID *uuid.UUID, platform bool) ([]domain.PushCampaign, error)
	getFn      func(ctx context.Context, actor uc.Actor, id uuid.UUID) (*uc.GetResult, error)
}

func (f *fakeFacade) Estimate(ctx context.Context, actor uc.Actor, kind domain.PushCampaignKind, subjectID uuid.UUID) (*uc.EstimateResult, error) {
	return f.estimateFn(ctx, actor, kind, subjectID)
}
func (f *fakeFacade) Create(ctx context.Context, actor uc.Actor, in uc.CreateInput) (*domain.PushCampaign, error) {
	return f.createFn(ctx, actor, in)
}
func (f *fakeFacade) ListLatestBySubjects(ctx context.Context, actor uc.Actor, kind domain.PushCampaignKind, restaurantID *uuid.UUID, platform bool) ([]domain.PushCampaign, error) {
	return f.listFn(ctx, actor, kind, restaurantID, platform)
}
func (f *fakeFacade) Get(ctx context.Context, actor uc.Actor, id uuid.UUID) (*uc.GetResult, error) {
	return f.getFn(ctx, actor, id)
}

func newRouter(f *fakeFacade, roles map[uuid.UUID]domain.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{roles: roles}))
	NewHandler(f).RegisterRoutes(authed)
	return r
}

func do(r *gin.Engine, method, path string, body any, bearer string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeEnvelope(t *testing.T, w *httptest.ResponseRecorder) response.Envelope {
	t.Helper()
	var env response.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (body: %s)", err, w.Body.String())
	}
	return env
}

func TestEstimateReturns200(t *testing.T) {
	admin := uuid.New()
	subjectID := uuid.New()
	f := &fakeFacade{estimateFn: func(_ context.Context, actor uc.Actor, kind domain.PushCampaignKind, sid uuid.UUID) (*uc.EstimateResult, error) {
		if actor.Role != domain.RoleAdmin || kind != domain.PushCampaignKindEvent || sid != subjectID {
			t.Fatalf("unexpected call: role=%s kind=%s subject=%s", actor.Role, kind, sid)
		}
		return &uc.EstimateResult{City: "Алматы", InCity: 10, Eligible: 8, Preview: map[string]uc.PushText{"ru": {Title: "t", Body: "b"}}}, nil
	}}
	r := newRouter(f, map[uuid.UUID]domain.Role{admin: domain.RoleAdmin})

	w := do(r, http.MethodGet, "/api/v1/admin/push-campaigns/estimate?kind=event&subject_id="+subjectID.String(), nil, admin.String())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	env := decodeEnvelope(t, w)
	raw, _ := json.Marshal(env.Data)
	var got estimateResponse
	_ = json.Unmarshal(raw, &got)
	if got.City != "Алматы" || got.Eligible != 8 {
		t.Fatalf("got %+v", got)
	}
}

func TestEstimateInvalidKind(t *testing.T) {
	admin := uuid.New()
	f := &fakeFacade{}
	r := newRouter(f, map[uuid.UUID]domain.Role{admin: domain.RoleAdmin})
	w := do(r, http.MethodGet, "/api/v1/admin/push-campaigns/estimate?kind=bogus&subject_id="+uuid.New().String(), nil, admin.String())
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
}

// TestCreateErrorMapping pins criterion 4's status codes end to end: the
// facade's sentinel+code errors reach the client as the right HTTP status
// and machine-readable code.
func TestCreateErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"forbidden", fmt.Errorf("%w: only admin", domain.ErrForbidden), http.StatusForbidden, "forbidden"},
		{"not found", fmt.Errorf("%w", domain.ErrNotFound), http.StatusNotFound, "not_found"},
		{"subject not published", domain.WithCode(domain.CodeSubjectNotPublished, fmt.Errorf("%w", domain.ErrValidation)), http.StatusUnprocessableEntity, "subject_not_published"},
		{"quiet hours", domain.WithCode(domain.CodeQuietHours, fmt.Errorf("%w", domain.ErrValidation)), http.StatusUnprocessableEntity, "quiet_hours"},
		{"conflict", domain.WithCode(domain.CodeCampaignInProgress, fmt.Errorf("%w", domain.ErrAlreadyExists)), http.StatusConflict, "campaign_in_progress"},
		{"channel disabled", domain.WithCode(domain.CodePushChannelDisabled, fmt.Errorf("%w", domain.ErrUnavailable)), http.StatusServiceUnavailable, "push_channel_disabled"},
	}
	admin := uuid.New()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeFacade{createFn: func(context.Context, uc.Actor, uc.CreateInput) (*domain.PushCampaign, error) {
				return nil, c.err
			}}
			r := newRouter(f, map[uuid.UUID]domain.Role{admin: domain.RoleAdmin})
			w := do(r, http.MethodPost, "/api/v1/admin/push-campaigns",
				map[string]any{"kind": "event", "subject_id": uuid.New().String()}, admin.String())
			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, c.wantStatus, w.Body.String())
			}
			env := decodeEnvelope(t, w)
			if env.Code != c.wantCode {
				t.Fatalf("code = %q, want %q", env.Code, c.wantCode)
			}
		})
	}
}

func TestCreateHappyPathReturns201(t *testing.T) {
	admin := uuid.New()
	campaignID := uuid.New()
	f := &fakeFacade{createFn: func(_ context.Context, actor uc.Actor, in uc.CreateInput) (*domain.PushCampaign, error) {
		return &domain.PushCampaign{ID: campaignID, Status: domain.PushCampaignQueued, EstimatedRecipients: 5}, nil
	}}
	r := newRouter(f, map[uuid.UUID]domain.Role{admin: domain.RoleAdmin})
	w := do(r, http.MethodPost, "/api/v1/admin/push-campaigns",
		map[string]any{"kind": "event", "subject_id": uuid.New().String()}, admin.String())
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	env := decodeEnvelope(t, w)
	raw, _ := json.Marshal(env.Data)
	var got createdResponse
	_ = json.Unmarshal(raw, &got)
	if got.ID != campaignID.String() || got.Status != "queued" || got.EstimatedRecipients != 5 {
		t.Fatalf("got %+v", got)
	}
}

func TestListAndGetRouteResolution(t *testing.T) {
	admin := uuid.New()
	subjectID := uuid.New()
	campaignID := uuid.New()
	f := &fakeFacade{
		listFn: func(_ context.Context, _ uc.Actor, _ domain.PushCampaignKind, _ *uuid.UUID, _ bool) ([]domain.PushCampaign, error) {
			return []domain.PushCampaign{{ID: campaignID, SubjectID: subjectID, Status: domain.PushCampaignDone}}, nil
		},
		getFn: func(_ context.Context, _ uc.Actor, id uuid.UUID) (*uc.GetResult, error) {
			if id != campaignID {
				t.Fatalf("get called with %s, want %s", id, campaignID)
			}
			return &uc.GetResult{Campaign: domain.PushCampaign{ID: id, Status: domain.PushCampaignDone}, SkipCounts: map[domain.PushCampaignRecipientStatus]int{domain.RecipientSkippedOptOut: 3}}, nil
		},
	}
	r := newRouter(f, map[uuid.UUID]domain.Role{admin: domain.RoleAdmin})

	w := do(r, http.MethodGet, "/api/v1/admin/push-campaigns?kind=event&platform=true", nil, admin.String())
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", w.Code, w.Body.String())
	}
	env := decodeEnvelope(t, w)
	raw, _ := json.Marshal(env.Data)
	var list []summaryResponse
	_ = json.Unmarshal(raw, &list)
	if len(list) != 1 || list[0].SubjectID != subjectID.String() {
		t.Fatalf("list = %+v", list)
	}

	// "estimate" as a literal segment must never be captured by the
	// GET /admin/push-campaigns/:id route.
	w = do(r, http.MethodGet, "/api/v1/admin/push-campaigns/"+campaignID.String(), nil, admin.String())
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", w.Code, w.Body.String())
	}
	env = decodeEnvelope(t, w)
	raw, _ = json.Marshal(env.Data)
	var got detailResponse
	_ = json.Unmarshal(raw, &got)
	if got.ID != campaignID.String() || got.SkipCounts["skipped_optout"] != 3 {
		t.Fatalf("detail = %+v", got)
	}
}

func TestUnauthenticatedRequestsAre401(t *testing.T) {
	r := newRouter(&fakeFacade{}, nil)
	w := do(r, http.MethodGet, "/api/v1/admin/push-campaigns?kind=event&platform=true", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
