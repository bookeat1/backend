package otpsender

import (
	"context"
	"fmt"
	"time"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/whatsapp"
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
	return c.transport().Configured() && trimmed(c.TemplateName) != ""
}

func (c WhatsAppConfig) transport() whatsapp.Config {
	return whatsapp.Config{
		AccessToken:   c.AccessToken,
		PhoneNumberID: c.PhoneNumberID,
		APIVersion:    c.APIVersion,
		Timeout:       c.Timeout,
		BaseURL:       c.BaseURL,
	}
}

// whatsAppDefaultLang is the approved language of bookeat_otp_en, confirmed by
// a real delivery rather than by inspection.
const whatsAppDefaultLang = "en"

// WhatsApp is the Meta Cloud API login-code channel. The HTTP call itself lives
// in internal/infrastructure/whatsapp, shared with the venue booking-alert
// channel; what stays here is everything specific to a LOGIN CODE.
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
	cfg    WhatsAppConfig
	client *whatsapp.Client
}

// NewWhatsApp builds the channel. Build it only when cfg.Configured().
func NewWhatsApp(cfg WhatsAppConfig) *WhatsApp {
	if trimmed(cfg.TemplateLang) == "" {
		cfg.TemplateLang = whatsAppDefaultLang
	}
	return &WhatsApp{cfg: cfg, client: whatsapp.NewClient(cfg.transport())}
}

var _ Channel = (*WhatsApp)(nil)

func (w *WhatsApp) Name() string { return domain.OTPChannelWhatsApp }

// Send posts the authentication template. A nil error means Meta ACCEPTED it and
// the returned string is the wamid — see the type comment for why acceptance is
// not delivery, and why the wamid is worth logging anyway (it is what a delivery
// webhook and Meta's own dashboard key on).
func (w *WhatsApp) Send(ctx context.Context, phone, code string) (string, error) {
	tpl := whatsapp.Template{
		To:         phone,
		Name:       trimmed(w.cfg.TemplateName),
		Lang:       trimmed(w.cfg.TemplateLang),
		BodyParams: []string{code},
	}
	if w.cfg.CopyCodeButton {
		// The button's URL embeds {{1}}; the parameter is what WhatsApp copies
		// to the clipboard when the guest taps "Copy code". It is never rendered
		// as text, but it is required whenever the template has the button.
		tpl.ButtonURLParam = code
	}

	res, err := w.client.Send(ctx, tpl)
	if err != nil {
		// "No WhatsApp account on that number" is not a fault of ours: it means
		// this channel cannot serve this guest, and the waterfall must fall
		// through to the next one instead of reporting a failure.
		if res.ErrorCode == whatsapp.ErrCodeNotOnWhatsApp {
			return "", fmt.Errorf("%w: recipient has no whatsapp (131026)", ErrChannelUnavailable)
		}
		return "", err
	}
	return res.MessageID, nil
}
