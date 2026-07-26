package otpsender

import (
	"context"
	"fmt"
	"strings"
	"time"

	"backend-core/internal/domain"
)

// SMSProvider is the swappable half of the SMS channel: the part that actually
// hands a text to a carrier. The waterfall never sees it — it sees one "sms"
// channel — so swapping Twilio for Mobizon (or adding a third) is a config
// change plus one small adapter, and nothing above this line moves.
type SMSProvider interface {
	// ProviderName is for logs and startup diagnostics only. It must NEVER be
	// the channel name stored on an otp_codes row: the guest's remembered
	// channel is "sms", so switching providers must not orphan every memory.
	ProviderName() string
	// SendSMS delivers text to phone (E.164). A nil error means the provider
	// accepted it; the returned string is the provider's message id (Twilio's
	// SID, Mobizon's messageId), empty when the provider gives none.
	SendSMS(ctx context.Context, phone, text string) (string, error)
}

// SMSConfig configures the SMS channel itself (provider selection lives in
// bootstrap; this is the part the channel needs).
type SMSConfig struct {
	// CodeTTL is used to render "действует N мин." in the message body.
	CodeTTL time.Duration
}

// SMS is the last-resort channel: it reaches any working phone, it is the most
// expensive, and it is the only one that cannot be pre-checked or verified
// asynchronously. It is last in the waterfall for exactly those reasons.
type SMS struct {
	provider SMSProvider
	ttl      time.Duration
}

// NewSMS builds the SMS channel over a provider. Build it only when a provider
// is configured.
func NewSMS(provider SMSProvider, cfg SMSConfig) *SMS {
	ttl := cfg.CodeTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &SMS{provider: provider, ttl: ttl}
}

var _ Channel = (*SMS)(nil)

func (s *SMS) Name() string { return domain.OTPChannelSMS }

// ProviderName exposes which provider is behind the channel (startup log).
func (s *SMS) ProviderName() string { return s.provider.ProviderName() }

// Send renders the message and hands it to the provider.
//
// The wording is the old system's, kept deliberately: it is short (one SMS
// segment matters for cost), it names the brand so the guest knows what they are
// confirming, and it says how long the code lives. Cyrillic text makes the
// segment 70 chars, so the template stays under that.
func (s *SMS) Send(ctx context.Context, phone, code string) (string, error) {
	minutes := int(s.ttl.Minutes())
	if minutes < 1 {
		minutes = 1
	}
	text := fmt.Sprintf("Ваш код BookEat: %s. Действует %d мин.", code, minutes)
	return s.provider.SendSMS(ctx, phone, text)
}

// smsDigits strips the "+" for providers that want a bare national/international
// number. Kept here so both provider adapters agree on the shape.
func smsDigits(phone string) string { return strings.TrimPrefix(phone, "+") }
