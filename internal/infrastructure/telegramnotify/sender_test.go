package telegramnotify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigured(t *testing.T) {
	if (Config{}).Configured() {
		t.Fatal("empty token must not be Configured")
	}
	if (Config{BotToken: "  "}).Configured() {
		t.Fatal("blank token must not be Configured")
	}
	if !(Config{BotToken: "123:abc"}).Configured() {
		t.Fatal("a token must be Configured")
	}
}

// Send posts the correct sendMessage body to /bot<token>/sendMessage and
// returns the server's status code.
func TestSend_PostsSendMessage(t *testing.T) {
	const token = "111:secretTOKEN"
	var gotPath string
	var gotBody sendMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	s := NewSender(Config{BotToken: token})
	s.baseURL = srv.URL

	status, err := s.Send(context.Background(), "-100123", "Новая бронь")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if gotPath != "/bot"+token+"/sendMessage" {
		t.Fatalf("path = %q, want /bot<token>/sendMessage", gotPath)
	}
	if gotBody.ChatID != "-100123" || gotBody.Text != "Новая бронь" {
		t.Fatalf("body = %+v, want chat -100123 / text 'Новая бронь'", gotBody)
	}
}

// A non-2xx status is returned as-is for the caller's retry logic.
func TestSend_ReturnsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	s := NewSender(Config{BotToken: "t:ok"})
	s.baseURL = srv.URL
	status, err := s.Send(context.Background(), "1", "x")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
}

// A transport error must NEVER carry the bot token (a *url.Error embeds the full
// URL, token included) — scrub replaces it with ***.
func TestSend_TransportErrorScrubsToken(t *testing.T) {
	const token = "999:VERYSECRET"
	// Point at a closed server → client.Do fails with a *url.Error containing
	// the URL (and the token).
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	s := NewSender(Config{BotToken: token})
	s.baseURL = closedURL
	_, err := s.Send(context.Background(), "1", "x")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked the bot token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("error should have a scrubbed marker, got %q", err.Error())
	}
}
