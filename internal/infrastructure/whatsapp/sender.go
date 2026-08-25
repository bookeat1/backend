package whatsapp

import (
	"context"
	"strings"

	"backend-core/internal/infrastructure/scrubhttp"
)

// DefaultBookingTemplateName is the APPROVED venue-alert template
// (Meta id 1046784127953752, language ru, category UTILITY, APPROVED
// 2026-08-24). Its BODY is:
//
//	«Новая бронь в вашем заведении на {{1}}. Гостей {{2}}, имя гостя {{3}},
//	 телефон {{4}}. Подтвердите или отклоните бронь в панели заведения BookEat.»
//
// so it takes EXACTLY four parameters, in that order: when, how many, who,
// which phone. It is a default and not a constant because Meta requires a NEW
// template (a new name) for any wording change — being able to point the sender
// at the successor without a deploy is the whole reason it is configurable.
const DefaultBookingTemplateName = "bookeat_venue_new_booking_ru"

// DefaultBookingTemplateLang is that template's approved language code. A
// mismatch is rejected by Meta on every single send.
const DefaultBookingTemplateLang = "ru"

// SenderConfig is a Client plus the one template this sender is dedicated to.
type SenderConfig struct {
	Config
	// TemplateName / TemplateLang name the approved template. Empty falls back
	// to the booking-alert defaults above.
	TemplateName string
	TemplateLang string
}

// Configured reports whether the sender can send: credentials plus a template.
func (c SenderConfig) Configured() bool { return c.Config.Configured() }

// Sender delivers ONE approved template to a phone number and reports Meta's
// HTTP status. It is the infrastructure half of the venue WhatsApp notification
// channel; the notifier (usecase/notifications) holds the fan-out, dedupe and
// tenant scoping and never sees an HTTP request.
//
// The status is what the caller classifies on, exactly like telegramnotify:
// 2xx = accepted, 4xx = this send can never succeed as sent (bad number, no
// WhatsApp account, wrong parameters, dead token) and must NOT be retried
// forever, 429/5xx/0 = transient.
type Sender struct {
	client *Client
	name   string
	lang   string
}

// NewSender builds the sender. Build it only when cfg.Configured().
func NewSender(cfg SenderConfig) *Sender {
	name := scrubhttp.Trimmed(cfg.TemplateName)
	if name == "" {
		name = DefaultBookingTemplateName
	}
	lang := scrubhttp.Trimmed(cfg.TemplateLang)
	if lang == "" {
		lang = DefaultBookingTemplateLang
	}
	return &Sender{client: NewClient(cfg.Config), name: name, lang: lang}
}

// Send posts the template to phone with params filling {{1}}…{{n}}.
func (s *Sender) Send(ctx context.Context, phone string, params []string) (int, error) {
	res, err := s.client.Send(ctx, Template{
		To:         phone,
		Name:       s.name,
		Lang:       s.lang,
		BodyParams: SanitizeParams(params),
	})
	return res.Status, err
}

// maxParamLen bounds one template parameter. Meta's own limit is far higher,
// but a guest-supplied name is the only unbounded input here and a template
// parameter is not the place to find out how long it can be.
const maxParamLen = 120

// SanitizeParams makes values safe to put in a template parameter.
//
// This is not cosmetic: Meta REJECTS a parameter containing a newline, a tab or
// four-plus consecutive spaces with error 132000-family, and the rejection is
// per-message. A guest who types a line break into their name would otherwise
// silently kill the venue's alert for that booking (and, before the permanent-
// failure classification, keep the outbox event bouncing).
//
// An empty result is replaced with "—": Meta also rejects an EMPTY parameter,
// and a booking with no guest name still has to reach the venue.
func SanitizeParams(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.Map(func(r rune) rune {
			switch r {
			case '\n', '\r', '\t', '\v', '\f':
				return ' '
			}
			return r
		}, v)
		v = strings.Join(strings.Fields(v), " ")
		if utf8Len(v) > maxParamLen {
			v = truncateRunes(v, maxParamLen)
		}
		if v == "" {
			v = "—"
		}
		out = append(out, v)
	}
	return out
}

func utf8Len(s string) int { return len([]rune(s)) }

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n]))
}
