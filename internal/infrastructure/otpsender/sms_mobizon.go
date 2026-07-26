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

// MobizonConfig configures the Mobizon SMS provider — the one the OLD BookEat
// system actually delivers Kazakh OTPs with today.
//
// Where the values come from (mobizon.kz → Настройки → API):
//   - APIKey — the account's API key. A credential: env only, never logged.
//   - Sender — the approved alphanumeric sender name. OPTIONAL: without it
//     Mobizon sends from a shared service number, which works but looks
//     anonymous to the guest. An UNAPPROVED name is rejected per message, so
//     leave it empty until the name is approved.
type MobizonConfig struct {
	APIKey  string
	Sender  string
	Timeout time.Duration
	// BaseURL overrides the API host. Tests only.
	BaseURL string
}

// Configured reports whether Mobizon can be built.
func (c MobizonConfig) Configured() bool { return trimmed(c.APIKey) != "" }

const mobizonBaseURL = "https://api.mobizon.kz"

// Mobizon sends SMS through api.mobizon.kz.
type Mobizon struct {
	cfg  MobizonConfig
	base string
	http *httpClient
}

// NewMobizon builds the provider. Build it only when cfg.Configured().
func NewMobizon(cfg MobizonConfig) *Mobizon {
	base := trimmed(cfg.BaseURL)
	if base == "" {
		base = mobizonBaseURL
	}
	return &Mobizon{cfg: cfg, base: base, http: newHTTPClient(cfg.Timeout, cfg.APIKey)}
}

var _ SMSProvider = (*Mobizon)(nil)

func (m *Mobizon) ProviderName() string { return "mobizon" }

type mobizonResponse struct {
	Code int `json:"code"`
	// Data carries the accepted message on success. Only the id is decoded: it
	// is the handle mobizon.kz reports delivery status under, and everything
	// else in there echoes our request (which contains the OTP).
	Data struct {
		MessageID json.Number `json:"messageId"`
	} `json:"data"`
}

// messageID renders Mobizon's messageId, which the API returns as a number in
// some responses and as a string in others.
func (r mobizonResponse) messageID() string { return r.Data.MessageID.String() }

// Mobizon's own result codes: 0 = sent, 100 = queued. Everything else is a
// rejection (bad key, unapproved sender, no balance, bad recipient).
const (
	mobizonCodeSent   = 0
	mobizonCodeQueued = 100
)

// SendSMS posts the form-encoded sendsmsmessage call.
//
// The api key travels in the FORM BODY rather than the query string, unlike the
// old edge function's URL-encoded call: a key in a query string is written to
// every proxy and access log between us and Mobizon.
func (m *Mobizon) SendSMS(ctx context.Context, phone, text string) (string, error) {
	form := url.Values{}
	form.Set("apiKey", trimmed(m.cfg.APIKey))
	form.Set("recipient", smsDigits(phone)) // digits only, no "+"
	form.Set("text", text)
	form.Set("output", "json")
	if sender := trimmed(m.cfg.Sender); sender != "" {
		form.Set("from", sender)
	}

	endpoint := m.base + "/service/message/sendsmsmessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("mobizon: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	status, body, err := m.http.do(req)
	if err != nil {
		return "", fmt.Errorf("mobizon: send: %w", err)
	}
	var parsed mobizonResponse
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr != nil {
		return "", fmt.Errorf("mobizon: unreadable response (http %d)", status)
	}
	if status >= 200 && status <= 299 && (parsed.Code == mobizonCodeSent || parsed.Code == mobizonCodeQueued) {
		return parsed.messageID(), nil
	}
	// Mobizon's `message` field is prose that echoes the request; only the code
	// is logged. The request contains the OTP.
	return "", fmt.Errorf("mobizon: rejected (http %d, code %d)", status, parsed.Code)
}
