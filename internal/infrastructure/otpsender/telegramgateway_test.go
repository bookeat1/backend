package otpsender

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// capturedRequest is what the fake provider server records for assertions on
// the request SHAPE — the part we cannot verify against the real API without
// tokens, and the part that breaks silently when someone edits a payload.
type capturedRequest struct {
	path   string
	auth   string
	header http.Header
	body   map[string]any
	form   map[string][]string
}

// fakeProvider serves scripted responses per path and records every request.
func fakeProvider(t *testing.T, responses map[string]func(w http.ResponseWriter)) (*httptest.Server, *[]capturedRequest) {
	t.Helper()
	var captured []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := capturedRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), header: r.Header.Clone()}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			_ = json.Unmarshal(raw, &rec.body)
		} else {
			if values, err := parseForm(string(raw)); err == nil {
				rec.form = values
			}
		}
		captured = append(captured, rec)

		key := r.URL.Path
		if fn, ok := responses[key]; ok {
			fn(w)
			return
		}
		if fn, ok := responses["*"]; ok {
			fn(w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

func parseForm(raw string) (map[string][]string, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	return values, nil
}

func jsonResponse(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

const testGatewayToken = "fake-gateway-token" //nolint:gosec // test-only literal, not a credential

func newTestGateway(t *testing.T, srv *httptest.Server) *TelegramGateway {
	t.Helper()
	cfg := TelegramGatewayConfig{
		Token:   testGatewayToken,
		CodeTTL: 5 * time.Minute,
		Timeout: 2 * time.Second,
		BaseURL: srv.URL,
	}
	if !cfg.Configured() {
		t.Fatal("test config should be Configured()")
	}
	return NewTelegramGateway(cfg)
}

// The pre-check is the reason this channel goes first: it is the only "will this
// fail?" answer we can get before spending anything. This test pins BOTH calls'
// shapes, including the request_id reuse that makes the pair cost one fee.
func TestTelegramGatewaySendRequestShape(t *testing.T) {
	srv, captured := fakeProvider(t, map[string]func(http.ResponseWriter){
		"/checkSendAbility":        jsonResponse(200, `{"ok":true,"result":{"request_id":"req-42"}}`),
		"/sendVerificationMessage": jsonResponse(200, `{"ok":true,"result":{"request_id":"req-42"}}`),
	})

	messageID, err := newTestGateway(t, srv).Send(context.Background(), testPhone, testCode)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// The request_id is the handle Telegram knows this delivery by; it is what
	// the waterfall logs so a "the guest got nothing" report can be traced.
	if messageID != "req-42" {
		t.Errorf("message id = %q, want the request_id %q", messageID, "req-42")
	}

	reqs := *captured
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2 (pre-check then send)", len(reqs))
	}
	if reqs[0].path != "/checkSendAbility" {
		t.Errorf("first call = %s, want /checkSendAbility (never send without the free pre-check)", reqs[0].path)
	}
	if reqs[0].body["phone_number"] != testPhone {
		t.Errorf("pre-check phone_number = %v, want %s", reqs[0].body["phone_number"], testPhone)
	}
	if reqs[0].auth != "Bearer "+testGatewayToken {
		t.Errorf("pre-check Authorization = %q, want a bearer token", reqs[0].auth)
	}

	send := reqs[1]
	if send.path != "/sendVerificationMessage" {
		t.Errorf("second call = %s, want /sendVerificationMessage", send.path)
	}
	if send.body["request_id"] != "req-42" {
		t.Errorf("request_id = %v, want req-42 reused from the pre-check (otherwise Telegram charges twice)", send.body["request_id"])
	}
	if send.body["code"] != testCode {
		t.Errorf("code = %v, want our own code (a Telegram-generated one is unverifiable on our side)", send.body["code"])
	}
	if ttl, _ := send.body["ttl"].(float64); ttl != 300 {
		t.Errorf("ttl = %v, want 300 seconds", send.body["ttl"])
	}
	if _, ok := send.body["sender_username"]; ok {
		t.Errorf("sender_username must be omitted when no verified channel is configured, got %v", send.body["sender_username"])
	}
}

func TestTelegramGatewayTTLClamped(t *testing.T) {
	tests := map[string]struct {
		ttl  time.Duration
		want float64
	}{
		"below the API minimum": {ttl: 5 * time.Second, want: 30},
		"above the API maximum": {ttl: 3 * time.Hour, want: 3600},
		"inside the window":     {ttl: 10 * time.Minute, want: 600},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv, captured := fakeProvider(t, map[string]func(http.ResponseWriter){
				"*": jsonResponse(200, `{"ok":true,"result":{"request_id":"r"}}`),
			})
			gw := NewTelegramGateway(TelegramGatewayConfig{Token: testGatewayToken, CodeTTL: tc.ttl, BaseURL: srv.URL})
			if _, err := gw.Send(context.Background(), testPhone, testCode); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if got := (*captured)[1].body["ttl"]; got != tc.want {
				t.Errorf("ttl = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTelegramGatewayOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		responses  map[string]func(http.ResponseWriter)
		wantErr    error
		wantSends  int // how many times /sendVerificationMessage was called
		wantErrMsg string
	}{
		{
			name: "number is not on telegram — a definite, cheap no",
			responses: map[string]func(http.ResponseWriter){
				"/checkSendAbility": jsonResponse(200, `{"ok":false,"error":"PHONE_NUMBER_NOT_TELEGRAM"}`),
			},
			wantErr:   ErrChannelUnavailable,
			wantSends: 0,
		},
		{
			// Observed live on 2026-07-25: the account balance can be 0 while a
			// request costs 0. An empty prepaid balance is a routing fact, not
			// an incident — it must fall through to WhatsApp, never 500.
			name: "out of balance — a fallback, not a failure",
			responses: map[string]func(http.ResponseWriter){
				"/checkSendAbility": jsonResponse(200, `{"ok":false,"error":"BALANCE_NOT_ENOUGH"}`),
			},
			wantErr:    ErrChannelUnavailable,
			wantSends:  0,
			wantErrMsg: "BALANCE_NOT_ENOUGH",
		},
		{
			name: "out of balance discovered only at send time",
			responses: map[string]func(http.ResponseWriter){
				"/checkSendAbility":        jsonResponse(200, `{"ok":true,"result":{"request_id":"r"}}`),
				"/sendVerificationMessage": jsonResponse(200, `{"ok":false,"error":"BALANCE_NOT_ENOUGH"}`),
			},
			wantErr:   ErrChannelUnavailable,
			wantSends: 1,
		},
		{
			name: "pre-check rejected for a reason we do not model — still a fallback",
			responses: map[string]func(http.ResponseWriter){
				"/checkSendAbility": jsonResponse(200, `{"ok":false,"error":"ACCESS_TOKEN_INVALID"}`),
			},
			wantSends:  0,
			wantErrMsg: "ACCESS_TOKEN_INVALID",
		},
		{
			name: "send rejected after a good pre-check",
			responses: map[string]func(http.ResponseWriter){
				"/checkSendAbility":        jsonResponse(200, `{"ok":true,"result":{"request_id":"r"}}`),
				"/sendVerificationMessage": jsonResponse(200, `{"ok":false,"error":"CODE_INVALID"}`),
			},
			wantSends:  1,
			wantErrMsg: "CODE_INVALID",
		},
		{
			name: "provider answers html — status only, no body echo",
			responses: map[string]func(http.ResponseWriter){
				"/checkSendAbility": func(w http.ResponseWriter) {
					w.WriteHeader(502)
					_, _ = io.WriteString(w, "<html>bad gateway</html>")
				},
			},
			wantSends:  0,
			wantErrMsg: "unreadable response",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, captured := fakeProvider(t, tc.responses)
			_, err := newTestGateway(t, srv).Send(context.Background(), testPhone, testCode)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErrMsg != "" && !strings.Contains(err.Error(), tc.wantErrMsg) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErrMsg)
			}
			sends := 0
			for _, r := range *captured {
				if r.path == "/sendVerificationMessage" {
					sends++
				}
			}
			if sends != tc.wantSends {
				t.Fatalf("sends = %d, want %d", sends, tc.wantSends)
			}
			// Whatever went wrong, neither credential nor code may be in it.
			assertNoSecrets(t, err, testGatewayToken)
		})
	}
}

// assertNoSecrets is the shared security assertion for every provider adapter:
// an error is a thing that gets logged, so it may never carry the OTP or a
// provider credential.
func assertNoSecrets(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	if strings.Contains(msg, testCode) {
		t.Fatalf("the OTP code leaked into an error: %q", msg)
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(msg, s) {
			t.Fatalf("a credential leaked into an error: %q", msg)
		}
	}
}
