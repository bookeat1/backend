package kwaakahook

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeInbox struct {
	rows map[string]domain.KwaakaWebhookEvent
	err  error
}

func (f *fakeInbox) Insert(_ context.Context, e *domain.KwaakaWebhookEvent) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if _, ok := f.rows[e.DedupKey]; ok {
		return false, nil
	}
	f.rows[e.DedupKey] = *e
	return true, nil
}
func (f *fakeInbox) LockDue(context.Context, time.Time, int) ([]domain.KwaakaWebhookEvent, error) {
	return nil, nil
}
func (f *fakeInbox) Finish(context.Context, uuid.UUID, string, *string, *uuid.UUID) error { return nil }
func (f *fakeInbox) Defer(context.Context, uuid.UUID, time.Time) error                    { return nil }
func (f *fakeInbox) PruneProcessed(context.Context, time.Time, int) (int, error)          { return 0, nil }

func do(t *testing.T, h *Handler, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.RegisterRoutes(r.Group("/api/v1"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1"+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func newH(secret string, in *fakeInbox) *Handler {
	return NewHandler(in, secret, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestReceive(t *testing.T) {
	in := &fakeInbox{rows: map[string]domain.KwaakaWebhookEvent{}}
	h := newH("s3", in)
	ok := map[string]string{secretHeader: "s3"}
	body := `{"orderId":"K1","status":"open"}`
	if w := do(t, h, "/webhooks/kwaaka/order-status", body, ok); w.Code != 200 {
		t.Fatal(w.Code)
	}
	// byte-for-byte repeat: 200, still one row
	if w := do(t, h, "/webhooks/kwaaka/order-status", body, ok); w.Code != 200 || len(in.rows) != 1 {
		t.Fatalf("%d rows=%d", w.Code, len(in.rows))
	}
	// reserve-status skeleton: accepted and kept (a different kind → its own row)
	if w := do(t, h, "/webhooks/kwaaka/reserve-status", `{"reserveId":"R","status":"x"}`, ok); w.Code != 200 || len(in.rows) != 2 {
		t.Fatalf("%d rows=%d", w.Code, len(in.rows))
	}
	// garbage is stored, not judged here
	if w := do(t, h, "/webhooks/kwaaka/order-status", `garbage`, ok); w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestReceiveAuth(t *testing.T) {
	in := &fakeInbox{rows: map[string]domain.KwaakaWebhookEvent{}}
	h := newH("s3", in)
	for name, hdr := range map[string]map[string]string{"none": nil, "wrong": {secretHeader: "x"}, "empty": {secretHeader: ""}} {
		if w := do(t, h, "/webhooks/kwaaka/order-status", `{}`, hdr); w.Code != 401 {
			t.Fatalf("%s: %d", name, w.Code)
		}
	}
	if len(in.rows) != 0 {
		t.Fatal("unauthenticated body stored")
	}
}

// An empty secret must NEVER disable auth: the routes answer 404 and the
// anonymous POST writes nothing (ADR-050).
func TestEmptySecretIs404AndWritesNothing(t *testing.T) {
	in := &fakeInbox{rows: map[string]domain.KwaakaWebhookEvent{}}
	for _, secret := range []string{"", "   "} {
		h := newH(secret, in)
		for _, path := range []string{"/webhooks/kwaaka/order-status", "/webhooks/kwaaka/reserve-status"} {
			// anonymous, and also with a header guess (empty secret must not match "")
			for _, hdr := range []map[string]string{nil, {secretHeader: ""}, {secretHeader: "x"}} {
				if w := do(t, h, path, `{"orderId":"a","status":"b"}`, hdr); w.Code != 404 {
					t.Fatalf("secret=%q %s hdr=%v: got %d, want 404", secret, path, hdr, w.Code)
				}
			}
		}
	}
	if len(in.rows) != 0 {
		t.Fatalf("anonymous POST wrote %d rows", len(in.rows))
	}
}

// KWAAKA_ORDERS_ENABLED=false: 404 even with a correct secret, nothing stored.
func TestFlagOffIs404AndWritesNothing(t *testing.T) {
	in := &fakeInbox{rows: map[string]domain.KwaakaWebhookEvent{}}
	h := NewHandler(in, "s3", false, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, hdr := range []map[string]string{nil, {secretHeader: "s3"}} {
		if w := do(t, h, "/webhooks/kwaaka/order-status", `{"orderId":"a","status":"b"}`, hdr); w.Code != 404 {
			t.Fatalf("hdr=%v: got %d, want 404", hdr, w.Code)
		}
	}
	if len(in.rows) != 0 {
		t.Fatal("flag-off endpoint stored a body")
	}
}

func TestReceiveLimitsAndDBError(t *testing.T) {
	in := &fakeInbox{rows: map[string]domain.KwaakaWebhookEvent{}}
	big := strings.Repeat("a", maxBody+1)
	ok := map[string]string{secretHeader: "s3"}
	if w := do(t, newH("s3", in), "/webhooks/kwaaka/order-status", big, ok); w.Code != 413 || len(in.rows) != 0 {
		t.Fatal(w.Code)
	}
	in.err = errors.New("db down")
	if w := do(t, newH("s3", in), "/webhooks/kwaaka/order-status", `{}`, ok); w.Code != 503 {
		t.Fatal(w.Code)
	}
}
