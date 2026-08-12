package telegramhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/bookings"
)

const secret = "s3cret-header"

type fakeSettings struct {
	chatToVenue map[string]uuid.UUID
}

func (f *fakeSettings) RestaurantByTelegramChatID(_ context.Context, chatID string) (uuid.UUID, error) {
	if id, ok := f.chatToVenue[chatID]; ok {
		return id, nil
	}
	return uuid.Nil, domain.ErrNotFound
}
func (f *fakeSettings) WebPushEnabled(context.Context, uuid.UUID) (bool, error) { return true, nil }
func (f *fakeSettings) TelegramSettings(context.Context, uuid.UUID) (domain.TelegramSettings, error) {
	return domain.TelegramSettings{}, nil
}
func (f *fakeSettings) SetTelegramChatID(context.Context, uuid.UUID, string) error { return nil }
func (f *fakeSettings) ClearTelegramChatID(context.Context, uuid.UUID) error       { return nil }

// WhatsApp-половина того же порта: этот тест про телеграм, поэтому здесь заглушки.
func (f *fakeSettings) WhatsAppSettings(context.Context, uuid.UUID) (domain.WhatsAppSettings, error) {
	return domain.WhatsAppSettings{}, nil
}
func (f *fakeSettings) SetWhatsAppPhone(context.Context, uuid.UUID, string) error { return nil }
func (f *fakeSettings) ClearWhatsAppPhone(context.Context, uuid.UUID) error       { return nil }
func (f *fakeSettings) RestaurantByWhatsAppPhone(context.Context, string) (uuid.UUID, error) {
	return uuid.Nil, domain.ErrNotFound
}

type fakeAnswer struct {
	toast  string
	edited string
	calls  int
}

func (f *fakeAnswer) AnswerCallback(_ context.Context, _, text string) error {
	f.toast = text
	f.calls++
	return nil
}
func (f *fakeAnswer) EditMessageText(_ context.Context, _ string, _ int64, text string) error {
	f.edited = text
	return nil
}

// fakeStatus records what the handler asked for and returns a canned outcome.
type fakeStatus struct {
	uc.StatusUseCase
	gotRestaurant uuid.UUID
	gotBooking    uuid.UUID
	gotDecision   uc.VenueDecision
	res           uc.VenueDecisionResult
	err           error
	called        int
}

func (f *fakeStatus) DecideAsVenue(_ context.Context, restaurantID, bookingID uuid.UUID, d uc.VenueDecision) (uc.VenueDecisionResult, error) {
	f.gotRestaurant, f.gotBooking, f.gotDecision = restaurantID, bookingID, d
	f.called++
	return f.res, f.err
}

func newRouter(t *testing.T, st *fakeStatus, s *fakeSettings, a *fakeAnswer, sec string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(st, s, a, sec).RegisterRoutes(r.Group("/api/v1"))
	return r
}

func press(t *testing.T, r *gin.Engine, header string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telegram/webhook", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", header)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func callback(chatID int64, data string) gin.H {
	return gin.H{"callback_query": gin.H{
		"id": "cb1", "data": data,
		"from":    gin.H{"first_name": "Алмас"},
		"message": gin.H{"message_id": 42, "chat": gin.H{"id": chatID}, "text": "Новая бронь"},
	}}
}

// Without the header this endpoint is a way for anyone who guesses the URL to
// confirm other people's bookings. Nothing may reach the usecase.
func TestRejectsAPressWithoutTheSecretHeader(t *testing.T) {
	st := &fakeStatus{}
	r := newRouter(t, st, &fakeSettings{}, &fakeAnswer{}, secret)

	for _, h := range []string{"", "wrong"} {
		w := press(t, r, h, callback(100, CallbackConfirm(uuid.New())))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("header %q: status = %d, want 401", h, w.Code)
		}
	}
	if st.called != 0 {
		t.Fatal("an unauthenticated press must never reach the decision")
	}
}

// A missing secret means the deployment never configured this. Answering 404
// keeps an unconfigured endpoint from advertising itself, and it must be inert.
func TestUnconfiguredWebhookIsInert(t *testing.T) {
	st := &fakeStatus{}
	r := newRouter(t, st, &fakeSettings{}, &fakeAnswer{}, "")

	w := press(t, r, "anything", callback(100, CallbackConfirm(uuid.New())))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if st.called != 0 {
		t.Fatal("nothing may be decided while the webhook is unconfigured")
	}
}

// The chat is the credential. One that belongs to no venue decides nothing.
func TestPressFromAnUnknownChatDecidesNothing(t *testing.T) {
	st := &fakeStatus{}
	ans := &fakeAnswer{}
	r := newRouter(t, st, &fakeSettings{chatToVenue: map[string]uuid.UUID{}}, ans, secret)

	w := press(t, r, secret, callback(999, CallbackConfirm(uuid.New())))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Telegram must not retry)", w.Code)
	}
	if st.called != 0 {
		t.Fatal("an unknown chat must not reach the decision")
	}
	if ans.toast == "" {
		t.Fatal("the press must still be acknowledged")
	}
}

// The happy path: the venue behind the chat decides its own booking, the press
// is acknowledged, and the alert is rewritten with who answered.
func TestConfirmPressAppliesTheDecisionAndRewritesTheAlert(t *testing.T) {
	venue, booking := uuid.New(), uuid.New()
	st := &fakeStatus{res: uc.VenueDecisionResult{Applied: true}}
	ans := &fakeAnswer{}
	r := newRouter(t, st, &fakeSettings{chatToVenue: map[string]uuid.UUID{"-100500": venue}}, ans, secret)

	w := press(t, r, secret, callback(-100500, CallbackConfirm(booking)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if st.gotRestaurant != venue || st.gotBooking != booking {
		t.Fatalf("wrong target: venue=%s booking=%s", st.gotRestaurant, st.gotBooking)
	}
	if st.gotDecision != uc.VenueDecisionConfirm {
		t.Fatalf("decision = %q", st.gotDecision)
	}
	if ans.toast != "Подтверждено" {
		t.Fatalf("toast = %q", ans.toast)
	}
	if ans.edited == "" || !bytes.Contains([]byte(ans.edited), []byte("Алмас")) {
		t.Fatalf("the alert must record who answered, got %q", ans.edited)
	}
}

func TestRejectPressPassesTheRejectDecision(t *testing.T) {
	venue := uuid.New()
	st := &fakeStatus{res: uc.VenueDecisionResult{Applied: true}}
	r := newRouter(t, st, &fakeSettings{chatToVenue: map[string]uuid.UUID{"7": venue}}, &fakeAnswer{}, secret)

	press(t, r, secret, callback(7, CallbackReject(uuid.New())))

	if st.gotDecision != uc.VenueDecisionReject {
		t.Fatalf("decision = %q, want reject", st.gotDecision)
	}
}

// A booking the guest already cancelled. The venue is told the truth instead of
// being shown a success it did not get.
func TestConflictIsReportedHonestly(t *testing.T) {
	venue := uuid.New()
	st := &fakeStatus{res: uc.VenueDecisionResult{Conflict: true}}
	ans := &fakeAnswer{}
	r := newRouter(t, st, &fakeSettings{chatToVenue: map[string]uuid.UUID{"7": venue}}, ans, secret)

	press(t, r, secret, callback(7, CallbackConfirm(uuid.New())))

	if ans.toast == "Подтверждено" {
		t.Fatal("a conflict must not read as success")
	}
	if ans.edited == "" {
		t.Fatal("the alert must say the answer was not applied")
	}
}

// Garbage in callback_data, and updates that are not presses at all, must be
// answered 200 and ignored: any other status makes Telegram retry forever.
func TestNonPressUpdatesAreAcknowledgedAndIgnored(t *testing.T) {
	st := &fakeStatus{}
	r := newRouter(t, st, &fakeSettings{chatToVenue: map[string]uuid.UUID{"7": uuid.New()}}, &fakeAnswer{}, secret)

	cases := []any{
		gin.H{"message": gin.H{"text": "привет"}},
		callback(7, "nonsense"),
		callback(7, "bk:explode:not-a-uuid"),
	}
	for i, body := range cases {
		if w := press(t, r, secret, body); w.Code != http.StatusOK {
			t.Fatalf("case %d: status = %d, want 200", i, w.Code)
		}
	}
	if st.called != 0 {
		t.Fatal("no malformed update may reach the decision")
	}
}

// The two builders and the parser must agree; if they drift, every button in
// production silently stops working.
func TestCallbackDataRoundTrips(t *testing.T) {
	id := uuid.New()

	for _, tc := range []struct {
		data string
		want uc.VenueDecision
	}{
		{CallbackConfirm(id), uc.VenueDecisionConfirm},
		{CallbackReject(id), uc.VenueDecisionReject},
	} {
		if len(tc.data) > 64 {
			t.Fatalf("callback data must fit Telegram's 64-byte cap, got %d", len(tc.data))
		}
		d, got, ok := parseCallbackData(tc.data)
		if !ok || d != tc.want || got != id {
			t.Fatalf("round trip failed for %q: %v %v %v", tc.data, d, got, ok)
		}
	}
}
