package otpsender

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TwilioConfig configures the Twilio SMS provider.
//
// Where the values come from (console.twilio.com):
//   - AccountSID / AuthToken — Account Info on the console dashboard. The auth
//     token is a credential: env only, never logged, never in an error.
//   - From — a Twilio phone number you own, in E.164 ("+1555…"), OR leave it
//     empty and set MessagingServiceSID instead. Exactly one of the two.
//   - MessagingServiceSID — Messaging → Services ("MG…"). Preferred for
//     production: it owns sender selection, opt-outs and per-country routing,
//     which a bare From number does not.
type TwilioConfig struct {
	AccountSID          string
	AuthToken           string
	From                string
	MessagingServiceSID string
	Timeout             time.Duration
	// BaseURL overrides the Twilio host. Tests only.
	BaseURL string
}

// Configured reports whether Twilio can be built: credentials plus exactly one
// sender (a From number or a messaging service).
func (c TwilioConfig) Configured() bool {
	hasSender := trimmed(c.From) != "" || trimmed(c.MessagingServiceSID) != ""
	return trimmed(c.AccountSID) != "" && trimmed(c.AuthToken) != "" && hasSender
}

const twilioBaseURL = "https://api.twilio.com"

// Twilio sends SMS through the Programmable Messaging REST API.
type Twilio struct {
	cfg  TwilioConfig
	base string
	http *httpClient
}

// NewTwilio builds the provider. Build it only when cfg.Configured().
func NewTwilio(cfg TwilioConfig) *Twilio {
	base := trimmed(cfg.BaseURL)
	if base == "" {
		base = twilioBaseURL
	}
	return &Twilio{cfg: cfg, base: base, http: newHTTPClient(cfg.Timeout, cfg.AuthToken)}
}

var _ SMSProvider = (*Twilio)(nil)

func (t *Twilio) ProviderName() string { return "twilio" }

type twilioResponse struct {
	SID       string `json:"sid"`
	ErrorCode int    `json:"error_code"`
}

// SendSMS posts to /2010-04-01/Accounts/{sid}/Messages.json.
//
// Twilio replies 201 with a message resource whose `status` is "queued" or
// "accepted" — like WhatsApp, that is an acceptance and not a delivery. The
// system tolerates that the same way (the channel is remembered only after a
// verified login).
func (t *Twilio) SendSMS(ctx context.Context, phone, text string) (string, error) {
	form := url.Values{}
	form.Set("To", phone) // Twilio wants E.164 WITH the leading "+".
	form.Set("Body", text)
	if sid := trimmed(t.cfg.MessagingServiceSID); sid != "" {
		// A messaging service and a From number are mutually exclusive; the
		// service wins because it is the production-grade sender.
		form.Set("MessagingServiceSid", sid)
	} else {
		form.Set("From", trimmed(t.cfg.From))
	}

	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json", t.base, trimmed(t.cfg.AccountSID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("twilio: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Basic auth over the header rather than in the URL: a token in the URL ends
	// up inside any *url.Error, which is exactly the leak httpClient.scrub then
	// has to clean up.
	req.SetBasicAuth(trimmed(t.cfg.AccountSID), trimmed(t.cfg.AuthToken))

	status, body, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("twilio: send: %w", err)
	}
	var parsed twilioResponse
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr != nil {
		return "", fmt.Errorf("twilio: unreadable response (http %d)", status)
	}
	if status >= 200 && status <= 299 && parsed.SID != "" && parsed.ErrorCode == 0 {
		return parsed.SID, nil
	}
	// Reported by code only — Twilio's `message` is prose that can quote the
	// request, and the request contains the OTP.
	// TODO(verify): проверить на боевом аккаунте — какие error_code Twilio
	// возвращает для "не мобильный / не существует" (ожидаем 21211/21614), чтобы
	// мапить их в ErrChannelUnavailable, а не в общий сбой.
	return "", fmt.Errorf("twilio: rejected (http %d, code %d)", status, parsed.ErrorCode)
}
