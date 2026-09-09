package telegramhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const staffSecret = "staff-s3cret"

// fakeMessenger records what the new bot said back into the chat.
type fakeMessenger struct {
	sent []string
	err  error
}

func (f *fakeMessenger) Send(_ context.Context, _, text string) (int, error) {
	f.sent = append(f.sent, text)
	if f.err != nil {
		return 0, f.err
	}
	return 200, nil
}

func newStaffRouter(t *testing.T, st *fakeStatus, s *fakeSettings, a *fakeAnswer, m *fakeMessenger, sec string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewStaffHandler(st, s, a, sec, s, m).RegisterRoutes(r.Group("/api/v1"))
	return r
}

func postStaff(t *testing.T, r *gin.Engine, header string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telegram/staff-webhook", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", header)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func startMessage(chatID int64, text string) gin.H {
	return gin.H{"message": gin.H{
		"text": text,
		"chat": gin.H{"id": chatID, "type": "private"},
	}}
}

func memberUpdate(chatID int64, status string) gin.H {
	return gin.H{"my_chat_member": gin.H{
		"chat":            gin.H{"id": chatID},
		"new_chat_member": gin.H{"status": status},
	}}
}

// The point of the whole self-service step: staff press Start, the venue is
// marked ready, and nobody re-types a chat id anywhere.
func TestStaffWebhook_StartMarksTheVenueReady(t *testing.T) {
	venue := uuid.New()
	s := &fakeSettings{chatToVenue: map[string]uuid.UUID{"555": venue}}
	m := &fakeMessenger{}
	r := newStaffRouter(t, &fakeStatus{}, s, &fakeAnswer{}, m, staffSecret)

	for _, text := range []string{"/start", "/start@book_eat_restaurants_bot", "/start deeplink"} {
		s.readyCalls = nil
		w := postStaff(t, r, staffSecret, startMessage(555, text))
		if w.Code != http.StatusOK {
			t.Fatalf("%q: status = %d, want 200", text, w.Code)
		}
		if len(s.readyCalls) != 1 || s.readyCalls[0] != venue {
			t.Fatalf("%q: readyCalls = %v, want [%s]", text, s.readyCalls, venue)
		}
	}
	if len(m.sent) == 0 {
		t.Fatal("staff got no confirmation that the bot is connected")
	}
}

// A Start from a chat nobody connected proves nothing. It must not mark
// anything, and the only useful thing to say back is the chat's own id, which
// is what the panel asks for.
func TestStaffWebhook_StartFromUnknownChatMarksNothingAndReturnsTheChatID(t *testing.T) {
	s := &fakeSettings{chatToVenue: map[string]uuid.UUID{}}
	m := &fakeMessenger{}
	r := newStaffRouter(t, &fakeStatus{}, s, &fakeAnswer{}, m, staffSecret)

	w := postStaff(t, r, staffSecret, startMessage(777, "/start"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(s.readyCalls) != 0 {
		t.Fatalf("an unknown chat marked %v ready", s.readyCalls)
	}
	if len(m.sent) != 1 || !bytes.Contains([]byte(m.sent[0]), []byte("777")) {
		t.Fatalf("reply = %v, want it to contain the chat id 777", m.sent)
	}
}

// Being added to a venue's group is the group-chat equivalent of pressing
// Start; being removed demotes the venue immediately, so its alerts return to
// the old bot without spending a booking to discover the 403.
func TestStaffWebhook_MyChatMemberMarksReadyAndFailed(t *testing.T) {
	venue := uuid.New()
	cases := []struct {
		status    string
		wantReady int
		wantFail  int
	}{
		{"member", 1, 0},
		{"administrator", 1, 0},
		{"creator", 1, 0},
		{"left", 0, 1},
		{"kicked", 0, 1},
		{"restricted", 0, 0}, // not a statement about being able to write
	}
	for _, tc := range cases {
		s := &fakeSettings{chatToVenue: map[string]uuid.UUID{"-100999": venue}}
		r := newStaffRouter(t, &fakeStatus{}, s, &fakeAnswer{}, &fakeMessenger{}, staffSecret)

		w := postStaff(t, r, staffSecret, memberUpdate(-100999, tc.status))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.status, w.Code)
		}
		if len(s.readyCalls) != tc.wantReady {
			t.Fatalf("%s: readyCalls = %v, want %d", tc.status, s.readyCalls, tc.wantReady)
		}
		if len(s.failedCalls) != tc.wantFail {
			t.Fatalf("%s: failedCalls = %v, want %d", tc.status, s.failedCalls, tc.wantFail)
		}
	}
}

// The OLD bot's webhook must never move the migration flag: it has no
// onboarding port at all. A /start sent to it is ignored, as before.
func TestOldWebhook_StartDoesNotMigrateAnything(t *testing.T) {
	venue := uuid.New()
	s := &fakeSettings{chatToVenue: map[string]uuid.UUID{"555": venue}}
	r := newRouter(t, &fakeStatus{}, s, &fakeAnswer{}, secret)

	w := press(t, r, secret, startMessage(555, "/start"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(s.readyCalls) != 0 || len(s.failedCalls) != 0 {
		t.Fatalf("the old bot's webhook moved the migration flag: ready=%v failed=%v",
			s.readyCalls, s.failedCalls)
	}
}

// The new bot's webhook has its OWN secret. The old bot's secret must not open
// it — otherwise a leak of one credential is a leak of both.
func TestStaffWebhook_RejectsTheOtherBotsSecret(t *testing.T) {
	venue := uuid.New()
	s := &fakeSettings{chatToVenue: map[string]uuid.UUID{"555": venue}}
	r := newStaffRouter(t, &fakeStatus{}, s, &fakeAnswer{}, &fakeMessenger{}, staffSecret)

	for _, h := range []string{"", secret, "wrong"} {
		w := postStaff(t, r, h, startMessage(555, "/start"))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("header %q: status = %d, want 401", h, w.Code)
		}
	}
	if len(s.readyCalls) != 0 {
		t.Fatal("an unauthenticated update must never move the migration flag")
	}
}

// Both webhooks answer presses, each with its own answerer: a press under
// yesterday's alert (sent by the old bot) still works after the new bot is
// live, and a press under a new alert is answered by the new bot's token.
func TestBothWebhooks_AnswerPressesIndependently(t *testing.T) {
	venue := uuid.New()
	chats := map[string]uuid.UUID{"555": venue}
	booking := uuid.New()

	oldAnswer, newAnswer := &fakeAnswer{}, &fakeAnswer{}
	oldStatus, newStatus := &fakeStatus{}, &fakeStatus{}
	oldRouter := newRouter(t, oldStatus, &fakeSettings{chatToVenue: chats}, oldAnswer, secret)
	newRouterEngine := newStaffRouter(t, newStatus, &fakeSettings{chatToVenue: chats}, newAnswer,
		&fakeMessenger{}, staffSecret)

	if w := press(t, oldRouter, secret, callback(555, CallbackConfirm(booking))); w.Code != http.StatusOK {
		t.Fatalf("old webhook status = %d", w.Code)
	}
	if oldStatus.called != 1 || oldAnswer.calls != 1 {
		t.Fatalf("old bot press: usecase=%d answers=%d, want 1/1", oldStatus.called, oldAnswer.calls)
	}
	if newAnswer.calls != 0 {
		t.Fatal("the NEW bot's token answered a press that belongs to the OLD bot")
	}

	if w := postStaff(t, newRouterEngine, staffSecret, callback(555, CallbackConfirm(booking))); w.Code != http.StatusOK {
		t.Fatalf("staff webhook status = %d", w.Code)
	}
	if newStatus.called != 1 || newAnswer.calls != 1 {
		t.Fatalf("new bot press: usecase=%d answers=%d, want 1/1", newStatus.called, newAnswer.calls)
	}
	if oldAnswer.calls != 1 {
		t.Fatal("the OLD bot's token answered a press that belongs to the NEW bot")
	}
}

// A failed write of the flag is logged and answered 200: a non-2xx would make
// Telegram replay the same /start for hours, and the retry cannot fix a DB
// error anyway. Staff are told to try again instead of being left with a
// silently spinning bot.
func TestStaffWebhook_MarkFailureIsAnsweredNotRetried(t *testing.T) {
	venue := uuid.New()
	s := &fakeSettings{
		chatToVenue: map[string]uuid.UUID{"555": venue},
		markErr:     errors.New("db down"),
	}
	m := &fakeMessenger{}
	r := newStaffRouter(t, &fakeStatus{}, s, &fakeAnswer{}, m, staffSecret)

	w := postStaff(t, r, staffSecret, startMessage(555, "/start"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(m.sent) != 1 {
		t.Fatalf("staff were told nothing after a failed connect: %v", m.sent)
	}
}
