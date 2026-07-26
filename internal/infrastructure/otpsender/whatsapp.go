package otpsender

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"backend-core/internal/domain"
)

// WhatsAppConfig configures the Meta WhatsApp Cloud API channel.
//
// Where the values come from (developers.facebook.com → your app → WhatsApp):
//   - AccessToken     — a System User token with whatsapp_business_messaging.
//     Use a SYSTEM USER token, not a temporary 24-hour test token.
//   - PhoneNumberID   — WhatsApp → API Setup → "Phone number ID" (a number id,
//     not the phone number itself).
//   - TemplateName    — an APPROVED template in the AUTHENTICATION category.
//     Meta does not allow free-form text for a login code.
type WhatsAppConfig struct {
	AccessToken   string
	PhoneNumberID string
	// TemplateName is the approved authentication template. "bookeat_otp_en"
	// is the one verified against the live number on 2026-07-25.
	TemplateName string
	// TemplateLang is the template's language code as approved. For
	// "bookeat_otp_en" it is "en" — verified by a live send that was both
	// accepted and delivered (2026-07-25). A mismatch here is rejected by Meta
	// on every single send, so this is not a value to guess at.
	TemplateLang string
	// APIVersion pins the Graph API version. Meta deprecates versions on a
	// schedule, so this is config: bumping it must not need a deploy of new code.
	APIVersion string
	// CopyCodeButton must mirror the approved template. Meta's authentication
	// templates carry a "Copy code" URL button by default, and that button needs
	// its OWN parameter — sending the body parameter alone is rejected with
	// "number of parameters does not match". A template approved WITHOUT the
	// button is rejected the other way round, hence a switch and not a constant.
	CopyCodeButton bool
	Timeout        time.Duration
	// BaseURL overrides the Graph host. Tests only.
	BaseURL string
}

// Configured reports whether the channel can be built: a token, a phone number
// id and a template name are all required — two out of three is a
// misconfiguration that would fail on every send, so it counts as absent.
func (c WhatsAppConfig) Configured() bool {
	return trimmed(c.AccessToken) != "" && trimmed(c.PhoneNumberID) != "" && trimmed(c.TemplateName) != ""
}

const (
	whatsAppBaseURL = "https://graph.facebook.com"
	// The Graph version the live send was verified on (2026-07-25).
	whatsAppDefaultVersion = "v22.0"
	// "en" is the approved language of bookeat_otp_en, confirmed by a real
	// delivery rather than by inspection.
	whatsAppDefaultLang = "en"
	// whatsAppNotOnWhatsApp is Meta's error code for "this recipient cannot
	// receive the message" — in practice, no WhatsApp account on that number.
	whatsAppNotOnWhatsApp = 131026
)

// WhatsApp is the Meta Cloud API channel.
//
// # Optimistic by nature
//
// Unlike Telegram Gateway there is NO pre-check. Meta answers 200 with a message
// id for any syntactically valid number, including numbers with no WhatsApp
// account, and the real outcome arrives later on the delivery webhook (or never).
// So "accepted" is all this adapter can report, and the system is built so a
// silent failure is survivable rather than fatal:
//
//  1. The channel is only REMEMBERED for a phone after a code delivered over it
//     was actually verified (domain.OTPChannelPreference), never on acceptance.
//  2. The usecase deprioritizes a channel that already holds an unverified code
//     for that phone, so the guest's next attempt moves past WhatsApp to SMS
//     instead of hitting the same silence twice.
//
// That pair is the replacement for the old system's timer-based "wait 40 s, then
// send an SMS anyway": no background scheduler, no second charge for guests who
// simply took their time typing.
type WhatsApp struct {
	cfg  WhatsAppConfig
	base string
	http *httpClient
}

// NewWhatsApp builds the channel. Build it only when cfg.Configured().
func NewWhatsApp(cfg WhatsAppConfig) *WhatsApp {
	base := trimmed(cfg.BaseURL)
	if base == "" {
		base = whatsAppBaseURL
	}
	if trimmed(cfg.APIVersion) == "" {
		cfg.APIVersion = whatsAppDefaultVersion
	}
	if trimmed(cfg.TemplateLang) == "" {
		cfg.TemplateLang = whatsAppDefaultLang
	}
	return &WhatsApp{cfg: cfg, base: base, http: newHTTPClient(cfg.Timeout, cfg.AccessToken)}
}

var _ Channel = (*WhatsApp)(nil)

func (w *WhatsApp) Name() string { return domain.OTPChannelWhatsApp }

type whatsAppResponse struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	// Only the numeric codes are decoded. Meta's prose "message" is deliberately
	// NOT parsed: it would be free text from a third party landing in our logs,
	// and everything we act on is expressible as a code.
	Error struct {
		Code    int `json:"code"`
		Subcode int `json:"error_subcode"`
	} `json:"error"`
}

// Send posts the authentication template. A nil error means Meta ACCEPTED it and
// the returned string is the wamid — see the type comment for why acceptance is
// not delivery, and why the wamid is worth logging anyway (it is what a delivery
// webhook and Meta's own dashboard key on).
func (w *WhatsApp) Send(ctx context.Context, phone, code string) (string, error) {
	// Meta wants the number WITHOUT the leading "+", digits only.
	to := strings.TrimPrefix(phone, "+")

	components := []map[string]any{{
		"type":       "body",
		"parameters": []map[string]any{{"type": "text", "text": code}},
	}}
	if w.cfg.CopyCodeButton {
		// The button's URL embeds {{1}}; the parameter is what WhatsApp copies
		// to the clipboard when the guest taps "Copy code". It is never rendered
		// as text, but it is required whenever the template has the button.
		components = append(components, map[string]any{
			"type":       "button",
			"sub_type":   "url",
			"index":      0,
			"parameters": []map[string]any{{"type": "text", "text": code}},
		})
	}

	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "template",
		"template": map[string]any{
			"name":       trimmed(w.cfg.TemplateName),
			"language":   map[string]any{"code": trimmed(w.cfg.TemplateLang)},
			"components": components,
		},
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("whatsapp: marshal request: %w", err)
	}
	url := fmt.Sprintf("%s/%s/%s/messages", w.base, trimmed(w.cfg.APIVersion), trimmed(w.cfg.PhoneNumberID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("whatsapp: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+trimmed(w.cfg.AccessToken))

	status, body, err := w.http.do(req)
	if err != nil {
		return "", fmt.Errorf("whatsapp: send: %w", err)
	}

	var parsed whatsAppResponse
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr != nil {
		return "", fmt.Errorf("whatsapp: unreadable response (http %d)", status)
	}
	if status >= 200 && status <= 299 && len(parsed.Messages) > 0 && parsed.Messages[0].ID != "" {
		return parsed.Messages[0].ID, nil
	}
	if parsed.Error.Code == whatsAppNotOnWhatsApp {
		return "", fmt.Errorf("%w: recipient has no whatsapp (131026)", ErrChannelUnavailable)
	}
	// The error is reported by CODE, not by Meta's prose message: the prose is
	// attacker-influenced free text that ends up in our logs, and (unlike the
	// code) it is not a stable thing to alert on.
	return "", fmt.Errorf("whatsapp: rejected (http %d, code %d, subcode %d)", status, parsed.Error.Code, parsed.Error.Subcode)
}
