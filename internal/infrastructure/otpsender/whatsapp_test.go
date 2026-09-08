package otpsender

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testWAToken = "fake-meta-token" //nolint:gosec // test-only literal
	testWAPNID  = "123456789"
)

func newTestWhatsApp(t *testing.T, srv *httptest.Server, copyButton bool) *WhatsApp {
	t.Helper()
	cfg := WhatsAppConfig{
		AccessToken:    testWAToken,
		PhoneNumberID:  testWAPNID,
		TemplateName:   "otp_bookeat_ru",
		TemplateLang:   "ru",
		APIVersion:     "v23.0",
		CopyCodeButton: copyButton,
		Timeout:        2 * time.Second,
		BaseURL:        srv.URL,
	}
	if !cfg.Configured() {
		t.Fatal("test config should be Configured()")
	}
	return NewWhatsApp(cfg)
}

// Meta rejects an authentication template whose parameter count does not match
// the approved one, so the payload shape IS the integration. This pins it.
func TestWhatsAppSendRequestShape(t *testing.T) {
	srv, captured := fakeProvider(t, map[string]func(http.ResponseWriter){
		"*": jsonResponse(200, `{"messages":[{"id":"wamid.X"}]}`),
	})

	messageID, err := newTestWhatsApp(t, srv, true).Send(context.Background(), testPhone, testCode)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// The wamid is the only handle Meta and we share: it is what the delivery
	// webhook and the Business Manager dashboard key on, so it is what the
	// waterfall logs.
	if messageID != "wamid.X" {
		t.Errorf("message id = %q, want the wamid %q", messageID, "wamid.X")
	}

	req := (*captured)[0]
	if want := "/v23.0/" + testWAPNID + "/messages"; req.path != want {
		t.Errorf("path = %s, want %s", req.path, want)
	}
	if req.auth != "Bearer "+testWAToken {
		t.Errorf("Authorization = %q, want a bearer token", req.auth)
	}
	if req.body["messaging_product"] != "whatsapp" {
		t.Errorf("messaging_product = %v", req.body["messaging_product"])
	}
	// Meta wants the number without the leading "+".
	if req.body["to"] != strings.TrimPrefix(testPhone, "+") {
		t.Errorf("to = %v, want %s (digits only)", req.body["to"], strings.TrimPrefix(testPhone, "+"))
	}
	tpl, _ := req.body["template"].(map[string]any)
	if tpl["name"] != "otp_bookeat_ru" {
		t.Errorf("template name = %v", tpl["name"])
	}
	lang, _ := tpl["language"].(map[string]any)
	if lang["code"] != "ru" {
		t.Errorf("template language = %v", lang["code"])
	}
	components, _ := tpl["components"].([]any)
	if len(components) != 2 {
		t.Fatalf("components = %d, want 2 (body + copy-code button)", len(components))
	}
	body, _ := components[0].(map[string]any)
	if body["type"] != "body" {
		t.Errorf("first component = %v, want body", body["type"])
	}
	button, _ := components[1].(map[string]any)
	if button["type"] != "button" || button["sub_type"] != "url" {
		t.Errorf("second component = %v/%v, want button/url", button["type"], button["sub_type"])
	}
	if idx, _ := button["index"].(float64); idx != 0 {
		t.Errorf("button index = %v, want 0", button["index"])
	}
}

// A template approved WITHOUT the copy-code button rejects the button
// parameter, so the switch has to actually change the payload.
func TestWhatsAppWithoutCopyButtonSendsBodyOnly(t *testing.T) {
	srv, captured := fakeProvider(t, map[string]func(http.ResponseWriter){
		"*": jsonResponse(200, `{"messages":[{"id":"wamid.X"}]}`),
	})
	if _, err := newTestWhatsApp(t, srv, false).Send(context.Background(), testPhone, testCode); err != nil {
		t.Fatalf("Send: %v", err)
	}
	tpl, _ := (*captured)[0].body["template"].(map[string]any)
	components, _ := tpl["components"].([]any)
	if len(components) != 1 {
		t.Fatalf("components = %d, want 1 (body only)", len(components))
	}
}

func TestWhatsAppOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantErr    error
		wantOK     bool
		wantErrMsg string
	}{
		{
			name:   "accepted",
			status: 200,
			body:   `{"messages":[{"id":"wamid.HBg"}]}`,
			wantOK: true,
		},
		{
			// 200 without a message id is not an acceptance, whatever Meta means
			// by it — falling through is cheaper than a lost login.
			name:       "200 with no message id is not an acceptance",
			status:     200,
			body:       `{"messages":[]}`,
			wantErrMsg: "rejected",
		},
		{
			name:    "recipient has no whatsapp",
			status:  400,
			body:    `{"error":{"code":131026,"error_subcode":0,"message":"Message undeliverable"}}`,
			wantErr: ErrChannelUnavailable,
		},
		{
			name:       "expired token",
			status:     401,
			body:       `{"error":{"code":190,"error_subcode":463,"message":"Session has expired"}}`,
			wantErrMsg: "code 190",
		},
		{
			name:       "html error page",
			status:     502,
			body:       `<html>nope</html>`,
			wantErrMsg: "unreadable response",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeProvider(t, map[string]func(http.ResponseWriter){
				"*": jsonResponse(tc.status, tc.body),
			})
			_, err := newTestWhatsApp(t, srv, true).Send(context.Background(), testPhone, testCode)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("Send: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErrMsg != "" && !strings.Contains(err.Error(), tc.wantErrMsg) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErrMsg)
			}
			// Meta's prose is never echoed, and neither is our token or code.
			if strings.Contains(err.Error(), "Session has expired") || strings.Contains(err.Error(), "Message undeliverable") {
				t.Fatalf("Meta's free-text message reached our error: %q", err)
			}
			assertNoSecrets(t, err, testWAToken)
		})
	}
}

func TestWhatsAppConfigured(t *testing.T) {
	tests := map[string]struct {
		cfg  WhatsAppConfig
		want bool
	}{
		"complete":         {cfg: WhatsAppConfig{AccessToken: "t", PhoneNumberID: "1", TemplateName: "tpl"}, want: true},
		"no token":         {cfg: WhatsAppConfig{PhoneNumberID: "1", TemplateName: "tpl"}},
		"no phone id":      {cfg: WhatsAppConfig{AccessToken: "t", TemplateName: "tpl"}},
		"no template":      {cfg: WhatsAppConfig{AccessToken: "t", PhoneNumberID: "1"}},
		"whitespace token": {cfg: WhatsAppConfig{AccessToken: "  ", PhoneNumberID: "1", TemplateName: "tpl"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tc.cfg.Configured(); got != tc.want {
				t.Fatalf("Configured() = %v, want %v", got, tc.want)
			}
		})
	}
}
