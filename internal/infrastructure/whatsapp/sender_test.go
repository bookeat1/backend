package whatsapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "fake-meta-token" //nolint:gosec // test-only literal

// captured is one request the fake Graph endpoint saw.
type captured struct {
	path string
	auth string
	body map[string]any
}

func fakeGraph(t *testing.T, status int, respBody string) (*httptest.Server, *[]captured) {
	t.Helper()
	var seen []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		seen = append(seen, captured{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func testSender(t *testing.T, srv *httptest.Server) *Sender {
	t.Helper()
	cfg := SenderConfig{Config: Config{
		AccessToken:   testToken,
		PhoneNumberID: "1079483488592272",
		APIVersion:    "v22.0",
		Timeout:       2 * time.Second,
		BaseURL:       srv.URL,
	}}
	if !cfg.Configured() {
		t.Fatal("test config should be Configured()")
	}
	return NewSender(cfg)
}

// Meta rejects a template whose parameter count or language does not match the
// approved one, so the payload shape IS the integration. This pins it against
// the approved bookeat_venue_new_booking_ru (four body params, ru, no button).
func TestSenderRequestShape(t *testing.T) {
	srv, seen := fakeGraph(t, 200, `{"messages":[{"id":"wamid.X"}]}`)

	status, err := testSender(t, srv).Send(context.Background(), "+77010000001",
		[]string{"25 августа в 19:30", "4", "Дамир", "+77078692233"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if status != 200 {
		t.Errorf("status = %d", status)
	}
	req := (*seen)[0]
	if want := "/v22.0/1079483488592272/messages"; req.path != want {
		t.Errorf("path = %s, want %s", req.path, want)
	}
	if req.auth != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want a bearer token", req.auth)
	}
	// Meta wants the number without the leading "+".
	if req.body["to"] != "77010000001" {
		t.Errorf("to = %v, want digits only", req.body["to"])
	}
	tpl, _ := req.body["template"].(map[string]any)
	if tpl["name"] != DefaultBookingTemplateName {
		t.Errorf("template = %v, want the approved venue template", tpl["name"])
	}
	lang, _ := tpl["language"].(map[string]any)
	if lang["code"] != "ru" {
		t.Errorf("language = %v, want ru", lang["code"])
	}
	comps, _ := tpl["components"].([]any)
	if len(comps) != 1 {
		t.Fatalf("components = %d, want 1 (body only — this template has no button)", len(comps))
	}
	body, _ := comps[0].(map[string]any)
	params, _ := body["parameters"].([]any)
	if len(params) != 4 {
		t.Fatalf("body parameters = %d, want exactly 4", len(params))
	}
	first, _ := params[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "25 августа в 19:30" {
		t.Errorf("first parameter = %v", first)
	}
}

// A template name / language override must actually reach the wire: it is the
// whole reason the wording can change without a deploy.
func TestSenderTemplateOverride(t *testing.T) {
	srv, seen := fakeGraph(t, 200, `{"messages":[{"id":"wamid.X"}]}`)
	s := NewSender(SenderConfig{
		Config:       Config{AccessToken: testToken, PhoneNumberID: "1", BaseURL: srv.URL},
		TemplateName: "bookeat_venue_new_booking_ru_v2",
		TemplateLang: "kk",
	})
	if _, err := s.Send(context.Background(), "+77010000001", []string{"a", "b", "c", "d"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	tpl, _ := (*seen)[0].body["template"].(map[string]any)
	if tpl["name"] != "bookeat_venue_new_booking_ru_v2" {
		t.Errorf("template = %v", tpl["name"])
	}
	lang, _ := tpl["language"].(map[string]any)
	if lang["code"] != "kk" {
		t.Errorf("language = %v", lang["code"])
	}
}

// The status is what the notifier classifies retry-vs-give-up on, so it must
// come back even when the send failed. And Meta's prose, our token and the
// recipient must never appear in the error.
func TestSenderReportsStatusAndScrubs(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantMsg    string
	}{
		{name: "no whatsapp account", status: 400,
			body:       `{"error":{"code":131026,"error_subcode":0,"message":"Message undeliverable"}}`,
			wantStatus: 400, wantMsg: "code 131026"},
		{name: "expired token", status: 401,
			body:       `{"error":{"code":190,"error_subcode":463,"message":"Session has expired"}}`,
			wantStatus: 401, wantMsg: "code 190"},
		{name: "proxy error page", status: 502, body: `<html>nope</html>`,
			wantStatus: 502, wantMsg: "unreadable response"},
		{name: "200 without a message id", status: 200, body: `{"messages":[]}`,
			wantStatus: 200, wantMsg: "rejected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeGraph(t, tc.status, tc.body)
			status, err := testSender(t, srv).Send(context.Background(), "+77010000001",
				[]string{"a", "b", "c", "d"})
			if err == nil {
				t.Fatal("expected an error")
			}
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d — the notifier classifies on it", status, tc.wantStatus)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantMsg)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("the access token reached the error: %q", err)
			}
			for _, prose := range []string{"Message undeliverable", "Session has expired"} {
				if strings.Contains(err.Error(), prose) {
					t.Fatalf("Meta's free text reached our error: %q", err)
				}
			}
		})
	}
}

// A transport failure has no status; the notifier reads that as transient.
func TestSenderTransportFailureHasNoStatus(t *testing.T) {
	srv, _ := fakeGraph(t, 200, `{}`)
	srv.Close() // nothing is listening any more
	status, err := testSender(t, srv).Send(context.Background(), "+77010000001", []string{"a"})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if status != 0 {
		t.Errorf("status = %d, want 0 for a transport failure", status)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("the access token reached the error: %q", err)
	}
}

// Meta rejects a parameter containing a newline, a tab or four-plus consecutive
// spaces, and rejects an empty one outright. A guest-typed name must not be able
// to kill the venue's alert.
func TestSanitizeParams(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "Дамир", want: "Дамир"},
		{in: "Да\nмир", want: "Да мир"},
		{in: "Да\tмир", want: "Да мир"},
		{in: "Дамир    Саркулин", want: "Дамир Саркулин"},
		{in: "  Дамир  ", want: "Дамир"},
		{in: "", want: "—"},
		{in: "   ", want: "—"},
		{in: strings.Repeat("я", 200), want: strings.Repeat("я", maxParamLen)},
	}
	for _, tc := range tests {
		got := SanitizeParams([]string{tc.in})[0]
		if got != tc.want {
			t.Errorf("SanitizeParams(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Sanitization must happen on the way out, not be left to the caller.
func TestSenderSanitizesOnTheWire(t *testing.T) {
	srv, seen := fakeGraph(t, 200, `{"messages":[{"id":"wamid.X"}]}`)
	if _, err := testSender(t, srv).Send(context.Background(), "+77010000001",
		[]string{"25 августа в 19:30", "4", "Да\nмир", ""}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	tpl, _ := (*seen)[0].body["template"].(map[string]any)
	comps, _ := tpl["components"].([]any)
	body, _ := comps[0].(map[string]any)
	params, _ := body["parameters"].([]any)
	third, _ := params[2].(map[string]any)
	if third["text"] != "Да мир" {
		t.Errorf("newline was not sanitized: %v", third["text"])
	}
	fourth, _ := params[3].(map[string]any)
	if fourth["text"] != "—" {
		t.Errorf("empty parameter was not replaced: %v", fourth["text"])
	}
}

func TestConfigured(t *testing.T) {
	tests := map[string]struct {
		cfg  Config
		want bool
	}{
		"complete":         {cfg: Config{AccessToken: "t", PhoneNumberID: "1"}, want: true},
		"no token":         {cfg: Config{PhoneNumberID: "1"}},
		"no phone id":      {cfg: Config{AccessToken: "t"}},
		"whitespace token": {cfg: Config{AccessToken: "  ", PhoneNumberID: "1"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tc.cfg.Configured(); got != tc.want {
				t.Fatalf("Configured() = %v, want %v", got, tc.want)
			}
		})
	}
}
