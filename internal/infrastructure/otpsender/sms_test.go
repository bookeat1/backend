package otpsender

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"backend-core/internal/domain"
)

const (
	testTwilioSID   = "ACfake0000000000000000000000000000"
	testTwilioToken = "fake-twilio-auth-token" //nolint:gosec // test-only literal
	testMobizonKey  = "fake-mobizon-key"       //nolint:gosec // test-only literal
)

// The SMS channel is the only one whose message text we write ourselves, and a
// text that grows past one Cyrillic segment (70 chars) silently doubles the
// bill. The wording is therefore pinned.
func TestSMSMessageText(t *testing.T) {
	var got string
	sms := NewSMS(fakeSMSProvider{capture: func(_, text string) { got = text }}, SMSConfig{CodeTTL: 5 * time.Minute})

	if sms.Name() != domain.OTPChannelSMS {
		t.Fatalf("Name() = %q, want %q", sms.Name(), domain.OTPChannelSMS)
	}
	if _, err := sms.Send(context.Background(), testPhone, testCode); err != nil {
		t.Fatalf("Send: %v", err)
	}
	want := "Ваш код BookEat: 482913. Действует 5 мин."
	if got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
	if n := len([]rune(got)); n > 70 {
		t.Fatalf("text is %d characters — over one Cyrillic SMS segment, every send now costs double", n)
	}
}

type fakeSMSProvider struct {
	capture   func(phone, text string)
	messageID string
	err       error
}

func (fakeSMSProvider) ProviderName() string { return "fake" }

func (f fakeSMSProvider) SendSMS(_ context.Context, phone, text string) (string, error) {
	if f.capture != nil {
		f.capture(phone, text)
	}
	return f.messageID, f.err
}

func TestTwilioRequestShape(t *testing.T) {
	tests := map[string]struct {
		cfg       TwilioConfig
		wantField string
		wantValue string
		absent    string
	}{
		"messaging service wins when both are set": {
			cfg:       TwilioConfig{From: "+15551112222", MessagingServiceSID: "MGfake"},
			wantField: "MessagingServiceSid",
			wantValue: "MGfake",
			absent:    "From",
		},
		"from number when there is no messaging service": {
			cfg:       TwilioConfig{From: "+15551112222"},
			wantField: "From",
			wantValue: "+15551112222",
			absent:    "MessagingServiceSid",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv, captured := fakeProvider(t, map[string]func(http.ResponseWriter){
				"*": jsonResponse(201, `{"sid":"SMfake","status":"queued"}`),
			})
			cfg := tc.cfg
			cfg.AccountSID, cfg.AuthToken, cfg.BaseURL = testTwilioSID, testTwilioToken, srv.URL
			if !cfg.Configured() {
				t.Fatal("test config should be Configured()")
			}

			if _, err := NewTwilio(cfg).SendSMS(context.Background(), testPhone, "Ваш код BookEat: "+testCode); err != nil {
				t.Fatalf("SendSMS: %v", err)
			}

			req := (*captured)[0]
			if want := "/2010-04-01/Accounts/" + testTwilioSID + "/Messages.json"; req.path != want {
				t.Errorf("path = %s, want %s", req.path, want)
			}
			// Basic auth in the header, never in the URL: a credential in a URL
			// ends up inside every *url.Error and every proxy log.
			wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(testTwilioSID+":"+testTwilioToken))
			if req.auth != wantAuth {
				t.Errorf("Authorization = %q, want basic auth", req.auth)
			}
			if got := req.form["To"]; len(got) != 1 || got[0] != testPhone {
				t.Errorf("To = %v, want %s (E.164 with the plus)", got, testPhone)
			}
			if got := req.form[tc.wantField]; len(got) != 1 || got[0] != tc.wantValue {
				t.Errorf("%s = %v, want %s", tc.wantField, got, tc.wantValue)
			}
			if got := req.form[tc.absent]; len(got) != 0 {
				t.Errorf("%s must be absent, got %v", tc.absent, got)
			}
		})
	}
}

func TestTwilioOutcomes(t *testing.T) {
	tests := map[string]struct {
		status  int
		body    string
		wantErr bool
	}{
		"queued":            {status: 201, body: `{"sid":"SMfake","status":"queued"}`, wantErr: false},
		"no sid":            {status: 201, body: `{"status":"failed"}`, wantErr: true},
		"error code in 201": {status: 201, body: `{"sid":"SMfake","error_code":21211}`, wantErr: true},
		"unauthorized":      {status: 401, body: `{"code":20003,"message":"Authenticate"}`, wantErr: true},
		"html":              {status: 502, body: `<html>no</html>`, wantErr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv, _ := fakeProvider(t, map[string]func(http.ResponseWriter){"*": jsonResponse(tc.status, tc.body)})
			_, err := NewTwilio(TwilioConfig{
				AccountSID: testTwilioSID, AuthToken: testTwilioToken, From: "+15551112222", BaseURL: srv.URL,
			}).SendSMS(context.Background(), testPhone, "Ваш код BookEat: "+testCode)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			assertNoSecrets(t, err, testTwilioToken)
		})
	}
}

func TestMobizonRequestShape(t *testing.T) {
	srv, captured := fakeProvider(t, map[string]func(http.ResponseWriter){
		"*": jsonResponse(200, `{"code":0,"data":{},"message":""}`),
	})
	cfg := MobizonConfig{APIKey: testMobizonKey, Sender: "BookEat", BaseURL: srv.URL}
	if !cfg.Configured() {
		t.Fatal("test config should be Configured()")
	}

	if _, err := NewMobizon(cfg).SendSMS(context.Background(), testPhone, "Ваш код BookEat: "+testCode); err != nil {
		t.Fatalf("SendSMS: %v", err)
	}

	req := (*captured)[0]
	if req.path != "/service/message/sendsmsmessage" {
		t.Errorf("path = %s", req.path)
	}
	// The api key must be in the BODY, not the query string: a key in a URL is
	// written to every proxy log between us and Mobizon.
	if got := req.form["apiKey"]; len(got) != 1 || got[0] != testMobizonKey {
		t.Errorf("apiKey missing from the form body: %v", req.form)
	}
	if got := req.form["recipient"]; len(got) != 1 || got[0] != strings.TrimPrefix(testPhone, "+") {
		t.Errorf("recipient = %v, want digits only", got)
	}
	if got := req.form["from"]; len(got) != 1 || got[0] != "BookEat" {
		t.Errorf("from = %v, want the approved sender name", got)
	}
}

func TestMobizonOmitsSenderWhenUnset(t *testing.T) {
	srv, captured := fakeProvider(t, map[string]func(http.ResponseWriter){
		"*": jsonResponse(200, `{"code":100}`),
	})
	if _, err := NewMobizon(MobizonConfig{APIKey: testMobizonKey, BaseURL: srv.URL}).
		SendSMS(context.Background(), testPhone, "x"); err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	// An UNapproved sender name is rejected per message, so "no name" must mean
	// "no field", not an empty one.
	if got := (*captured)[0].form["from"]; len(got) != 0 {
		t.Fatalf("from = %v, want the field omitted entirely", got)
	}
}

func TestMobizonOutcomes(t *testing.T) {
	tests := map[string]struct {
		status  int
		body    string
		wantErr bool
	}{
		"sent":            {status: 200, body: `{"code":0}`},
		"queued":          {status: 200, body: `{"code":100}`},
		"api error":       {status: 200, body: `{"code":1,"message":"bad key"}`, wantErr: true},
		"http error":      {status: 500, body: `{"code":0}`, wantErr: true},
		"unparseable":     {status: 200, body: `nope`, wantErr: true},
		"empty api error": {status: 200, body: `{"code":13}`, wantErr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv, _ := fakeProvider(t, map[string]func(http.ResponseWriter){"*": jsonResponse(tc.status, tc.body)})
			_, err := NewMobizon(MobizonConfig{APIKey: testMobizonKey, BaseURL: srv.URL}).
				SendSMS(context.Background(), testPhone, "Ваш код BookEat: "+testCode)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			assertNoSecrets(t, err, testMobizonKey)
		})
	}
}

func TestSMSConfiguredGuards(t *testing.T) {
	if (TwilioConfig{AccountSID: "AC", AuthToken: "t"}).Configured() {
		t.Error("twilio without any sender must not count as configured")
	}
	if (TwilioConfig{AccountSID: "AC", From: "+1"}).Configured() {
		t.Error("twilio without an auth token must not count as configured")
	}
	if (MobizonConfig{}).Configured() {
		t.Error("mobizon without an api key must not count as configured")
	}
	if !(MobizonConfig{APIKey: "k"}).Configured() {
		t.Error("mobizon with an api key is configured (the sender name is optional)")
	}
}
