package notifications

import (
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
	uc "backend-core/internal/usecase/notifications"
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

// fakeFeed is a minimal domain.NotificationFeedRepository for the handler tests.
type fakeFeed struct {
	rows []domain.Notification
}

func (f *fakeFeed) Insert(_ context.Context, n *domain.Notification) (bool, error) {
	f.rows = append(f.rows, *n)
	return true, nil
}

func (f *fakeFeed) ListByUser(_ context.Context, userID uuid.UUID, cursor *domain.NotificationCursor, limit int) ([]domain.Notification, error) {
	var out []domain.Notification
	for _, r := range f.rows {
		if r.UserID != userID {
			continue
		}
		if cursor != nil && !r.CreatedAt.Before(cursor.CreatedAt) {
			continue
		}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeFeed) CountUnread(_ context.Context, userID uuid.UUID) (int, error) {
	n := 0
	for _, r := range f.rows {
		if r.UserID == userID && r.ReadAt == nil {
			n++
		}
	}
	return n, nil
}

func (f *fakeFeed) MarkRead(_ context.Context, id, userID uuid.UUID) error {
	for i := range f.rows {
		if f.rows[i].ID == id && f.rows[i].UserID == userID {
			now := time.Now()
			f.rows[i].ReadAt = &now
			return nil
		}
	}
	return domain.ErrNotFound
}

func (f *fakeFeed) MarkAllRead(_ context.Context, userID uuid.UUID) error {
	now := time.Now()
	for i := range f.rows {
		if f.rows[i].UserID == userID && f.rows[i].ReadAt == nil {
			f.rows[i].ReadAt = &now
		}
	}
	return nil
}

func newRouter(feed domain.NotificationFeedRepository) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(uc.NewNotificationFeedUseCase(feed))
	api := r.Group("/api/v1")
	authed := api.Group("")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{}))
	h.RegisterRoutes(authed)
	return r
}

func do(r *gin.Engine, method, path, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func seed(feed *fakeFeed, userID uuid.UUID, created time.Time, read bool) uuid.UUID {
	id := uuid.New()
	var readAt *time.Time
	if read {
		t := created.Add(time.Minute)
		readAt = &t
	}
	feed.rows = append(feed.rows, domain.Notification{
		ID: id, UserID: userID, Type: domain.FeedTypeBooking,
		Title: "Бронь подтверждена", Body: "b", OutboxEventID: uuid.New(),
		ReadAt: readAt, CreatedAt: created,
	})
	return id
}

func decodeFeed(t *testing.T, w *httptest.ResponseRecorder) feedResponse {
	t.Helper()
	var env response.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	raw, _ := json.Marshal(env.Data)
	var fr feedResponse
	if err := json.Unmarshal(raw, &fr); err != nil {
		t.Fatalf("unmarshal feed: %v", err)
	}
	return fr
}

func TestListReturnsItemsAndUnreadCount(t *testing.T) {
	feed := &fakeFeed{}
	uid := uuid.New()
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seed(feed, uid, base, false)
	seed(feed, uid, base.Add(time.Minute), true)
	seed(feed, uuid.New(), base, false) // other user's row must not leak

	w := do(newRouter(feed), http.MethodGet, "/api/v1/notifications", uid.String())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	fr := decodeFeed(t, w)
	if len(fr.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(fr.Items))
	}
	if fr.UnreadCount != 1 {
		t.Errorf("unread_count = %d, want 1", fr.UnreadCount)
	}
}

func TestListRequiresAuth(t *testing.T) {
	w := do(newRouter(&fakeFeed{}), http.MethodGet, "/api/v1/notifications", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestListRejectsBadCursor(t *testing.T) {
	uid := uuid.New()
	w := do(newRouter(&fakeFeed{}), http.MethodGet, "/api/v1/notifications?cursor=@@notbase64@@", uid.String())
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
}

func TestMarkReadScopesToOwner(t *testing.T) {
	feed := &fakeFeed{}
	owner := uuid.New()
	other := uuid.New()
	id := seed(feed, owner, time.Now(), false)

	// Another guest gets a 404, never mutates the owner's row.
	w := do(newRouter(feed), http.MethodPost, "/api/v1/notifications/"+id.String()+"/read", other.String())
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-user mark-read status = %d, want 404", w.Code)
	}
	if n, _ := feed.CountUnread(context.Background(), owner); n != 1 {
		t.Fatalf("owner row must stay unread, count=%d", n)
	}

	// The owner succeeds.
	w = do(newRouter(feed), http.MethodPost, "/api/v1/notifications/"+id.String()+"/read", owner.String())
	if w.Code != http.StatusOK {
		t.Fatalf("owner mark-read status = %d, body=%s", w.Code, w.Body.String())
	}
	if n, _ := feed.CountUnread(context.Background(), owner); n != 0 {
		t.Fatalf("owner row should be read, count=%d", n)
	}
}

func TestMarkReadRejectsBadID(t *testing.T) {
	w := do(newRouter(&fakeFeed{}), http.MethodPost, "/api/v1/notifications/not-a-uuid/read", uuid.New().String())
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
}

func TestReadAllClearsUnread(t *testing.T) {
	feed := &fakeFeed{}
	uid := uuid.New()
	seed(feed, uid, time.Now(), false)
	seed(feed, uid, time.Now(), false)

	w := do(newRouter(feed), http.MethodPost, "/api/v1/notifications/read-all", uid.String())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if n, _ := feed.CountUnread(context.Background(), uid); n != 0 {
		t.Fatalf("unread after read-all = %d, want 0", n)
	}
}

var _ domain.NotificationFeedRepository = (*fakeFeed)(nil)
